//go:build !mut_no_backoff && !mut_retry_permanent && !mut_ignore_conn_limit && !mut_ack_before_persist && !mut_shared_queue && !mut_exceed_warmup

package engine

// Mutant names the seeded fault this binary was built with (contract 4.3):
// empty in the original engine. Each mutant is one build tag; its file
// defines Mutant and replaces one hook of a hook_*.go file, so a binary
// built without tags holds none of the mutants' code.
const Mutant = ""
