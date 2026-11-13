//go:build darwin

package detector

import (
	"context"
	"fmt"
)

// DefaultBatteryRefreshTicks is how many Collect calls reuse a cached
// AppleSmartBattery reading. At the daemon's 2 s tick it refreshes the battery
// every 30 s; the GPU is read on every call because game detection needs it.
const DefaultBatteryRefreshTicks = 15

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
// evaluator. A failed battery refresh is retried on the next call.
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
