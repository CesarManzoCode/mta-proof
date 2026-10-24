//go:build mut_no_backoff

package engine

// Mutant mut_no_backoff (contract 4.3): every retry waits the first,
// shortest delay of the schedule, so the interval never grows.
const Mutant = "mut_no_backoff"

func retryDelayMs(p RetryPolicy, k int) int64 { return p.DelayMs(1) }
