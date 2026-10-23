// Command mtaengine is the outbound delivery engine: the ingest API in
// front of the durable queue in engine.db.
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

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: mtaengine serve [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := fs.String("db", "", "engine.db path (required)")
	httpAddr := fs.String("http", "127.0.0.1:8025", "ingest API address")
	seed := fs.Uint64("seed", 1, "run seed for the engine's random streams")
	maxBatch := fs.Int("max-batch", 256, "maximum operations per group commit")
	fs.Parse(os.Args[2:])
	if *db == "" {
		log.Fatal("mtaengine: -db is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := engine.Serve(ctx, engine.ServeConfig{
		DB: *db, HTTPAddr: *httpAddr, Seed: *seed, MaxBatch: *maxBatch,
		Ready: func(a net.Addr) {
			log.Printf("http %s", a)
			fmt.Println("ready")
		},
	})
	if err != nil {
		log.Fatalf("mtaengine: %v", err)
	}
}
