//go:build mut_shared_queue

package engine

import "testing"

func TestMutantSharedQueue(t *testing.T) {
	if Mutant != "mut_shared_queue" {
		t.Fatalf("Mutant %q", Mutant)
	}
	// fast.test waits behind the slow.test messages ingested before it.
	at := fastDeliveries(t)
	if at[len(at)-1] < 4*60_000 {
		t.Fatalf("fast.test delivered at %v: it did not wait for slow.test", at)
	}
}
