//go:build !mut_retry_permanent

package engine

import "github.com/CesarManzoCode/mta-proof/internal/classify"

// bounces reports whether a classified refusal ends the message as bounced.
func bounces(d classify.Decision, rep Reply) bool { return d.Class == classify.Permanent }
