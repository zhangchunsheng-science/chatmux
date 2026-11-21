// Package metrics holds process-global counters surfaced on /metrics.
// Atomics only: no locks, safe to bump from any goroutine.
//
// These count events the usage log can't show (they are transport
// health, not model calls): turns killed by the budget watchdog,
// model streams retried after a transient start failure, channel
// adapters bouncing in and out of connection, turns refused by the
// per-chat rate window, and turns re-routed to a fallback agent.
package metrics

import "sync/atomic"

var (
	watchdogFires    atomic.Int64
	streamRetries    atomic.Int64
	channelRestarts  atomic.Int64
	turnsRateLimited atomic.Int64
	turnFallbacks    atomic.Int64
	busDrops         atomic.Int64
)

// WatchdogFire records one turn cancelled by the turn-budget watchdog.
func WatchdogFire() { watchdogFires.Add(1) }

// StreamRetry records one model-stream start retried after a transient
// failure (the single retry added around streamWithRetry).
func StreamRetry() { streamRetries.Add(1) }

// ChannelRestart records one channel adapter exiting with an error and
// heading into backoff — a climbing rate is credential rot or a flappy
// upstream, not load.
func ChannelRestart() { channelRestarts.Add(1) }

// TurnRateLimited records one model turn refused by the per-chat
// turn-rate window (flood control). Climbing = a chat is hammering
// the bot — check who before the API-key bill does.
func TurnRateLimited() { turnsRateLimited.Add(1) }

// RateLimitedTurns returns turns refused by the rate window so far.
func RateLimitedTurns() int64 { return turnsRateLimited.Load() }

// TurnFallback records one turn re-routed to its fallbackAgent after the
// primary failed at stream start. Any steady rate means the primary's
// gateway/key is down — the bot keeps answering on the backup, so nothing
// else will tell you.
func TurnFallback() { turnFallbacks.Add(1) }

// TurnFallbacks returns turns re-routed to a fallback agent so far.
func TurnFallbacks() int64 { return turnFallbacks.Load() }

// BusDrop records one inbound message dropped because a subscriber's
// buffer was full. Steady rate = the router (or a second subscriber)
// can't keep up and user messages are vanishing — the bus drops rather
// than blocking a channel read loop, so nothing else will tell you.
func BusDrop() { busDrops.Add(1) }

// BusDrops returns messages dropped from a full subscriber buffer.
func BusDrops() int64 { return busDrops.Load() }

// Snapshot returns (watchdog fires, stream retries, channel restarts)
// since process start.
func Snapshot() (watchdog, retries, restarts int64) {
	return watchdogFires.Load(), streamRetries.Load(), channelRestarts.Load()
}
