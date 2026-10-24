// Command mtaengine is the outbound delivery engine: the ingest API in
// front of the durable queue in engine.db and, with run, the delivery
// that drains it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/CesarManzoCode/mta-proof/internal/engine"
)

const usage = `usage:
  mtaengine run --config <engine.json> --db <engine.db>
  mtaengine serve --db <engine.db> [--http addr] [--seed n] [--max-batch n]`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if engine.Mutant != "" {
		log.Printf("mutant %s (contract 4.3)", engine.Mutant)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ready := func(a net.Addr) {
		log.Printf("http %s", a)
		fmt.Println("ready")
	}
	var err error
	switch os.Args[1] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		cfgPath := fs.String("config", "", "engine configuration, like lab/engine.json (required)")
		db := fs.String("db", "", "engine.db path (required)")
		fs.Parse(os.Args[2:])
		if *cfgPath == "" || *db == "" {
			log.Fatal("mtaengine: --config and --db are required")
		}
		cfg, cerr := engine.LoadConfig(*cfgPath)
		if cerr != nil {
			log.Fatalf("mtaengine: %v", cerr)
		}
		err = engine.Run(ctx, engine.RunConfig{DB: *db, Config: cfg, Ready: ready})
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		db := fs.String("db", "", "engine.db path (required)")
		httpAddr := fs.String("http", "127.0.0.1:8025", "ingest API address")
		seed := fs.Uint64("seed", 1, "run seed for the engine's random streams")
		maxBatch := fs.Int("max-batch", 256, "maximum operations per group commit")
		fs.Parse(os.Args[2:])
		if *db == "" {
			log.Fatal("mtaengine: --db is required")
		}
		err = engine.Serve(ctx, engine.ServeConfig{DB: *db, HTTPAddr: *httpAddr, Seed: *seed, MaxBatch: *maxBatch, Ready: ready})
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("mtaengine: %v", err)
	}
}
