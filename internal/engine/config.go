package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"strings"
	"time"
)

// Config is the engine configuration file (lab/engine.json, contract 5 and
// the delivery design of 2.6). Unknown fields are rejected.
type Config struct {
	HTTP       string      `json:"http"`        // ingest API address
	DNS        string      `json:"dns"`         // DNS server the engine queries; resolv.conf is never read
	SMTPPort   int         `json:"smtp_port"`   // port of every MX address
	Hostname   string      `json:"hostname"`    // EHLO name and Reporting-MTA of DSNs
	Postmaster string      `json:"postmaster"`  // From of DSNs [R-3464 §2]
	Seed       uint64      `json:"seed"`        // run seed for the core's random streams
	MaxBatch   int         `json:"max_batch"`   // operations per group commit
	SrcIPs     []string    `json:"src_ips"`     // source addresses, tried in order
	RouteTTLS  int64       `json:"route_ttl_s"` // how long a resolved route is reused
	IdleS      float64     `json:"idle_s"`      // idle connections are closed after this
	Timeouts   Timeouts    `json:"timeouts"`
	Retry      RetryPolicy `json:"retry_policy"`
	Providers  []Provider  `json:"providers"`
	Limits     Limits      `json:"limits"`
	Adapt      Adapt       `json:"adapt"`

	srcIPs []netip.Addr
}

// Timeouts bound the real transport's waits, in seconds.
type Timeouts struct {
	ConnectS float64 `json:"connect_s"`
	CommandS float64 `json:"command_s"`
	DataEndS float64 `json:"data_end_s"`
}

// RetryPolicy is the retry schedule of contract 5 [R-5321 §4.5.4.1]: the
// k-th counted deferral waits min(initial_s * factor^(k-1), max_s), and a
// message whose next attempt would fall after accepted + lifetime_s
// expires instead.
type RetryPolicy struct {
	InitialS  float64 `json:"initial_s"`
	Factor    float64 `json:"factor"`
	MaxS      float64 `json:"max_s"`
	LifetimeS float64 `json:"lifetime_s"`
}

// DelayMs is the wait after the k-th counted deferral, k from 1. It never
// decreases with k because factor >= 1 and the cap is fixed.
func (p RetryPolicy) DelayMs(k int) int64 {
	if k < 1 {
		k = 1
	}
	d := p.InitialS * math.Pow(p.Factor, float64(k-1))
	if d > p.MaxS || math.IsInf(d, 1) || math.IsNaN(d) {
		d = p.MaxS
	}
	return int64(math.Round(d * 1000))
}

// LifetimeMs is the maximum life of a message.
func (p RetryPolicy) LifetimeMs() int64 { return int64(math.Round(p.LifetimeS * 1000)) }

// Provider names the receiver a destination belongs to, assigned by the
// suffix of its most preferred MX host [R-RESEND-FORGE: per-receiver
// configuration]. Names are the simulator's profile names.
type Provider struct {
	Name       string   `json:"name"`
	MXSuffixes []string `json:"mx_suffixes"`
}

// Limit is one set of limits; zero means no limit.
type Limit struct {
	Connections int   `json:"connections"`   // open connections
	MsgsPerConn int   `json:"msgs_per_conn"` // transactions per connection
	Rate        *Rate `json:"rate"`          // attempts started per sliding window
}

// Rate is a sliding-window rate: at most Limit attempts per PerS seconds.
type Rate struct {
	Limit int     `json:"limit"`
	PerS  float64 `json:"per_s"`
}

// Limits holds the limits of each scope by key; "*" is the default of its
// scope. Domain limits count all connections and attempts to the domain;
// provider limits count them per (provider, source IP), because providers
// limit per sending IP; tenant limits count the tenant's; src_ip limits
// count everything from that address.
type Limits struct {
	Domain   map[string]Limit `json:"domain"`
	Provider map[string]Limit `json:"provider"`
	Tenant   map[string]Limit `json:"tenant"`
	SrcIP    map[string]Limit `json:"src_ip"`
}

// Adapt configures the learned limits.
type Adapt struct {
	// RateWindowS is the window of the learned rate: after a throttling
	// reply the learned rate is half the attempts started in the last
	// window (supuesto).
	RateWindowS float64 `json:"rate_window_s"`
}

func lookupLimit(m map[string]Limit, key string) Limit {
	if l, ok := m[key]; ok {
		return l
	}
	return m["*"]
}

// LoadConfig reads and validates a configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ParseConfig decodes and validates a configuration.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the configuration object")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks the configuration and fills the defaults of optional
// fields.
func (c *Config) Validate() error {
	if c.HTTP == "" {
		c.HTTP = "127.0.0.1:8025"
	}
	if c.DNS == "" {
		return errors.New("dns: the engine needs the address of its DNS server")
	}
	if _, err := netip.ParseAddrPort(c.DNS); err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	if c.SMTPPort == 0 {
		c.SMTPPort = 25
	}
	if c.SMTPPort < 1 || c.SMTPPort > 65535 {
		return fmt.Errorf("smtp_port %d", c.SMTPPort)
	}
	if c.Hostname == "" || strings.ContainsAny(c.Hostname, " \t\r\n@") {
		return errors.New("hostname must be a host name")
	}
	if err := checkAddr("postmaster", c.Postmaster); err != nil {
		return err
	}
	if c.MaxBatch == 0 {
		c.MaxBatch = 256
	}
	if len(c.SrcIPs) == 0 {
		return errors.New("src_ips: at least one source address")
	}
	c.srcIPs = nil
	for _, s := range c.SrcIPs {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() {
			return fmt.Errorf("src_ips: %q is not an IPv4 address", s)
		}
		c.srcIPs = append(c.srcIPs, a)
	}
	if c.RouteTTLS == 0 {
		c.RouteTTLS = 300
	}
	if c.RouteTTLS < 0 {
		return errors.New("route_ttl_s must be positive")
	}
	if c.IdleS == 0 {
		c.IdleS = 5
	}
	if c.IdleS < 0 {
		return errors.New("idle_s must be positive")
	}
	for name, v := range map[string]*float64{"connect_s": &c.Timeouts.ConnectS, "command_s": &c.Timeouts.CommandS,
		"data_end_s": &c.Timeouts.DataEndS} {
		if *v == 0 {
			*v = 60
		}
		if *v < 0 {
			return fmt.Errorf("timeouts.%s must be positive", name)
		}
	}
	r := c.Retry
	if r.InitialS <= 0 || r.Factor < 1 || r.MaxS < r.InitialS || r.LifetimeS <= 0 {
		// R-5321 §4.5.4.1: the parameters MUST be configurable; the
		// schedule must still be non-decreasing and bounded.
		return errors.New("retry_policy needs initial_s > 0, factor >= 1, max_s >= initial_s and lifetime_s > 0")
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" || seen[p.Name] || len(p.MXSuffixes) == 0 {
			return fmt.Errorf("providers: %q needs a unique name and mx_suffixes", p.Name)
		}
		seen[p.Name] = true
		for _, s := range p.MXSuffixes {
			if s == "" || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
				return fmt.Errorf("providers: %s: suffix %q must be a domain without leading or trailing dot", p.Name, s)
			}
		}
	}
	for scope, m := range map[string]map[string]Limit{"domain": c.Limits.Domain, "provider": c.Limits.Provider,
		"tenant": c.Limits.Tenant, "src_ip": c.Limits.SrcIP} {
		for key, l := range m {
			if l.Connections < 0 || l.MsgsPerConn < 0 {
				return fmt.Errorf("limits.%s.%s: negative limit", scope, key)
			}
			if l.Rate != nil && (l.Rate.Limit < 1 || l.Rate.PerS <= 0) {
				return fmt.Errorf("limits.%s.%s: rate needs limit >= 1 and per_s > 0", scope, key)
			}
		}
	}
	if c.Adapt.RateWindowS == 0 {
		c.Adapt.RateWindowS = 60
	}
	if c.Adapt.RateWindowS < 0 {
		return errors.New("adapt.rate_window_s must be positive")
	}
	return nil
}

// providerOf returns the provider whose suffix matches host, or "".
// Providers are checked in file order.
func (c *Config) providerOf(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range c.Providers {
		for _, s := range p.MXSuffixes {
			s = strings.ToLower(s)
			if host == s || strings.HasSuffix(host, "."+s) {
				return p.Name
			}
		}
	}
	return ""
}

func secondsMs(s float64) int64 { return int64(math.Round(s * 1000)) }

func secondsDur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// maxWindowMs is the longest sliding window any limit uses, so attempt
// start times older than it can be forgotten.
func (c *Config) maxWindowMs() int64 {
	w := secondsMs(c.Adapt.RateWindowS)
	for _, m := range []map[string]Limit{c.Limits.Domain, c.Limits.Provider, c.Limits.Tenant, c.Limits.SrcIP} {
		for _, l := range m {
			if l.Rate != nil && secondsMs(l.Rate.PerS) > w {
				w = secondsMs(l.Rate.PerS)
			}
		}
	}
	return w
}
