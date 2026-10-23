// Command mtasim is the mailbox provider simulator: the lab DNS, one SMTP
// listener per profile host and a Unix control socket.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/CesarManzoCode/mta-proof/internal/sim"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: mtasim serve [flags]")
		os.Exit(2)
	}
	if err := serve(os.Args[2:]); err != nil {
		log.Fatalf("mtasim: %v", err)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	profilesDir := fs.String("profiles", "profiles", "directory of profile JSON files")
	zonePath := fs.String("zone", "lab/zone.json", "lab DNS zone file")
	research := fs.String("research", "vault/RESEARCH.md", "RESEARCH file used to check profile sources (empty: syntax only)")
	out := fs.String("out", "", "run directory for receiver.jsonl and messages/ (required)")
	dnsAddr := fs.String("dns", "127.0.0.1:53", "UDP and TCP address of the lab DNS (empty: no DNS)")
	smtpPort := fs.Int("smtp-port", 25, "SMTP port on every MX host address")
	control := fs.String("control", "", "Unix control socket (default <out>/control.sock)")
	noSync := fs.Bool("no-sync", false, "skip the fsync after each data_end")
	fs.Parse(args)
	if *out == "" {
		return errors.New("-out is required")
	}
	if *control == "" {
		*control = filepath.Join(*out, "control.sock")
	}

	var idx sim.SourceIndex
	if *research != "" {
		var err error
		if idx, err = sim.LoadResearch(*research); err != nil {
			return err
		}
	}
	profiles, err := sim.LoadProfiles(*profilesDir, idx)
	if err != nil {
		return err
	}
	zone, err := sim.LoadZone(*zonePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	recv, err := sim.NewReceiver(sim.Config{Dir: *out, Profiles: profiles, NoSync: *noSync})
	if err != nil {
		return err
	}
	defer recv.Close()
	srv := sim.NewServer(recv)
	defer srv.Close()

	if *dnsAddr != "" {
		d, err := sim.ServeDNSZone(zone, *dnsAddr)
		if err != nil {
			return fmt.Errorf("dns: %w", err)
		}
		defer d.Shutdown()
		log.Printf("dns %s", d.Addr())
	}
	for _, host := range recv.Hosts() {
		addrs := zone.Addrs(host)
		if len(addrs) == 0 {
			return fmt.Errorf("host %s (profile %s) has no A record in %s", host, recv.ProfileOf(host), *zonePath)
		}
		for _, a := range addrs {
			addr := net.JoinHostPort(a.String(), strconv.Itoa(*smtpPort))
			if _, err := srv.ListenSMTP(host, addr); err != nil {
				return fmt.Errorf("smtp %s: %w", host, err)
			}
			log.Printf("smtp %s %s profile=%s", host, addr, recv.ProfileOf(host))
		}
	}
	if _, err := srv.ServeControl(*control); err != nil {
		return fmt.Errorf("control: %w", err)
	}
	defer os.Remove(*control)
	log.Printf("control %s", *control)
	// The ready line lets the orchestrator start the engine.
	fmt.Println("ready")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	return nil
}
