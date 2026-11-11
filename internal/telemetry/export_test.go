package telemetry

import "time"

// SetClock replaces the Observer's clock, so tests can assert exact durations.
func (o *Observer) SetClock(now func() time.Time) { o.now = now }
