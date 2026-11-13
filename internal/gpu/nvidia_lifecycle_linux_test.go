//go:build linux && cgo

package gpu

import (
	"sync"
	"testing"
)

// nvmlRefsNow reads the process-wide NVML reference count.
func nvmlRefsNow() int {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	return nvmlRefs
}

// requireNVML skips the test when NVML or an NVIDIA driver is unavailable, so
// the lifecycle tests remain runnable on Linux machines without a GPU. It
// leaves the reference count exactly as it found it.
func requireNVML(t *testing.T) {
	t.Helper()
	p := &NvidiaProvider{}
	if err := p.Init(); err != nil {
		t.Skipf("NVML unavailable: %v", err)
	}
	if err := p.Shutdown(); err != nil {
		t.Skipf("NVML shutdown failed: %v", err)
	}
}

// TestNvidiaInitIsIdempotent guards CH-11: a second Init must not increment the
// NVML reference count, and a second Shutdown must not touch NVML again.
func TestNvidiaInitIsIdempotent(t *testing.T) {
	requireNVML(t)
	base := nvmlRefsNow()

	p := &NvidiaProvider{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := nvmlRefsNow(); got != base+1 {
		t.Fatalf("after Init: refs = %d, want %d", got, base+1)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if got := nvmlRefsNow(); got != base+1 {
		t.Fatalf("second Init leaked a reference: refs = %d, want %d", got, base+1)
	}
	if err := p.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := nvmlRefsNow(); got != base {
		t.Fatalf("after Shutdown: refs = %d, want %d", got, base)
	}
	if err := p.Shutdown(); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if got := nvmlRefsNow(); got != base {
		t.Fatalf("second Shutdown changed refs: refs = %d, want %d", got, base)
	}
}

// TestNvidiaShutdownBeforeInitIsNoop guards CH-11: Shutdown before Init must
// not call nvml.Shutdown on behalf of another live provider.
func TestNvidiaShutdownBeforeInitIsNoop(t *testing.T) {
	requireNVML(t)
	base := nvmlRefsNow()

	live := &NvidiaProvider{}
	if err := live.Init(); err != nil {
		t.Fatalf("live Init: %v", err)
	}
	t.Cleanup(func() { _ = live.Shutdown() })

	stray := &NvidiaProvider{}
	if err := stray.Shutdown(); err != nil {
		t.Fatalf("Shutdown before Init: %v (want nil no-op)", err)
	}
	if got := nvmlRefsNow(); got != base+1 {
		t.Fatalf("stray Shutdown changed refs: refs = %d, want %d", got, base+1)
	}
	if _, err := live.GetTelemetry(); err != nil {
		t.Fatalf("live provider broke after stray Shutdown: %v", err)
	}
}

// TestNvidiaProvidersShareProcessGlobalNVML verifies that two providers can
// coexist: shutting one down keeps NVML alive for the other, and only the last
// Shutdown tears NVML down.
func TestNvidiaProvidersShareProcessGlobalNVML(t *testing.T) {
	requireNVML(t)
	base := nvmlRefsNow()

	p1 := &NvidiaProvider{}
	p2 := &NvidiaProvider{}
	if err := p1.Init(); err != nil {
		t.Fatalf("p1 Init: %v", err)
	}
	if err := p2.Init(); err != nil {
		t.Fatalf("p2 Init: %v", err)
	}
	if got := nvmlRefsNow(); got != base+2 {
		t.Fatalf("with two providers: refs = %d, want %d", got, base+2)
	}
	if err := p1.Shutdown(); err != nil {
		t.Fatalf("p1 Shutdown: %v", err)
	}
	if got := nvmlRefsNow(); got != base+1 {
		t.Fatalf("after p1 Shutdown: refs = %d, want %d", got, base+1)
	}
	if _, err := p2.GetTelemetry(); err != nil {
		t.Fatalf("p2 broke after p1 Shutdown: %v", err)
	}
	if _, err := p1.GetTelemetry(); err == nil {
		t.Fatal("p1 still returns telemetry after Shutdown")
	}
	if err := p2.Shutdown(); err != nil {
		t.Fatalf("p2 Shutdown: %v", err)
	}
	if got := nvmlRefsNow(); got != base {
		t.Fatalf("after last Shutdown: refs = %d, want %d", got, base)
	}
}

// TestNvidiaReinitAfterShutdown verifies the daemon's restart path: the same
// provider can acquire NVML again after a full shutdown.
func TestNvidiaReinitAfterShutdown(t *testing.T) {
	requireNVML(t)
	base := nvmlRefsNow()

	p := &NvidiaProvider{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := p.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := nvmlRefsNow(); got != base {
		t.Fatalf("after Shutdown: refs = %d, want %d", got, base)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown() })
	if got := nvmlRefsNow(); got != base+1 {
		t.Fatalf("after re-Init: refs = %d, want %d", got, base+1)
	}
	if _, err := p.GetTelemetry(); err != nil {
		t.Fatalf("GetTelemetry after re-Init: %v", err)
	}
}

// TestNvidiaConcurrentInitShutdown runs under -race and verifies that racing
// lifecycle calls never leak or underflow the NVML reference count.
func TestNvidiaConcurrentInitShutdown(t *testing.T) {
	requireNVML(t)
	base := nvmlRefsNow()

	p := &NvidiaProvider{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Init()
			_ = p.Shutdown()
		}()
	}
	wg.Wait()

	if got := nvmlRefsNow(); got != base {
		t.Fatalf("after concurrent lifecycle: refs = %d, want %d", got, base)
	}
}
