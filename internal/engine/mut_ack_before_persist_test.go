//go:build mut_ack_before_persist

package engine

import "testing"

func TestMutantAckBeforePersist(t *testing.T) {
	if Mutant != "mut_ack_before_persist" {
		t.Fatalf("Mutant %q", Mutant)
	}
	returned, persisted := ackBeforeCommit(t)
	if !returned || persisted {
		t.Fatalf("returned %v, persisted %v: want a reply for a message not yet in engine.db", returned, persisted)
	}
}
