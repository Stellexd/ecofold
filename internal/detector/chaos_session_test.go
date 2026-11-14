package detector

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/Stellexd/ecofold/internal/gpu"
)

// The session chaos model feeds the evaluator random ticks of load, game
// sessions and live sets, and checks properties that must hold for any
// sequence:
//
//	S1: a pause starts only after SustainedTicksRequired consecutive load
//	    ticks; sessions never start one.
//	S2: after CooldownTicksRequired consecutive ticks with no load and no live
//	    session that ever caused load, the evaluator is not paused.
//	S3: the held set never exceeds maxHeldSessions.
//	S4: with no sessions anywhere, the result equals a session-free evaluator.

var chaosSessions = []string{"steam:1/1/1", "steam:2/2/2", "wine:a.exe/3/3", "steam:431960/4/4"}

// sessionTick decodes one byte: bit 0 load, bits 1-2 the session of the GPU
// process (0 none), bits 3-6 which sessions are live.
func sessionTick(b byte) (SystemSnapshot, bool, string) {
	high := b&1 != 0
	util := 5
	if high {
		util = 60
	}
	s := SystemSnapshot{
		OnAC:         true,
		GPUProcesses: []gpu.ProcessInfo{{PID: 100, UsedVRAMMiB: 512, SMUtilPct: util, SMUtilValid: true}},
	}
	tag := ""
	if idx := int(b>>1) & 0x3; idx > 0 {
		tag = chaosSessions[idx-1]
		s.GPUProcessSessions = map[uint32]string{100: tag}
	}
	for i := range chaosSessions {
		if b&(1<<(3+i)) != 0 {
			s.LiveGameSessions = append(s.LiveGameSessions, chaosSessions[i])
		}
	}
	return s, high, tag
}

func runSessionModel(t testing.TB, events []byte) {
	cfg := DefaultConfig()
	ev := NewEvaluator(cfg)
	plain := NewEvaluator(cfg)
	sustain, cooldown := cfg.SustainedTicksRequired, cfg.CooldownTicksRequired

	highStreak, quietStreak := 0, 0
	caused := map[string]bool{}
	prev := StateFolding
	anySession := false

	for i, b := range events {
		snap, high, tag := sessionTick(b)
		if tag != "" || len(snap.LiveGameSessions) > 0 {
			anySession = true
		}
		got := ev.Evaluate(snap)
		bare := snap
		bare.GPUProcessSessions, bare.LiveGameSessions = nil, nil
		want := plain.Evaluate(bare)

		if high {
			highStreak++
			if tag != "" {
				caused[tag] = true
			}
		} else {
			highStreak = 0
		}
		liveCaused := false
		for _, key := range snap.LiveGameSessions {
			if caused[key] {
				liveCaused = true
			}
		}
		if high || liveCaused {
			quietStreak = 0
		} else {
			quietStreak++
		}

		if prev != StatePausedLoad && got == StatePausedLoad && highStreak < sustain {
			t.Fatalf("S1 tick %d: pausa con racha de carga %d < %d", i, highStreak, sustain)
		}
		if quietStreak >= cooldown && got == StatePausedLoad {
			t.Fatalf("S2 tick %d: sigue en pausa tras %d ticks sin carga ni sesión causante", i, quietStreak)
		}
		if n := len(ev.heldSessions); n > maxHeldSessions {
			t.Fatalf("S3 tick %d: heldSessions = %d", i, n)
		}
		if !anySession && got != want {
			t.Fatalf("S4 tick %d: con sesiones=%q, sin sesiones=%q", i, got, want)
		}
		// Sessions are forgotten only on a tick with no load and no pause;
		// a load streak keeps them even before the pause starts.
		if !high && got != StatePausedLoad {
			caused = map[string]bool{}
		}
		prev = got
	}
}

// TestChaosSessionModel runs fixed seeds; long runs of the same byte make
// sustain and cooldown reachable.
func TestChaosSessionModel(t *testing.T) {
	seeds := 200
	if testing.Short() {
		seeds = 40
	}
	for seed := int64(0); seed < int64(seeds); seed++ {
		rng := rand.New(rand.NewSource(seed))
		var events []byte
		for len(events) < 2000 {
			b := byte(rng.Intn(256))
			for n := rng.Intn(80) + 1; n > 0; n-- {
				events = append(events, b)
			}
		}
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) { runSessionModel(t, events) })
	}
}

// FuzzEvaluatorSessions explores tick sequences with the native fuzzer. Each
// input byte is repeated (high bit of the next byte picks 1 or 70 times) so
// cooldowns are reachable from short inputs.
func FuzzEvaluatorSessions(f *testing.F) {
	f.Add([]byte{0x03, 0x03, 0x03, 0x82, 0x80})       // load in session 1, menu, exit
	f.Add([]byte{0x41, 0x41, 0x41, 0xc0})             // native load, unrelated live session
	f.Add([]byte{0x7f, 0x7f, 0x7f, 0x78, 0xf8, 0x80}) // every session live
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) > 64 {
			in = in[:64]
		}
		var events []byte
		for i, b := range in {
			n := 1
			if i+1 < len(in) && in[i+1]&0x80 != 0 {
				n = 70
			}
			for ; n > 0; n-- {
				events = append(events, b&0x7f)
			}
		}
		runSessionModel(t, events)
	})
}
