//go:build !mut_ignore_conn_limit

package engine

// learnedMsgsPerConn is the transactions-per-connection limit the engine
// learned for scope x, 0 for none.
func learnedMsgsPerConn(x scope) int {
	return x.learnedField(func(l *learnedLimits) int { return l.msgs })
}
