// Package fah controls the local Folding@home client: it pauses and resumes
// folding and reports the client's state. The contract is platform-agnostic;
// ClientV8 talks to a real FAH v8 client and MockController is an in-memory
// stand-in for tests and development.
package fah

import (
	"context"
	"errors"
)

// Values of Status.State.
const (
	StateFolding = "folding" // not paused and a work unit is running
	StatePaused  = "paused"  // folding is paused in every resource group
	StateIdle    = "idle"    // not paused, but no work unit is running
	StateUnknown = "unknown" // the client state could not be determined
)

// ErrClosed is returned by every Controller method after Close.
var ErrClosed = errors.New("fah: controller closed")

// Status is a snapshot of the Folding@home client state.
type Status struct {
	Paused bool
	// ActiveProjectID is the project of the running work unit (for example
	// 18626), or 0 when no work unit is running.
	ActiveProjectID int
	// State is one of StateFolding, StatePaused, StateIdle or StateUnknown.
	State string
}

// Controller pauses and resumes the local Folding@home client. Every method
// must honor ctx cancellation and deadlines. Implementations are safe for
// concurrent use.
type Controller interface {
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	GetStatus(ctx context.Context) (Status, error)
	Close() error
}
