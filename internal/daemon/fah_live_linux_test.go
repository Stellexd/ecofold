//go:build linux && fahlive

package daemon

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Stellexd/ecofold/internal/detector"
	"github.com/Stellexd/ecofold/internal/fah"
)

// The fahlive tests drive the daemon against the real FAH v8 container. They
// pause, freeze and restart it, so they are opt-in:
//
//	go test -tags fahlive -run TestLiveFAH -v ./internal/daemon/
//
// The GPU is a fake without power-limit support, so no root is needed and
// the real power limit is never touched.

const liveContainer = "foldingathome"

func docker(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %v: %v: %s", args, err, out)
	}
}

func liveStatus(t *testing.T, c fah.Controller) (fah.Status, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.GetStatus(ctx)
}

func waitLive(t *testing.T, what string, timeout time.Duration, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > timeout {
			t.Fatalf("tiempo agotado (%s) esperando: %s", timeout, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return time.Since(start)
}

// TestLiveFAHFrozenAndRestarted covers the failures seen on CachyOS:
// a container frozen by docker pause (the legacy autofold) and a FAH restart
// in the middle of the cooldown.
func TestLiveFAHFrozenAndRestarted(t *testing.T) {
	client, err := fah.NewClientV8("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "unpause", liveContainer).Run()
		c, err := fah.NewClientV8("")
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = c.Resume(ctx)
			cancel()
			_ = c.Close()
		}
	})
	if st, err := liveStatus(t, client); err != nil || st.Paused {
		t.Fatalf("FAH debe estar plegando al empezar: %+v, %v", st, err)
	}

	log := &callLog{}
	logs := &syncBuffer{}
	col := &scriptedCollector{snap: snapIdle}
	cfg := DefaultConfig()
	cfg.Tick = 250 * time.Millisecond // cooldown of 60 ticks = 15 s
	cfg.RuntimeDir = t.TempDir()
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	d, err := New(cfg, Deps{GPU: newFakeGPU(log, false), FAH: client, Collector: col, Evaluator: detector.NewEvaluator(detector.DefaultConfig())})
	if err != nil {
		t.Fatal(err)
	}
	m := marker{dir: cfg.RuntimeDir}
	owned := func() bool { ok, _ := m.present(); return ok }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// 1. Freeze FAH, then load: the loop must keep ticking while every FAH
	// call times out.
	docker(t, "pause", liveContainer)
	col.set(snapLoad, nil)
	before := col.callCount()
	time.Sleep(8 * time.Second)
	ticks := col.callCount() - before
	t.Logf("ticks con FAH congelado en 8 s: %d", ticks)
	if ticks < 3 {
		t.Fatalf("el loop se bloqueó con FAH congelado: %d ticks en 8 s", ticks)
	}
	if owned() {
		t.Fatal("se tomó la pausa como propia sin confirmación de FAH")
	}

	// 2. Thaw: the pending pause must land within a couple of ticks.
	docker(t, "unpause", liveContainer)
	took := waitLive(t, "pausa confirmada tras descongelar", 15*time.Second, owned)
	t.Logf("pausa confirmada %s después de descongelar", took.Round(time.Millisecond))
	if st, err := liveStatus(t, client); err != nil || !st.Paused {
		t.Fatalf("FAH no quedó pausado: %+v, %v", st, err)
	}

	// 3. Idle → cooldown; restart FAH halfway through it.
	col.set(snapIdle, nil)
	time.Sleep(7 * time.Second)
	docker(t, "restart", liveContainer)
	took = waitLive(t, "reanudación tras el cooldown y el reinicio de FAH", 90*time.Second, func() bool { return !owned() })
	t.Logf("reanudado %s después del reinicio de FAH", took.Round(time.Millisecond))
	waitLive(t, "FAH plegando", 30*time.Second, func() bool {
		st, err := liveStatus(t, client)
		return err == nil && !st.Paused
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := strings.Count(logs.String(), "fah action failed"); n > 5 {
		t.Fatalf("demasiados avisos de FAH (%d); el rate limit no funciona", n)
	}
	t.Logf("log del daemon:\n%s", logs.String())
}
