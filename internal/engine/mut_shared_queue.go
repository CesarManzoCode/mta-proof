//go:build mut_shared_queue

package engine

// Mutant mut_shared_queue (contract 4.3): every destination shares one
// queue, so a blocked head holds up all the others.
const Mutant = "mut_shared_queue"

func queueOf(k queueKey) queueKey { return queueKey{} }
