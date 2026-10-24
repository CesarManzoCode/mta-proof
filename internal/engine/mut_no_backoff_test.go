//go:build mut_no_backoff

package engine

import "testing"

func TestMutantNoBackoff(t *testing.T) {
	if Mutant != "mut_no_backoff" {
		t.Fatalf("Mutant %q", Mutant)
	}
	starts := backoffStarts(t)
	// Every retry waits the first delay, 60 s, until the 3600 s lifetime.
	if len(starts) != 60 {
		t.Fatalf("%d attempts, want 60 at a constant 60 s", len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if d := starts[i] - starts[i-1]; d != 60_000 {
			t.Fatalf("interval %d is %d ms, want the constant 60000", i, d)
		}
	}
}
