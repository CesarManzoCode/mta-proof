//go:build mut_retry_permanent

package engine

import (
	"testing"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

func TestMutantRetryPermanent(t *testing.T) {
	if Mutant != "mut_retry_permanent" {
		t.Fatalf("Mutant %q", Mutant)
	}
	// The 550 is retried on the schedule until the lifetime runs out.
	if state, n := permanentRefusal(t); state != store.StateExpired || n != 9 {
		t.Fatalf("permanent refusal: %s after %d attempts, want expired after 9", state, n)
	}
}
