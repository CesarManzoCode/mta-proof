// Command mtaq is the debugging tool over the engine's events log
// (contract 4.2): mtaq timeline prints the causal history of a message and
// mtaq agg counts events by dimension. It opens engine.db read-only.
package main

import (
	"os"

	"github.com/CesarManzoCode/mta-proof/internal/mtaq"
)

func main() { os.Exit(mtaq.Main(os.Args[1:], os.Stdout, os.Stderr)) }
