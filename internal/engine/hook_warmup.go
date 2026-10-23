//go:build !mut_exceed_warmup

package engine

// hourlyCap is the hourly warm-up cap the engine applies, 0 for none.
func hourlyCap(h int) int { return h }
