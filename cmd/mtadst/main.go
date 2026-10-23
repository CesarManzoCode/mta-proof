// Command mtadst is the deterministic simulation: the engine core and the
// simulator's session machine in one process on a virtual clock. The same
// scenario and seed give the same trace, byte for byte.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/CesarManzoCode/mta-proof/internal/dst"
)

const usage = `usage:
  mtadst run --scenario <file> [--variant <name>] --seed <n> --out <dir> [--root .] [--research vault/RESEARCH.md]
  mtadst classify --corpus corpus/classification.json --out <file>`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		scenario := fs.String("scenario", "", "scenario file, like lab/scenarios/backoff.json (required)")
		variant := fs.String("variant", "", "variant of the scenario to run (default: the scenario itself)")
		seed := fs.Uint64("seed", 1, "run seed: engine streams and arrivals")
		out := fs.String("out", "", "run directory, new or empty (required)")
		root := fs.String("root", ".", "repository root; scenario paths are relative to it")
		research := fs.String("research", "vault/RESEARCH.md", "RESEARCH file used to check profile sources (empty: syntax only)")
		fs.Parse(os.Args[2:])
		if *scenario == "" || *out == "" {
			log.Fatal("mtadst: --scenario and --out are required")
		}
		sc, err := dst.LoadScenario(*scenario, *variant)
		if err != nil {
			log.Fatalf("mtadst: %v", err)
		}
		sum, err := dst.Run(dst.Options{Root: *root, Research: *research, Scenario: sc, Seed: *seed, Out: *out})
		if err != nil {
			log.Fatalf("mtadst: %v", err)
		}
		b, _ := json.Marshal(sum)
		fmt.Println(string(b))
	case "classify":
		fs := flag.NewFlagSet("classify", flag.ExitOnError)
		corpus := fs.String("corpus", "corpus/classification.json", "classification corpus")
		out := fs.String("out", "", "decisions JSONL (required)")
		fs.Parse(os.Args[2:])
		if *out == "" {
			log.Fatal("mtadst: --out is required")
		}
		n, err := dst.ClassifyCorpus(*corpus, *out)
		if err != nil {
			log.Fatalf("mtadst: %v", err)
		}
		log.Printf("classified %d cases into %s", n, *out)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}
