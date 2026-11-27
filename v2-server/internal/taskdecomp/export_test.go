package taskdecomp

import "time"

// SetPollIntervalForTest shortens the watcher poll, which also paces the retried withdrawal of a
// refused start's run, so tests do not wait the production interval.
func SetPollIntervalForTest(h *Host, d time.Duration) { h.poll = d }
