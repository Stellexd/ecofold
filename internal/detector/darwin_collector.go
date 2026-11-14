//go:build darwin

package detector

import (
	"context"
	"errors"
	"fmt"
)

// DefaultBatteryRefreshTicks is how many Collect calls reuse a cached
// AppleSmartBattery reading. At the daemon's 2 s tick it refreshes the battery
// every 30 s; the GPU is read on every call because game detection needs it.
const DefaultBatteryRefreshTicks = 15

// ErrBatteryVanished is returned by DarwinCollector.Collect when
// AppleSmartBattery reports no battery after an earlier read found one. A
// MacBook does not lose its battery at runtime, so the empty result is a
// transient ioreg glitch; reporting it as a desktop Mac on AC would fold at
// full power on battery (R4).
var ErrBatteryVanished = errors.New("detector: battery vanished after it was seen")

// DarwinCollector builds SystemSnapshot values on macOS from ioreg output.
//
// It reads IOAccelerator on every Collect and AppleSmartBattery every
// DefaultBatteryRefreshTicks calls, so ioreg runs at most twice per daemon
// tick and usually once.
//
// DarwinCollector is not safe for concurrent use by multiple goroutines; each
// daemon tick creates one snapshot at a time.
type DarwinCollector struct {
	// fetch is the ioreg seam. Production uses FetchIORegContext.
	fetch func(ctx context.Context, class string) ([]byte, error)

	batteryEvery int
	calls        int
	batt         BatteryMetrics
	battValid    bool
	// seenBattery latches once any read reports HasBattery=true.
	seenBattery bool
}

// NewDarwinCollector returns a collector that runs /usr/sbin/ioreg.
func NewDarwinCollector() *DarwinCollector {
	return &DarwinCollector{
		fetch:        FetchIORegContext,
		batteryEvery: DefaultBatteryRefreshTicks,
	}
}

// Collect returns a snapshot with the current GPU metrics and the cached (or
// freshly read) battery metrics.
//
// Any fetch or parse error is returned instead of an empty snapshot: the
// caller must keep its previous decision rather than feed "no load" to the
// evaluator. A failed battery refresh is retried on the next call. An empty
// AppleSmartBattery result after a battery was seen returns
// ErrBatteryVanished.
func (c *DarwinCollector) Collect(ctx context.Context) (SystemSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return SystemSnapshot{}, err
	}

	every := c.batteryEvery
	if every <= 0 {
		every = DefaultBatteryRefreshTicks
	}
	if !c.battValid || c.calls%every == 0 {
		raw, err := c.fetch(ctx, "AppleSmartBattery")
		if err != nil {
			c.battValid = false
			return SystemSnapshot{}, fmt.Errorf("detector: battery: %w", err)
		}
		batt, err := ParseSmartBatteryPlist(raw)
		if err != nil {
			c.battValid = false
			return SystemSnapshot{}, fmt.Errorf("detector: battery: %w", err)
		}
		if !batt.HasBattery && c.seenBattery {
			c.battValid = false
			return SystemSnapshot{}, ErrBatteryVanished
		}
		c.seenBattery = c.seenBattery || batt.HasBattery
		c.batt, c.battValid = batt, true
		c.calls = 0
	}

	raw, err := c.fetch(ctx, "IOAccelerator")
	if err != nil {
		return SystemSnapshot{}, fmt.Errorf("detector: gpu: %w", err)
	}
	gpu, err := ParseIOAcceleratorPlist(raw)
	if err != nil {
		return SystemSnapshot{}, fmt.Errorf("detector: gpu: %w", err)
	}

	c.calls++
	return snapshotFromMetrics(c.batt, gpu), nil
}
