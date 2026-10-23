//go:build !mut_no_backoff

package engine

// retryDelayMs is the wait after the k-th counted deferral (contract 5).
func retryDelayMs(p RetryPolicy, k int) int64 { return p.DelayMs(k) }
