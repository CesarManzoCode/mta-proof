//go:build mut_retry_permanent

package engine

import "github.com/CesarManzoCode/mta-proof/internal/classify"

// Mutant mut_retry_permanent (contract 4.3): a 5xx refusal is deferred and
// retried like a transient one, whatever the classifier decided.
const Mutant = "mut_retry_permanent"

func bounces(d classify.Decision, rep Reply) bool {
	return d.Class == classify.Permanent && rep.Code/100 != 5
}
