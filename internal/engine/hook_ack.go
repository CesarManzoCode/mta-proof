//go:build !mut_ack_before_persist

package engine

// ackEarly lets an operation reply before its group is committed; the
// engine never does, so a 202 always follows the commit (contract 2.4).
func ackEarly(o *op, v any) bool { return false }
