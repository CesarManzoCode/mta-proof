//go:build mut_exceed_warmup

package engine

import "testing"

func TestMutantExceedWarmup(t *testing.T) {
	if Mutant != "mut_exceed_warmup" {
		t.Fatalf("Mutant %q", Mutant)
	}
	// Only the daily cap of 100 is left: all 50 go in the first hour.
	if n := firstHourStarts(t); n != 50 {
		t.Fatalf("%d attempts in the first hour, want all 50 past the hourly cap 10", n)
	}
}
