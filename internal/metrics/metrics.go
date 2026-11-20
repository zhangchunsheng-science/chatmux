// Package metrics holds process-global counters surfaced on /metrics.
// Atomics only: no locks, safe to bump from any goroutine.
//
// These count events the usage log can't show (they are transport
// health, not model calls): turns killed by the budget watchdog and
// model streams retried after a transient start failure.
package metrics

import "sync/atomic"

var (
	watchdogFires atomic.Int64
	streamRetries atomic.Int64
)

// WatchdogFire records one turn cancelled by the turn-budget watchdog.
func WatchdogFire() { watchdogFires.Add(1) }

// StreamRetry records one model-stream start retried after a transient
// failure (the single retry added around streamWithRetry).
func StreamRetry() { streamRetries.Add(1) }

// Snapshot returns (watchdog fires, stream retries) since process start.
func Snapshot() (watchdog, retries int64) {
	return watchdogFires.Load(), streamRetries.Load()
}
