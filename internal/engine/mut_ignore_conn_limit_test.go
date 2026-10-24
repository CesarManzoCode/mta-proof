//go:build mut_ignore_conn_limit

package engine

import "testing"

func TestMutantIgnoreConnLimit(t *testing.T) {
	if Mutant != "mut_ignore_conn_limit" {
		t.Fatalf("Mutant %q", Mutant)
	}
	// Without the learned limit, every connection meets the close again.
	if n := silentCloses(t); n < 3 {
		t.Fatalf("%d silent closes, want one per connection", n)
	}
}
