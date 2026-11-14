package daemon

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/Stellexd/ecofold/internal/detector"
)

// randomCollector returns load, idle, low battery or an error at random.
type randomCollector struct {
	mu  sync.Mutex
	rng *rand.Rand
}

func (c *randomCollector) Collect(ctx context.Context) (detector.SystemSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.rng.Intn(6) {
	case 0, 1:
		return snapLoad, nil
	case 2:
		return snapLowBattery, nil
	case 3:
		return detector.SystemSnapshot{}, context.DeadlineExceeded
	default:
		return snapIdle, nil
	}
}

// TestChaosRunCancelAtRandomPoints cancels Run at random moments while GPU
// and FAH calls are slow, so cancellation lands at every step of a tick. Run
// must return promptly, restore the limit once on exit, shut the GPU down
// and never call FAH after Close.
func TestChaosRunCancelAtRandomPoints(t *testing.T) {
	iterations := 200
	if testing.Short() {
		iterations = 20
	}
	for i := 0; i < iterations; i++ {
		rng := rand.New(rand.NewSource(int64(i)))
		h := newHarness(t, true)
		h.d.collector = &randomCollector{rng: rand.New(rand.NewSource(int64(i)))}
		h.d.cfg.Tick = time.Millisecond
		h.d.cfg.StartupHoldTicks = 1
		h.gpu.set(func(g *fakeGPU) { g.delay = time.Duration(rng.Intn(3)) * time.Millisecond })
		h.fah.set(func(f *fakeFAH) { f.delay = time.Duration(rng.Intn(4)) * time.Millisecond })

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.d.Run(ctx) }()
		time.Sleep(time.Duration(rng.Intn(40)) * time.Millisecond)
		before := h.log.count("gpu.Restore")
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iteración %d: Run devolvió error: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteración %d: Run no terminó tras cancelar", i)
		}
		if got := h.log.count("gpu.Restore"); got < before+1 {
			t.Fatalf("iteración %d: sin restauración al salir: %v", i, h.log.all())
		}
		if h.log.count("gpu.Shutdown") != 1 || h.log.count("fah.Close") != 1 {
			t.Fatalf("iteración %d: faltan Shutdown/Close: %v", i, h.log.all())
		}
		calls := h.log.all()
		if last := calls[len(calls)-1]; last != "gpu.Shutdown" {
			t.Fatalf("iteración %d: última llamada %q, se esperaba gpu.Shutdown", i, last)
		}
		h.fah.mu.Lock()
		after := h.fah.afterClose
		h.fah.mu.Unlock()
		if after != 0 {
			t.Fatalf("iteración %d: %d llamadas a FAH después de Close", i, after)
		}
	}
}
