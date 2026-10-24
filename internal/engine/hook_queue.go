//go:build !mut_shared_queue

package engine

// queueOf is the queue that holds the messages of destination k: its own.
func queueOf(k queueKey) queueKey { return k }
