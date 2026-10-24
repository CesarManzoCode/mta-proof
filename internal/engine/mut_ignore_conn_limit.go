//go:build mut_ignore_conn_limit

package engine

// Mutant mut_ignore_conn_limit (contract 4.3): the learned
// transactions-per-connection limit is recorded but never applied.
const Mutant = "mut_ignore_conn_limit"

func learnedMsgsPerConn(x scope) int { return 0 }
