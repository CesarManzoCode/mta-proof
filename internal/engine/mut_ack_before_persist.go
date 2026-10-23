//go:build mut_ack_before_persist

package engine

// Mutant mut_ack_before_persist (contract 4.3): an ingest replies as soon
// as the core admitted it, before its group is committed, so the API
// answers 202 for a message a crash can still lose.
const Mutant = "mut_ack_before_persist"

func ackEarly(o *op, v any) bool {
	if !o.ingest {
		return false
	}
	o.reply <- result{v: v}
	return true
}
