package mtaq

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

const usage = `usage:
  mtaq timeline --db <engine.db> (--msg <msg_id> | --all) [filters]
  mtaq agg --db <engine.db> --by <dim,...> [filters]
dimensions: tenant, domain, provider, mx_host, src_ip, msg_type, kind
filters: --tenant, --domain, --provider, --src-ip, --msg-type, --kind`

// Main runs the command line of 4.2 and returns the exit status: 0 on
// success, 1 when the database cannot be read or the message does not
// match, 2 on a usage error.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("mtaq "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "engine.db, opened read-only (required)")
	var f Filter
	fs.StringVar(&f.Tenant, "tenant", "", "only events of this tenant")
	fs.StringVar(&f.Domain, "domain", "", "only events of this recipient domain")
	fs.StringVar(&f.Provider, "provider", "", "only events of this provider")
	fs.StringVar(&f.SrcIP, "src-ip", "", "only events from this source address")
	fs.StringVar(&f.MsgType, "msg-type", "", "only events of this message type")
	fs.StringVar(&f.Kind, "kind", "", "only events of this kind")
	var msg, by *string
	var all *bool
	switch args[0] {
	case "timeline":
		msg = fs.String("msg", "", "msg_id of the message")
		all = fs.Bool("all", false, "one line per message")
	case "agg":
		by = fs.String("by", "", "comma-separated dimensions (required)")
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "mtaq: "+format+"\n", a...)
		return code
	}
	if fs.NArg() > 0 {
		return fail(2, "unexpected arguments %q", fs.Args())
	}
	if *dbPath == "" {
		return fail(2, "--db is required")
	}
	var dims []string
	switch {
	case args[0] == "timeline" && (*msg == "") == !*all:
		return fail(2, "timeline needs exactly one of --msg and --all")
	case args[0] == "agg":
		if *by == "" {
			return fail(2, "agg needs --by")
		}
		var err error
		if dims, err = ParseDims(*by); err != nil {
			return fail(2, "%v", err)
		}
	}
	db, err := Open(*dbPath)
	if err != nil {
		return fail(1, "%v", err)
	}
	defer db.Close()

	w := bufio.NewWriter(stdout)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // reply texts carry <domain>; keep them as the receiver sent them
	switch {
	case args[0] == "agg":
		rows, err := Agg(db, dims, f)
		if err != nil {
			return fail(1, "%v", err)
		}
		// One row per line inside the list, so a large result stays readable.
		var buf bytes.Buffer
		row := json.NewEncoder(&buf)
		row.SetEscapeHTML(false)
		sep := "[\n"
		for _, r := range rows {
			buf.Reset()
			row.Encode(r)
			w.WriteString(sep)
			w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
			sep = ",\n"
		}
		if len(rows) == 0 {
			w.WriteString("[")
		} else {
			w.WriteString("\n")
		}
		w.WriteString("]\n")
	case *all:
		if err := Timelines(db, f, func(t Timeline) error { return enc.Encode(t) }); err != nil {
			return fail(1, "%v", err)
		}
	default:
		t, err := TimelineOf(db, *msg, f)
		if err != nil {
			return fail(1, "%v", err)
		}
		enc.Encode(t)
	}
	if err := w.Flush(); err != nil {
		return fail(1, "%v", err)
	}
	return 0
}
