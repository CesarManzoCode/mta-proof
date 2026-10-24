package sim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Profile is a simulator profile (contract 3.1).
type Profile struct {
	Name              string   `json:"name"`
	Sources           []string `json:"sources"`
	Limitations       string   `json:"limitations,omitempty"`
	AdaptationPeriodS int      `json:"adaptation_period_s"`
	Hosts             []string `json:"hosts"`
	Rules             []Rule   `json:"rules"`

	compiled []*compiledRule
}

// Rule is one declarative rule of a profile.
type Rule struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Params      json.RawMessage `json:"params"`
	OnViolation *OnViolation    `json:"on_violation,omitempty"`
	Source      string          `json:"source,omitempty"`
	Assumption  *bool           `json:"assumption"`
	Note        string          `json:"note,omitempty"`
}

// OnViolation is how the simulator answers a command above a limit.
type OnViolation struct {
	Action   string    `json:"action"`
	Response *Response `json:"response,omitempty"`
}

// Rule kinds (contract 3.1).
const (
	KindMaxMsgsPerConn = "max_msgs_per_conn"
	KindRate           = "rate"
	KindConcurrency    = "concurrency"
	KindUnknownIP      = "unknown_ip"
	KindRespond        = "respond"
	KindDropMidData    = "drop_mid_data"
	KindLatency        = "latency"
	KindAuth           = "auth"
)

// Violation actions.
const (
	ActionCloseSilent  = "close_silent"
	ActionRespond      = "respond"
	ActionRespondClose = "respond_close"
)

// Stages where respond and latency rules apply.
const (
	StageConnect = "connect"
	StageEHLO    = "ehlo"
	StageMail    = "mail"
	StageRcpt    = "rcpt"
	StageData    = "data"
	StageDataEnd = "data_end"
	StageAny     = "any"
)

// Rate scopes.
const (
	ScopeSrcIP   = "src_ip"
	ScopeProfile = "profile"
	ScopeRcpt    = "rcpt"
)

type maxMsgsParams struct {
	Limit int `json:"limit"`
}

type rateParams struct {
	Scope string `json:"scope"`
	Limit int    `json:"limit"`
	PerS  int    `json:"per_s"`
}

type concurrencyParams struct {
	Limit int `json:"limit"`
}

type unknownIPParams struct {
	KnownIPs       []string    `json:"known_ips"`
	MaxMsgsPerConn int         `json:"max_msgs_per_conn,omitempty"`
	Rate           *rateParams `json:"rate,omitempty"`
	Concurrency    int         `json:"concurrency,omitempty"`
}

// Match selects the transactions a respond rule applies to. Empty fields
// match everything; all non-empty fields must match.
type Match struct {
	RcptLocalPrefix string   `json:"rcpt_local_prefix,omitempty"`
	RcptDomain      string   `json:"rcpt_domain,omitempty"`
	MailFromDomain  string   `json:"mail_from_domain,omitempty"`
	SrcIPs          []string `json:"src_ips,omitempty"`
}

type respondParams struct {
	Stage    string   `json:"stage"`
	Match    Match    `json:"match"`
	Response Response `json:"response"`
	Close    bool     `json:"close"`
}

type dropParams struct {
	EveryN     int `json:"every_n"`
	AfterBytes int `json:"after_bytes"`
}

type latencyParams struct {
	Stage string `json:"stage"`
	Ms    int    `json:"ms"`
}

// authParams: every method in Require must pass; otherwise the reply to
// DATA is DMARCRejectResponse when DMARC failed under an effective "reject"
// policy and the profile has one, and FailResponse in any other case.
type authParams struct {
	Require             []string  `json:"require"`
	FailResponse        *Response `json:"fail_response,omitempty"`
	DMARCRejectResponse *Response `json:"dmarc_reject_response,omitempty"`
}

// compiledRule is a validated rule ready for the session machine. An
// unknown_ip rule expands into one compiled rule per limit, all carrying
// the unknown_ip rule id and its list of known prefixes.
type compiledRule struct {
	id          string
	kind        string
	onViolation *OnViolation
	limit       int
	rate        *rateParams
	known       []netip.Prefix
	unknownOnly bool
	respond     *respondParams
	srcIPs      []netip.Prefix
	drop        *dropParams
	latency     *latencyParams
	auth        *authParams
}

// appliesTo reports whether a limit rule applies to the source address.
func (c *compiledRule) appliesTo(src netip.Addr) bool {
	if !c.unknownOnly {
		return true
	}
	for _, p := range c.known {
		if p.Contains(src) {
			return false
		}
	}
	return true
}

// SourceIndex maps a RESEARCH id to whether the source is secondary.
type SourceIndex map[string]bool

var researchRow = regexp.MustCompile(`(?m)^\|\s*([RS]-[A-Z0-9-]+)\s*\|(.*)$`)

// ParseResearch extracts the source ids of vault/RESEARCH.md. A row whose
// type column says "secundaria" is a secondary source.
func ParseResearch(text string) SourceIndex {
	idx := SourceIndex{}
	for _, m := range researchRow.FindAllStringSubmatch(text, -1) {
		idx[m[1]] = strings.Contains(m[2], "| secundaria |")
	}
	return idx
}

// LoadResearch reads and parses a RESEARCH file.
func LoadResearch(path string) (SourceIndex, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseResearch(string(b)), nil
}

var (
	profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	sourceID    = regexp.MustCompile(`^[RS]-[A-Z0-9-]+$`)
)

// ParseProfile decodes and validates a profile. idx may be nil, in which
// case source ids are only checked for syntax.
func ParseProfile(data []byte, idx SourceIndex) (*Profile, error) {
	var p Profile
	if err := strictUnmarshal(data, &p); err != nil {
		return nil, err
	}
	if err := p.validate(idx); err != nil {
		return nil, fmt.Errorf("profile %q: %w", p.Name, err)
	}
	return &p, nil
}

// LoadProfile reads a profile file; its name must match the file name.
func LoadProfile(path string, idx SourceIndex) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := ParseProfile(b, idx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if want := strings.TrimSuffix(filepath.Base(path), ".json"); p.Name != want {
		return nil, fmt.Errorf("%s: name %q does not match file name", path, p.Name)
	}
	return p, nil
}

// LoadProfiles loads every *.json profile of a directory.
func LoadProfiles(dir string, idx SourceIndex) (map[string]*Profile, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no profiles in %s", dir)
	}
	sort.Strings(paths)
	out := map[string]*Profile{}
	for _, path := range paths {
		p, err := LoadProfile(path, idx)
		if err != nil {
			return nil, err
		}
		out[p.Name] = p
	}
	return out, nil
}

func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

func (p *Profile) validate(idx SourceIndex) error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !profileName.MatchString(p.Name) {
		fail("invalid name %q", p.Name)
	}
	if len(p.Sources) == 0 {
		fail("sources is empty")
	}
	secondary := false
	for _, s := range p.Sources {
		switch {
		case !sourceID.MatchString(s):
			fail("invalid source id %q", s)
		case idx != nil:
			sec, ok := idx[s]
			if !ok {
				fail("source %s is not in RESEARCH", s)
			}
			secondary = secondary || sec
		}
	}
	if p.AdaptationPeriodS < 0 {
		fail("adaptation_period_s is negative")
	}
	for _, h := range p.Hosts {
		if !isTestName(h) {
			fail("host %q is not a name under .test", h)
		}
	}

	ids := map[string]bool{}
	p.compiled = nil
	for i := range p.Rules {
		r := &p.Rules[i]
		where := fmt.Sprintf("rule %d (%s)", i, r.ID)
		if r.ID == "" {
			fail("%s: empty id", where)
		} else if ids[r.ID] {
			fail("%s: duplicate id", where)
		}
		ids[r.ID] = true
		if r.Assumption == nil {
			fail("%s: assumption is required", where)
		}
		if r.Source == "" {
			if r.Assumption == nil || !*r.Assumption {
				fail("%s: no source and not marked assumption: true", where)
			}
		} else if !slices.Contains(p.Sources, r.Source) {
			fail("%s: source %s is not listed in the profile sources", where, r.Source)
		}
		compiled, responses, err := compileRule(r)
		if err != nil {
			fail("%s: %v", where, err)
			continue
		}
		for _, resp := range responses {
			d, ok := lookupDocumented(resp)
			if !ok {
				fail("%s: response %q is not in contract table 3.2", where, resp.String())
				continue
			}
			if !slices.Contains(p.Sources, d.Source) {
				fail("%s: response %q comes from %s, which is not in the profile sources", where, resp.String(), d.Source)
			}
			secondary = secondary || d.Secondary
		}
		p.compiled = append(p.compiled, compiled...)
	}
	if secondary && strings.TrimSpace(p.Limitations) == "" {
		fail("uses secondary sources but limitations is empty")
	}
	return errors.Join(errs...)
}

var limitKinds = []string{KindMaxMsgsPerConn, KindRate, KindConcurrency, KindUnknownIP}

// compileRule checks the kind-specific params and returns the compiled
// rules and every response the rule can send.
func compileRule(r *Rule) ([]*compiledRule, []Response, error) {
	isLimit := slices.Contains(limitKinds, r.Kind)
	var responses []Response
	if isLimit {
		if r.OnViolation == nil {
			return nil, nil, errors.New("limit rule without on_violation")
		}
		ov := r.OnViolation
		switch ov.Action {
		case ActionCloseSilent:
			if ov.Response != nil {
				return nil, nil, errors.New("close_silent takes no response")
			}
		case ActionRespond, ActionRespondClose:
			if ov.Response == nil {
				return nil, nil, fmt.Errorf("%s needs a response", ov.Action)
			}
			// RFC 5321 §3.8: a 421 reply means the server closes the channel.
			if ov.Response.Code == 421 && ov.Action != ActionRespondClose {
				return nil, nil, errors.New("a 421 response must use respond_close (RFC 5321 §3.8)")
			}
			responses = append(responses, *ov.Response)
		default:
			return nil, nil, fmt.Errorf("unknown on_violation action %q", ov.Action)
		}
	} else if r.OnViolation != nil {
		return nil, nil, fmt.Errorf("%s rules take no on_violation", r.Kind)
	}
	if len(r.Params) == 0 {
		return nil, nil, errors.New("params is required")
	}

	base := compiledRule{id: r.ID, kind: r.Kind, onViolation: r.OnViolation}
	switch r.Kind {
	case KindMaxMsgsPerConn:
		var p maxMsgsParams
		if err := strictUnmarshal(r.Params, &p); err != nil {
			return nil, nil, err
		}
		if p.Limit < 1 {
			return nil, nil, errors.New("limit must be >= 1")
		}
		base.limit = p.Limit
		return []*compiledRule{&base}, responses, nil

	case KindRate:
		var p rateParams
		if err := strictUnmarshal(r.Params, &p); err != nil {
			return nil, nil, err
		}
		if err := p.check(true); err != nil {
			return nil, nil, err
		}
		base.rate = &p
		return []*compiledRule{&base}, responses, nil

	case KindConcurrency:
		var p concurrencyParams
		if err := strictUnmarshal(r.Params, &p); err != nil {
			return nil, nil, err
		}
		if p.Limit < 1 {
			return nil, nil, errors.New("limit must be >= 1")
		}
		if r.OnViolation.Action == ActionRespond {
			return nil, nil, errors.New("a connection-time violation must close (close_silent or respond_close)")
		}
		base.limit = p.Limit
		return []*compiledRule{&base}, responses, nil

	case KindUnknownIP:
		var p unknownIPParams
		if err := strictUnmarshal(r.Params, &p); err != nil {
			return nil, nil, err
		}
		known, err := parsePrefixes(p.KnownIPs)
		if err != nil {
			return nil, nil, err
		}
		var out []*compiledRule
		sub := func(kind string) *compiledRule {
			c := base
			c.kind, c.known, c.unknownOnly = kind, known, true
			out = append(out, &c)
			return &c
		}
		if p.MaxMsgsPerConn < 0 || p.Concurrency < 0 {
			return nil, nil, errors.New("limits must be positive")
		}
		if p.MaxMsgsPerConn > 0 {
			sub(KindMaxMsgsPerConn).limit = p.MaxMsgsPerConn
		}
		if p.Rate != nil {
			if err := p.Rate.check(false); err != nil {
				return nil, nil, err
			}
			rp := *p.Rate
			if rp.Scope == "" {
				rp.Scope = ScopeSrcIP
			}
			sub(KindRate).rate = &rp
		}
		if p.Concurrency > 0 {
			if r.OnViolation.Action == ActionRespond {
				return nil, nil, errors.New("a connection-time violation must close (close_silent or respond_close)")
			}
			sub(KindConcurrency).limit = p.Concurrency
		}
		if len(out) == 0 {
			return nil, nil, errors.New("unknown_ip declares no limit")
		}
		return out, responses, nil

	case KindRespond:
		var p respondParams
		if err := strictUnmarshal(r.Params, &p); err != nil {
			return nil, nil, err
		}
		switch p.Stage {
		case StageConnect, StageMail, StageRcpt, StageDataEnd:
		default:
			return nil, nil, fmt.Errorf("respond stage %q is not one of connect, mail, rcpt, data_end", p.Stage)
		}
		if p.Response.Code == 421 && !p.Close {
			return nil, nil, errors.New("a 421 response must close (RFC 5321 §3.8)")
		}
		if p.Response.Code < 400 {
			return nil, nil, errors.New("respond rules only send 4xx or 5xx replies")
		}
		srcs, err := parsePrefixes(p.Match.SrcIPs)
		if err != nil {
			return nil, nil, err
		}
		base.respond, base.srcIPs = &p, srcs
		return []*compiledRule{&base}, append(responses, p.Response), nil

	case KindDropMidData:
		var p dropParams
		if err := strictUnmarshal(r.Params, &p); err != nil {
			return nil, nil, err
		}
		if p.EveryN == 0 {
			p.EveryN = 1
		}
		if p.EveryN < 1 || p.AfterBytes < 0 {
			return nil, nil, errors.New("every_n must be >= 1 and after_bytes >= 0")
		}
		base.drop = &p
		return []*compiledRule{&base}, responses, nil

	case KindLatency:
		var p latencyParams
		if err := strictUnmarshal(r.Params, &p); err != nil {
			return nil, nil, err
		}
		switch p.Stage {
		case StageConnect, StageEHLO, StageMail, StageRcpt, StageData, StageDataEnd, StageAny:
		default:
			return nil, nil, fmt.Errorf("unknown latency stage %q", p.Stage)
		}
		if p.Ms < 0 {
			return nil, nil, errors.New("ms must be >= 0")
		}
		base.latency = &p
		return []*compiledRule{&base}, responses, nil

	case KindAuth:
		var p authParams
		if err := strictUnmarshal(r.Params, &p); err != nil {
			return nil, nil, err
		}
		if len(p.Require) == 0 {
			return nil, nil, errors.New("require lists no method")
		}
		for _, m := range p.Require {
			if m != "spf" && m != "dkim" && m != "dmarc" {
				return nil, nil, fmt.Errorf("unknown auth method %q", m)
			}
		}
		if p.FailResponse == nil {
			return nil, nil, errors.New("fail_response is required")
		}
		for _, resp := range []*Response{p.FailResponse, p.DMARCRejectResponse} {
			if resp != nil {
				if resp.Code < 500 {
					return nil, nil, errors.New("auth responses are 5xx")
				}
				responses = append(responses, *resp)
			}
		}
		base.auth = &p
		return []*compiledRule{&base}, responses, nil
	}
	return nil, nil, fmt.Errorf("unknown kind %q", r.Kind)
}

func (p rateParams) check(scopeRequired bool) error {
	switch p.Scope {
	case ScopeSrcIP, ScopeProfile, ScopeRcpt:
	case "":
		if scopeRequired {
			return errors.New("rate scope is required (src_ip, profile or rcpt)")
		}
	default:
		return fmt.Errorf("unknown rate scope %q", p.Scope)
	}
	if p.Limit < 1 || p.PerS < 1 {
		return errors.New("rate limit and per_s must be >= 1")
	}
	return nil
}

func parsePrefixes(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("invalid address or prefix %q", s)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// isTestName reports whether s is a host name under the .test TLD
// (RFC 6761 §6.2).
func isTestName(s string) bool { return testName(s, false) }

// isZoneName is isTestName that also admits labels starting with "_", as
// the DKIM key ("_domainkey") and DMARC ("_dmarc") names do.
func isZoneName(s string) bool { return testName(s, true) }

func testName(s string, underscore bool) bool {
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if !strings.HasSuffix(s, ".test") || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if underscore && len(label) > 1 && label[0] == '_' {
			label = label[1:]
		}
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
