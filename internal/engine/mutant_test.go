//go:build !mut_no_backoff && !mut_retry_permanent && !mut_ignore_conn_limit && !mut_ack_before_persist && !mut_shared_queue && !mut_exceed_warmup

package engine

import (
	"slices"
	"testing"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// TestMutantNone checks the original engine on the scenarios of the
// mutants: what each mut_*_test.go shows its mutant breaks.
func TestMutantNone(t *testing.T) {
	if Mutant != "" {
		t.Fatalf("Mutant %q without a mutant tag", Mutant)
	}
	t.Run("backoff", func(t *testing.T) {
		want := []int64{0, 60_000, 180_000, 420_000, 900_000, 1_500_000, 2_100_000, 2_700_000, 3_300_000}
		if got := backoffStarts(t); !slices.Equal(got, want) {
			t.Fatalf("attempts at %v, want %v", got, want)
		}
	})
	t.Run("permanent", func(t *testing.T) {
		if state, n := permanentRefusal(t); state != store.StateBounced || n != 1 {
			t.Fatalf("permanent refusal: %s after %d attempts, want bounced after 1", state, n)
		}
	})
	t.Run("conn_limit", func(t *testing.T) {
		if n := silentCloses(t); n != 1 {
			t.Fatalf("%d silent closes, want 1: the learned limit avoids the others", n)
		}
	})
	t.Run("ack", func(t *testing.T) {
		if returned, _ := ackBeforeCommit(t); returned {
			t.Fatal("Ingest returned before its group was committed")
		}
	})
	t.Run("queue", func(t *testing.T) {
		if got := fastDeliveries(t); !slices.Equal(got, []int64{0, 0, 0, 0, 0}) {
			t.Fatalf("fast.test delivered at %v: it waited for slow.test", got)
		}
	})
	t.Run("warmup", func(t *testing.T) {
		if n := firstHourStarts(t); n != 10 {
			t.Fatalf("%d attempts in the first hour, hourly cap 10", n)
		}
	})
}
