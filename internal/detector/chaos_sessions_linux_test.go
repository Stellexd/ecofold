//go:build linux

package detector

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// procTree is a mutable fake /proc built from fuzz bytes.
type procTree struct {
	mu    sync.Mutex
	files map[string]string
	reads int
	// vanishAfter makes every read fail with ENOENT once reads exceeds it,
	// simulating processes exiting in the middle of the walk.
	vanishAfter int
}

func (p *procTree) read(path string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if p.vanishAfter > 0 && p.reads > p.vanishAfter {
		return nil, fs.ErrNotExist
	}
	v, ok := p.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(v), nil
}

// buildTree decodes up to 40 processes. Per process: byte 0 picks the parent
// (any index, so cycles and self-parents happen), byte 1 the kind.
func buildTree(in []byte) (*procTree, []gpu.ProcessInfo) {
	tree := &procTree{files: map[string]string{}}
	var procs []gpu.ProcessInfo
	n := len(in) / 2
	if n > 40 {
		n = 40
	}
	for i := 0; i < n; i++ {
		pid := uint32(1000 + i)
		ppid := uint32(1000 + int(in[2*i])%(n+1))
		if int(in[2*i])%(n+1) == n {
			ppid = 1
		}
		kind := in[2*i+1]
		state := "S"
		if kind&0x80 != 0 {
			state = "Z"
		}
		var cmd string
		switch kind & 0x07 {
		case 0:
			cmd = "/home/u/.local/share/Steam/steamrt64/reaper\x00SteamLaunch\x00AppId=" + fmt.Sprint(kind) + "\x00--\x00x\x00"
		case 1:
			cmd = `S:\steamapps\common\Game\game.exe` + "\x00-steam\x00\x00\x00"
		case 2:
			cmd = "/usr/bin/wine64-preloader\x00C:\\x\\y.EXE\x00"
		case 3:
			cmd = "/usr/bin/blender\x00"
		case 4:
			cmd = "" // kernel thread or exited
		case 5:
			cmd = "reaper\x00--\x00SteamLaunch\x00" // SteamLaunch after "--"
		case 6:
			cmd = strings.Repeat("A", 300) + "\x00"
		case 7:
			cmd = "\x00\x00\x00"
		}
		if kind&0x40 == 0 {
			tree.files[procCmdlinePath(pid)] = cmd
		}
		if kind&0x20 == 0 {
			tree.files[procStatPath(pid)] = statLine(pid, "c) (x", state, ppid, uint64(kind)+1)
		} else if kind&0x10 != 0 {
			tree.files[procStatPath(pid)] = "garbage"
		}
		tree.files[procCommPath(pid)] = "proc\n"
		if kind&0x08 != 0 {
			procs = append(procs, gpu.ProcessInfo{PID: pid, UsedVRAMMiB: 2048, SMUtilPct: 60, SMUtilValid: true})
		}
	}
	return tree, procs
}

func checkCollect(t testing.TB, c *LinuxCollector, procs []gpu.ProcessInfo) SystemSnapshot {
	t.Helper()
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	kept := map[uint32]bool{}
	for _, p := range snap.GPUProcesses {
		kept[p.PID] = true
	}
	for pid, key := range snap.GPUProcessSessions {
		if !kept[pid] {
			t.Fatalf("sesión %q para el PID %d, que no está en GPUProcesses", key, pid)
		}
		if !strings.HasPrefix(key, "steam:") && !strings.HasPrefix(key, "wine:") {
			t.Fatalf("clave de sesión inesperada %q", key)
		}
	}
	if len(c.sessions) > maxTrackedSessions {
		t.Fatalf("sesiones rastreadas = %d > %d", len(c.sessions), maxTrackedSessions)
	}
	for _, key := range snap.LiveGameSessions {
		if _, ok := c.sessions[key]; !ok {
			t.Fatalf("sesión viva %q no rastreada", key)
		}
	}
	return snap
}

func runTreeModel(t testing.TB, in []byte, vanish int) {
	tree, procs := buildTree(in)
	tree.vanishAfter = vanish
	prov := &fakeGPU{tel: gpu.Telemetry{Name: "GPU"}, procs: procs}
	c := NewLinuxCollector(prov)
	c.readFile = tree.read

	checkCollect(t, c, procs)
	// Reads per tick stay bounded: comm + cmdline + argv per process plus at
	// most two reads per ancestor level, and one stat per tracked session.
	if limit := len(procs)*(3+2*(maxAncestorDepth+1)) + maxTrackedSessions + 1; tree.reads > limit {
		t.Fatalf("lecturas de /proc = %d > %d", tree.reads, limit)
	}

	// Every process exits: the next tick has no live session.
	tree.mu.Lock()
	tree.files, tree.vanishAfter = map[string]string{}, 0
	tree.mu.Unlock()
	prov.procs = nil
	if snap := checkCollect(t, c, nil); len(snap.LiveGameSessions) != 0 {
		t.Fatalf("sesiones vivas tras salir todo: %v", snap.LiveGameSessions)
	}
}

// FuzzCollectorProcTree feeds random, hostile /proc trees to the collector:
// cycles, self-parents, zombies, malformed stat, missing files, empty argv and
// processes that vanish in the middle of the walk.
func FuzzCollectorProcTree(f *testing.F) {
	f.Add([]byte{3, 0x08, 0, 0x01, 1, 0x09}, 0) // reaper -> exe (GPU)
	f.Add([]byte{0, 0x09, 0, 0x0b}, 0)          // self-parent cycle
	f.Add([]byte{1, 0x88, 0, 0x89}, 0)          // zombies
	f.Add([]byte{1, 0x39, 0, 0x29, 1, 0x49}, 3) // malformed stat, vanish
	f.Add(make([]byte, 80), 0)                  // 40 reapers in a chain
	f.Fuzz(func(t *testing.T, in []byte, vanish int) {
		if vanish < 0 || vanish > 200 {
			vanish = 0
		}
		runTreeModel(t, in, vanish)
	})
}

// TestChaosCollectorProcTree runs fixed random trees in the normal suite.
func TestChaosCollectorProcTree(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		in := make([]byte, 2*(rng.Intn(40)+1))
		rng.Read(in)
		vanish := 0
		if seed%3 == 0 {
			vanish = rng.Intn(60) + 1
		}
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) { runTreeModel(t, in, vanish) })
	}
}

// TestChaosTrackedSessionsBounded opens more games than maxTrackedSessions.
func TestChaosTrackedSessionsBounded(t *testing.T) {
	files := map[string]string{}
	var procs []gpu.ProcessInfo
	for i := 0; i < 3*maxTrackedSessions; i++ {
		pid := uint32(5000 + i)
		files[procStatPath(pid)] = statLine(pid, "g.exe", "S", 1, uint64(i+1))
		files[procCommPath(pid)] = "g.exe\n"
		files[procCmdlinePath(pid)] = fmt.Sprintf(`C:\g%d.exe`, i) + "\x00"
		procs = append(procs, gpu.ProcessInfo{PID: pid, UsedVRAMMiB: 2048, SMUtilPct: 60, SMUtilValid: true})
	}
	c := collectorWith(&fakeGPU{procs: procs}, files)
	for i := 0; i < 5; i++ {
		checkCollect(t, c, procs)
	}
}

// TestSpoofedReaperOnlyHoldsItsOwnLoad documents the accepted risk: any user
// process can name itself `reaper SteamLaunch` and become a session. It still
// cannot start a pause, and it only holds a pause its own GPU load caused,
// which the same user could get by pausing FAH by hand.
func TestSpoofedReaperOnlyHoldsItsOwnLoad(t *testing.T) {
	files := map[string]string{
		procStatPath(700):    statLine(700, "reaper", "S", 1, 10),
		procCmdlinePath(700): "/tmp/reaper\x00SteamLaunch\x00AppId=1\x00",
		procStatPath(701):    statLine(701, "miner", "S", 700, 11),
		procCommPath(701):    "miner\n",
		procCmdlinePath(701): "/tmp/miner\x00",
	}
	prov := &fakeGPU{procs: []gpu.ProcessInfo{{PID: 701, UsedVRAMMiB: 100, SMUtilPct: 5, SMUtilValid: true}}}
	c := collectorWith(prov, files)
	ev := NewEvaluator(DefaultConfig())
	for i := 0; i < 200; i++ {
		snap, err := c.Collect(context.Background())
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if len(snap.LiveGameSessions) != 1 {
			t.Fatalf("se esperaba la sesión falsa viva: %v", snap.LiveGameSessions)
		}
		if got := ev.Evaluate(snap); got != StateFolding {
			t.Fatalf("tick %d = %q: una sesión sin carga no debe pausar", i, got)
		}
	}
}

// BenchmarkCollectSessions measures one tick with 10 GPU processes, each 8
// levels below a reaper.
func BenchmarkCollectSessions(b *testing.B) {
	files := map[string]string{}
	var procs []gpu.ProcessInfo
	for g := 0; g < 10; g++ {
		base := uint32(10000 + 100*g)
		files[procStatPath(base)] = statLine(base, "reaper", "S", 1, 1)
		files[procCmdlinePath(base)] = "/x/reaper\x00SteamLaunch\x00AppId=1\x00"
		for l := uint32(1); l <= 8; l++ {
			files[procStatPath(base+l)] = statLine(base+l, "x", "S", base+l-1, uint64(l))
			files[procCmdlinePath(base+l)] = "/x/wrap\x00"
		}
		files[procCommPath(base+8)] = "game.exe\n"
		procs = append(procs, gpu.ProcessInfo{PID: base + 8, UsedVRAMMiB: 2048, SMUtilPct: 60, SMUtilValid: true})
	}
	c := collectorWith(&fakeGPU{procs: procs}, files)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.Collect(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
