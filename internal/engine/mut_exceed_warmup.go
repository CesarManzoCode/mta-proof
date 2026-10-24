//go:build mut_exceed_warmup

package engine

// Mutant mut_exceed_warmup (contract 4.3): the hourly cap of the warm-up
// plan is ignored; the daily one still applies.
const Mutant = "mut_exceed_warmup"

func hourlyCap(h int) int { return 0 }
