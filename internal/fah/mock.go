package fah

import (
	"context"
	"sync"
)

// MockController is an in-memory Controller for daemon tests and for
// development when Folding@home is not running. It is safe for concurrent use.
type MockController struct {
	mu      sync.RWMutex
	status  Status
	project int // project restored by Resume
	err     error
	closed  bool
}

var _ Controller = (*MockController)(nil)

// NewMockController returns a mock that starts folding project.
func NewMockController(project int) *MockController {
	return &MockController{
		status:  Status{ActiveProjectID: project, State: StateFolding},
		project: project,
	}
}

// SetError makes every later call fail with err, simulating an unreachable
// client. A nil err restores normal behavior.
func (m *MockController) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// Pause marks folding as paused.
func (m *MockController) Pause(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.check(ctx); err != nil {
		return err
	}
	m.status = Status{Paused: true, State: StatePaused}
	return nil
}

// Resume marks folding as running the last known project, or idle if there is
// none.
func (m *MockController) Resume(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.check(ctx); err != nil {
		return err
	}
	if m.project == 0 {
		m.status = Status{State: StateIdle}
	} else {
		m.status = Status{ActiveProjectID: m.project, State: StateFolding}
	}
	return nil
}

// GetStatus returns the simulated status.
func (m *MockController) GetStatus(ctx context.Context) (Status, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := m.check(ctx); err != nil {
		return Status{State: StateUnknown}, err
	}
	return m.status, nil
}

// Close makes every later call return ErrClosed. It is idempotent.
func (m *MockController) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// check must be called with m.mu held.
func (m *MockController) check(ctx context.Context) error {
	if m.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.err
}
