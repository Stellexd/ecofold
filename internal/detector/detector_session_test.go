package detector

import (
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

const testSession = "steam:1422450/90/1000"

// sessionSnap is a Linux-style snapshot with one GPU process at utilPct,
// optionally tied to session, plus the live sessions of this tick.
func sessionSnap(utilPct int, session string, live ...string) SystemSnapshot {
	s := SystemSnapshot{
		OnAC:             true,
		GPUProcesses:     []gpu.ProcessInfo{{PID: 100, UsedVRAMMiB: 512, SMUtilPct: utilPct, SMUtilValid: true}},
		LiveGameSessions: live,
	}
	if session != "" {
		s.GPUProcessSessions = map[uint32]string{100: session}
	}
	return s
}

func pauseWith(t *testing.T, ev *Evaluator, snap SystemSnapshot) {
	t.Helper()
	var got OperatingState
	for i := 0; i < ev.cfg.SustainedTicksRequired; i++ {
		got = ev.Evaluate(snap)
	}
	if got != StatePausedLoad {
		t.Fatalf("estado tras la carga sostenida = %q, se esperaba %q", got, StatePausedLoad)
	}
}

// TestSessionHoldsPauseWhileGameRuns: a game in a menu (low load) keeps the
// pause for as long as its session lives, then the normal cooldown runs.
func TestSessionHoldsPauseWhileGameRuns(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	pauseWith(t, ev, sessionSnap(60, testSession, testSession))

	menu := sessionSnap(5, testSession, testSession)
	for i := 0; i < 5*ev.cfg.CooldownTicksRequired; i++ {
		if got := ev.Evaluate(menu); got != StatePausedLoad {
			t.Fatalf("tick %d en el menú = %q, se esperaba %q", i, got, StatePausedLoad)
		}
	}

	exited := SystemSnapshot{OnAC: true, GPUProcesses: []gpu.ProcessInfo{}}
	for i := 0; i < ev.cfg.CooldownTicksRequired-1; i++ {
		if got := ev.Evaluate(exited); got != StatePausedLoad {
			t.Fatalf("cooldown tick %d = %q, se esperaba %q", i, got, StatePausedLoad)
		}
	}
	if got := ev.Evaluate(exited); got != StateFolding {
		t.Fatalf("fin del cooldown = %q, se esperaba %q", got, StateFolding)
	}
}

// TestUnrelatedLiveSessionDoesNotHold: a session that never caused load (for
// example Wallpaper Engine under Proton) must not hold a pause caused by a
// native app.
func TestUnrelatedLiveSessionDoesNotHold(t *testing.T) {
	const wallpaper = "steam:431960/50/10"
	ev := NewEvaluator(DefaultConfig())
	pauseWith(t, ev, sessionSnap(60, "", wallpaper))

	idle := sessionSnap(5, "", wallpaper)
	for i := 0; i < ev.cfg.CooldownTicksRequired-1; i++ {
		ev.Evaluate(idle)
	}
	if got := ev.Evaluate(idle); got != StateFolding {
		t.Fatalf("estado = %q, se esperaba %q (la sesión ajena no debe sostener la pausa)", got, StateFolding)
	}
}

// TestSessionNeverStartsPause: a live session with an idle GPU never pauses.
func TestSessionNeverStartsPause(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	idle := sessionSnap(5, testSession, testSession)
	for i := 0; i < 100; i++ {
		if got := ev.Evaluate(idle); got != StateFolding {
			t.Fatalf("tick %d = %q, se esperaba %q", i, got, StateFolding)
		}
	}
}

// TestUnsustainedLoadDropsHeldSession: load that never reaches the sustain
// count must not leave a session held for a later, unrelated pause.
func TestUnsustainedLoadDropsHeldSession(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	for i := 0; i < ev.cfg.SustainedTicksRequired-1; i++ {
		ev.Evaluate(sessionSnap(60, testSession, testSession))
	}
	ev.Evaluate(sessionSnap(5, testSession, testSession))

	// A native app now causes a pause; the old session is still live.
	pauseWith(t, ev, sessionSnap(60, "", testSession))
	idle := sessionSnap(5, "", testSession)
	for i := 0; i < ev.cfg.CooldownTicksRequired-1; i++ {
		ev.Evaluate(idle)
	}
	if got := ev.Evaluate(idle); got != StateFolding {
		t.Fatalf("estado = %q, se esperaba %q", got, StateFolding)
	}
}

// TestHeldSessionsAreBounded checks that many distinct sessions cannot grow
// the held set without limit.
func TestHeldSessionsAreBounded(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	for i := 0; i < 10*maxHeldSessions; i++ {
		key := "wine:game.exe/" + string(rune('a'+i%26)) + "/" + string(rune('A'+i/26))
		ev.Evaluate(sessionSnap(60, key, key))
	}
	if n := len(ev.heldSessions); n > maxHeldSessions {
		t.Fatalf("heldSessions = %d, máximo %d", n, maxHeldSessions)
	}
}

// TestBatteryStillWinsDuringSessionHold keeps the documented priority: a
// battery pause is reported once the load is gone, even while a session holds.
func TestBatteryStillWinsDuringSessionHold(t *testing.T) {
	ev := NewEvaluator(DefaultConfig())
	pauseWith(t, ev, sessionSnap(60, testSession, testSession))

	low := sessionSnap(5, testSession, testSession)
	low.HasBattery, low.OnAC, low.BatteryPct = true, false, 10
	if got := ev.Evaluate(low); got != StatePausedBattery {
		t.Fatalf("estado = %q, se esperaba %q", got, StatePausedBattery)
	}
}
