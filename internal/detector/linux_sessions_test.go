//go:build linux

package detector

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// statLine builds a /proc/<pid>/stat line with the fields layer 2 reads.
func statLine(pid uint32, comm, state string, ppid uint32, start uint64) string {
	// Fields 5-21 are filler; field 22 is starttime.
	filler := strings.Repeat("0 ", 17)
	return fmt.Sprintf("%d (%s) %s %d %s%d 0 0\n", pid, comm, state, ppid, filler, start)
}

// protonTree is Steam -> reaper SteamLaunch -> proton -> game.exe.
func protonTree() map[string]string {
	return map[string]string{
		procStatPath(10):     statLine(10, "steam", "S", 1, 100),
		procCmdlinePath(10):  "/home/u/.local/share/Steam/ubuntu12_32/steam\x00",
		procStatPath(90):     statLine(90, "reaper", "S", 10, 1000),
		procCmdlinePath(90):  "/home/u/.local/share/Steam/ubuntu12_32/reaper\x00SteamLaunch\x00AppId=1422450\x00--\x00/home/u/.local/share/Steam/ubuntu12_32/steam-launch-wrapper\x00--\x00proton\x00waitforexitandrun\x00",
		procStatPath(95):     statLine(95, "python3", "S", 90, 1010),
		procCmdlinePath(95):  "python3\x00/home/u/.local/share/Steam/steamapps/common/Proton - Experimental/proton\x00waitforexitandrun\x00",
		procStatPath(100):    statLine(100, "deadlock.exe", "S", 95, 1100),
		procCommPath(100):    "deadlock.exe\n",
		procCmdlinePath(100): `C:\Program Files (x86)\Steam\steamapps\common\Deadlock\game\bin\win64\deadlock.exe` + "\x00",
	}
}

func gameProc(pid uint32) *fakeGPU {
	return &fakeGPU{
		tel:   gpu.Telemetry{Name: "GPU"},
		procs: []gpu.ProcessInfo{{PID: pid, UsedVRAMMiB: 4096, SMUtilPct: 60, SMUtilValid: true}},
	}
}

func TestParseProcStat(t *testing.T) {
	st, err := parseProcStat([]byte(statLine(7, "weird ) (name", "Z", 3, 42)))
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if st.ppid != 3 || st.start != 42 || !st.zombie {
		t.Fatalf("stat = %+v, se esperaba ppid=3 start=42 zombie", st)
	}
	for _, bad := range []string{"", "7 (x) S", "7 x S 1 2 3", "7 (x) S notanumber " + strings.Repeat("0 ", 20)} {
		if _, err := parseProcStat([]byte(bad)); err == nil {
			t.Errorf("parseProcStat(%q) no devolvió error", bad)
		}
	}
}

// TestProtonGameBelongsToSteamSession: the .exe is tied to the reaper launch.
func TestProtonGameBelongsToSteamSession(t *testing.T) {
	c := collectorWith(gameProc(100), protonTree())
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	key := snap.GPUProcessSessions[100]
	if !strings.HasPrefix(key, "steam:1422450/90/") {
		t.Fatalf("sesión = %q, se esperaba la del reaper 90 con AppId 1422450", key)
	}
	if len(snap.LiveGameSessions) != 1 || snap.LiveGameSessions[0] != key {
		t.Fatalf("LiveGameSessions = %v, se esperaba [%s]", snap.LiveGameSessions, key)
	}
}

// TestSessionEndsWhenReaperExits: the session stays live while the reaper
// runs, even after the GPU process is gone, and ends with the reaper.
func TestSessionEndsWhenReaperExits(t *testing.T) {
	files := protonTree()
	prov := gameProc(100)
	c := collectorWith(prov, files)
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	prov.procs = nil
	snap, _ := c.Collect(context.Background())
	if len(snap.LiveGameSessions) != 1 {
		t.Fatalf("con el reaper vivo, LiveGameSessions = %v", snap.LiveGameSessions)
	}

	delete(files, procStatPath(90))
	snap, _ = c.Collect(context.Background())
	if len(snap.LiveGameSessions) != 0 || len(c.sessions) != 0 {
		t.Fatalf("tras salir el reaper, live=%v tracked=%d", snap.LiveGameSessions, len(c.sessions))
	}
}

// TestReusedOrZombieAnchorEndsSession: a new process with the same PID or a
// zombie anchor must not keep a session alive.
func TestReusedOrZombieAnchorEndsSession(t *testing.T) {
	for name, stat := range map[string]string{
		"reused": statLine(90, "bash", "S", 10, 9999),
		"zombie": statLine(90, "reaper", "Z", 10, 1000),
	} {
		t.Run(name, func(t *testing.T) {
			files := protonTree()
			prov := gameProc(100)
			c := collectorWith(prov, files)
			if _, err := c.Collect(context.Background()); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			prov.procs = nil
			files[procStatPath(90)] = stat
			if snap, _ := c.Collect(context.Background()); len(snap.LiveGameSessions) != 0 {
				t.Fatalf("LiveGameSessions = %v, se esperaba vacío", snap.LiveGameSessions)
			}
		})
	}
}

// TestWineExeOutsideSteam: Lutris/Heroic games anchor to the .exe itself.
func TestWineExeOutsideSteam(t *testing.T) {
	cases := map[string]string{
		"renamed": `Z:\home\u\Games\witcher3\bin\x64\witcher3.exe` + "\x00",
		"loader":  "/usr/bin/wine64-preloader\x00C:\\Games\\Game.EXE\x00",
	}
	for name, cmdline := range cases {
		t.Run(name, func(t *testing.T) {
			c := collectorWith(gameProc(200), map[string]string{
				procStatPath(200):    statLine(200, "game", "S", 150, 500),
				procCommPath(200):    "game\n",
				procCmdlinePath(200): cmdline,
				procStatPath(150):    statLine(150, "lutris", "S", 1, 400),
				procCmdlinePath(150): "/usr/bin/python3\x00/usr/bin/lutris\x00",
			})
			snap, err := c.Collect(context.Background())
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if key := snap.GPUProcessSessions[200]; !strings.HasPrefix(key, "wine:") || !strings.HasSuffix(key, "/200/500") {
				t.Fatalf("sesión = %q, se esperaba wine anclada al PID 200", key)
			}
		})
	}
}

// TestNativeAppHasNoSession: Blender outside Steam must resume after the
// normal cooldown, so it gets no session.
func TestNativeAppHasNoSession(t *testing.T) {
	c := collectorWith(gameProc(300), map[string]string{
		procStatPath(300):    statLine(300, "blender", "S", 250, 700),
		procCommPath(300):    "blender\n",
		procCmdlinePath(300): "/usr/bin/blender\x00",
		procStatPath(250):    statLine(250, "plasmashell", "S", 1, 50),
		procCmdlinePath(250): "/usr/bin/plasmashell\x00",
	})
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if snap.GPUProcessSessions != nil || snap.LiveGameSessions != nil {
		t.Fatalf("sesiones = %v / %v, se esperaba ninguna", snap.GPUProcessSessions, snap.LiveGameSessions)
	}
}

// TestAncestorLoopIsBounded: a malformed ppid cycle must terminate.
func TestAncestorLoopIsBounded(t *testing.T) {
	c := collectorWith(gameProc(400), map[string]string{
		procStatPath(400):    statLine(400, "a", "S", 401, 1),
		procCommPath(400):    "a\n",
		procCmdlinePath(400): "/bin/a\x00",
		procStatPath(401):    statLine(401, "b", "S", 400, 2),
		procCmdlinePath(401): "/bin/b\x00",
	})
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if snap.GPUProcessSessions != nil {
		t.Fatalf("sesiones = %v, se esperaba ninguna", snap.GPUProcessSessions)
	}
}

// TestSteamLaunchParsing covers reaper argv edge cases.
func TestSteamLaunchParsing(t *testing.T) {
	cases := []struct {
		argv []string
		id   string
		ok   bool
	}{
		{[]string{"/x/reaper", "SteamLaunch", "AppId=730", "--", "game"}, "730", true},
		{[]string{"/x/reaper", "SteamLaunch", "--", "AppId=1"}, "unknown", true},
		{[]string{"/x/reaper", "--", "SteamLaunch"}, "", false},
		{[]string{"/x/notreaper", "SteamLaunch", "AppId=730"}, "", false},
		{[]string{"/x/reaper"}, "", false},
	}
	for _, tc := range cases {
		id, ok := steamLaunch(tc.argv)
		if ok != tc.ok || (ok && id != tc.id) {
			t.Errorf("steamLaunch(%q) = %q, %v; se esperaba %q, %v", tc.argv, id, ok, tc.id, tc.ok)
		}
	}
}

// TestSessionHoldEndToEnd drives collector and evaluator together: a Proton
// game in its menu keeps folding paused until the reaper exits.
func TestSessionHoldEndToEnd(t *testing.T) {
	files := protonTree()
	prov := gameProc(100)
	c := collectorWith(prov, files)
	ev := NewEvaluator(DefaultConfig())

	tick := func() OperatingState {
		t.Helper()
		snap, err := c.Collect(context.Background())
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		return ev.Evaluate(snap)
	}
	for i := 0; i < ev.cfg.SustainedTicksRequired; i++ {
		tick()
	}
	prov.procs[0].SMUtilPct = 3 // main menu
	for i := 0; i < 2*ev.cfg.CooldownTicksRequired; i++ {
		if got := tick(); got != StatePausedLoad {
			t.Fatalf("menú tick %d = %q, se esperaba %q", i, got, StatePausedLoad)
		}
	}

	prov.procs = nil
	delete(files, procStatPath(90))
	var got OperatingState
	for i := 0; i < ev.cfg.CooldownTicksRequired; i++ {
		got = tick()
	}
	if got != StateFolding {
		t.Fatalf("tras cerrar el juego y el cooldown = %q, se esperaba %q", got, StateFolding)
	}
}
