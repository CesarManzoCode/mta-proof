package dst

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/engine"
)

// Scenario is lab/scenarios/<name>.json. Paths are relative to the
// repository root given to Run. Unknown fields are rejected.
type Scenario struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Engine is the engine configuration: a base file and fields that
	// replace or extend it.
	Engine EngineSpec `json:"engine"`
	// Zone is the lab zone (default lab/zone.json).
	Zone string `json:"zone"`
	// ProfilesDir holds the profiles (default profiles); Profiles names
	// the ones the receiver serves. Hosts of other profiles refuse
	// connections.
	ProfilesDir string   `json:"profiles_dir"`
	Profiles    []string `json:"profiles"`
	Tenants     []Tenant `json:"tenants"`
	Domains     []Domain `json:"domains"`
	// Messages is how many messages arrive.
	Messages int     `json:"messages"`
	Arrival  Arrival `json:"arrival"`
	// DurationS is the virtual time the run lasts; the run ends earlier
	// when nothing is left to happen.
	DurationS float64 `json:"duration_s"`
	// Variants are other runs of the same scenario, for the comparisons of
	// equivalent runs (D19): each one is merged over the scenario like
	// Engine.Set over its base. They may not change name or variants.
	Variants map[string]map[string]any `json:"variants"`
	// Variant is the variant this scenario was built as; "" for the base.
	Variant string `json:"-"`
}

// EngineSpec builds the engine configuration from Base with Set merged on
// top: objects merge key by key, any other value replaces the base value.
type EngineSpec struct {
	Base string         `json:"base"`
	Set  map[string]any `json:"set"`
}

// Tenant is a sender: each message of the tenant comes from From. With
// Domains, the tenant's messages go to those instead of the scenario's.
type Tenant struct {
	Name    string   `json:"name"`
	From    string   `json:"from"`
	MsgType string   `json:"msg_type"`
	Weight  int      `json:"weight"`
	Domains []Domain `json:"domains"`
}

// Domain is a destination. Recipients are <local_prefix><n>@<domain>: with
// Recipients > 0, n is drawn from [0, Recipients), so addresses repeat;
// with 0, n is the message number and every recipient is new.
type Domain struct {
	Domain      string `json:"domain"`
	LocalPrefix string `json:"local_prefix"`
	Recipients  int    `json:"recipients"`
	Weight      int    `json:"weight"`
}

// Arrival is the seeded arrival process. "poisson": exponential gaps of
// mean 1/rate_per_s seconds, the first one after start_s.
type Arrival struct {
	Process  string  `json:"process"`
	RatePerS float64 `json:"rate_per_s"`
	StartS   float64 `json:"start_s"`
}

// LoadScenario reads and validates a scenario file and returns it as
// variant ("" for the base scenario).
func LoadScenario(path, variant string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sc, err := ParseScenario(data)
	if err == nil && variant != "" {
		sc, err = sc.BuildVariant(data, variant)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return sc, nil
}

// ParseScenario decodes and validates a scenario and every variant of it,
// and fills defaults.
func ParseScenario(data []byte) (*Scenario, error) {
	sc, err := parseScenario(data)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sc.Variants))
	for name := range sc.Variants {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := sc.BuildVariant(data, name); err != nil {
			return nil, err
		}
	}
	return sc, nil
}

// BuildVariant returns variant name of the scenario whose file is data:
// the variant merged over the file without its variants.
func (sc *Scenario) BuildVariant(data []byte, name string) (*Scenario, error) {
	v, ok := sc.Variants[name]
	if !ok {
		return nil, fmt.Errorf("unknown variant %q", name)
	}
	if _, ok := v["name"]; ok {
		return nil, fmt.Errorf("variant %s: may not change name", name)
	}
	if _, ok := v["variants"]; ok {
		return nil, fmt.Errorf("variant %s: may not hold variants", name)
	}
	var base map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&base); err != nil {
		return nil, err
	}
	delete(base, "variants")
	merged, err := json.Marshal(merge(base, v))
	if err != nil {
		return nil, err
	}
	out, err := parseScenario(merged)
	if err != nil {
		return nil, fmt.Errorf("variant %s: %w", name, err)
	}
	out.Variant = name
	return out, nil
}

func parseScenario(data []byte) (*Scenario, error) {
	var sc Scenario
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&sc); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the scenario object")
	}
	return &sc, sc.validate()
}

func (sc *Scenario) validate() error {
	if sc.Name == "" {
		return errors.New("name is required")
	}
	if sc.Engine.Base == "" {
		sc.Engine.Base = "lab/engine.json"
	}
	if sc.Zone == "" {
		sc.Zone = "lab/zone.json"
	}
	if sc.ProfilesDir == "" {
		sc.ProfilesDir = "profiles"
	}
	if len(sc.Profiles) == 0 {
		return errors.New("profiles: at least one profile")
	}
	if len(sc.Tenants) == 0 || len(sc.Domains) == 0 {
		return errors.New("tenants and domains: at least one of each")
	}
	seen := map[string]bool{}
	for i := range sc.Tenants {
		t := &sc.Tenants[i]
		if t.MsgType == "" {
			t.MsgType = engine.TypeTransactional
		}
		if t.Weight == 0 {
			t.Weight = 1
		}
		if seen[t.Name] || t.Weight < 0 {
			return fmt.Errorf("tenants: %q is repeated or has a negative weight", t.Name)
		}
		seen[t.Name] = true
		probe := engine.Submission{Tenant: t.Name, From: t.From, To: "probe@example.test", Subject: "probe", MsgType: t.MsgType}
		if err := probe.Validate(); err != nil {
			return fmt.Errorf("tenants: %s: %w", t.Name, err)
		}
		if err := validateDomains(t.Domains); err != nil {
			return fmt.Errorf("tenants: %s: %w", t.Name, err)
		}
	}
	if err := validateDomains(sc.Domains); err != nil {
		return err
	}
	if sc.Messages < 1 {
		return errors.New("messages must be at least 1")
	}
	if sc.Arrival.Process != "poisson" || !(sc.Arrival.RatePerS > 0) || sc.Arrival.StartS < 0 {
		return errors.New(`arrival: process "poisson" with rate_per_s > 0 and start_s >= 0`)
	}
	if !(sc.DurationS > 0) {
		return errors.New("duration_s must be positive")
	}
	return nil
}

func validateDomains(ds []Domain) error {
	for i := range ds {
		d := &ds[i]
		if d.LocalPrefix == "" {
			d.LocalPrefix = "user"
		}
		if d.Weight == 0 {
			d.Weight = 1
		}
		if d.Domain == "" || d.Weight < 0 || d.Recipients < 0 {
			return fmt.Errorf("domains: %q needs a name and non-negative weight and recipients", d.Domain)
		}
		probe := engine.Submission{Tenant: "probe", From: "probe@example.test", To: d.LocalPrefix + "0@" + d.Domain,
			Subject: "probe", MsgType: engine.TypeTransactional}
		if err := probe.Validate(); err != nil {
			return fmt.Errorf("domains: %s: %w", d.Domain, err)
		}
	}
	return nil
}

// EngineConfig loads the base configuration, merges Set and validates the
// result with the engine's own loader.
func (sc *Scenario) EngineConfig(root string) (*engine.Config, error) {
	data, err := os.ReadFile(filepath.Join(root, sc.Engine.Base))
	if err != nil {
		return nil, err
	}
	var base map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&base); err != nil {
		return nil, fmt.Errorf("%s: %w", sc.Engine.Base, err)
	}
	merged, err := json.Marshal(merge(base, sc.Engine.Set))
	if err != nil {
		return nil, err
	}
	cfg, err := engine.ParseConfig(merged)
	if err != nil {
		return nil, fmt.Errorf("engine configuration of scenario %s: %w", sc.Name, err)
	}
	return cfg, nil
}

func merge(base, set map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range set {
		bm, ok1 := out[k].(map[string]any)
		sm, ok2 := v.(map[string]any)
		if ok1 && ok2 {
			out[k] = merge(bm, sm)
		} else {
			out[k] = v
		}
	}
	return out
}

// ArrivalAt is one message of the scenario and its arrival instant.
type ArrivalAt struct {
	AtMs int64
	Sub  engine.Submission
}

// Arrivals draws the arrivals of the scenario from the dst.arrivals stream
// of seed. Per message, in this order: the gap, the tenant, the domain
// (among the tenant's own domains, if it has them) and, when the domain has
// a recipient pool, the recipient.
func (sc *Scenario) Arrivals(seed uint64) []ArrivalAt {
	rng := clock.Rand(seed, "dst.arrivals")
	tw := 0
	for _, t := range sc.Tenants {
		tw += t.Weight
	}
	weights := func(ds []Domain) int {
		w := 0
		for _, d := range ds {
			w += d.Weight
		}
		return w
	}
	pick := func(total int, weight func(int) int) int {
		r := rng.IntN(total)
		for i := 0; ; i++ {
			if r < weight(i) {
				return i
			}
			r -= weight(i)
		}
	}
	out := make([]ArrivalAt, 0, sc.Messages)
	t := sc.Arrival.StartS
	for i := 0; i < sc.Messages; i++ {
		t += rng.ExpFloat64() / sc.Arrival.RatePerS
		ten := sc.Tenants[pick(tw, func(j int) int { return sc.Tenants[j].Weight })]
		ds := sc.Domains
		if len(ten.Domains) > 0 {
			ds = ten.Domains
		}
		dom := ds[pick(weights(ds), func(j int) int { return ds[j].Weight })]
		n := i
		if dom.Recipients > 0 {
			n = rng.IntN(dom.Recipients)
		}
		out = append(out, ArrivalAt{
			AtMs: int64(math.Round(t * 1000)),
			Sub: engine.Submission{
				Tenant:  ten.Name,
				From:    ten.From,
				To:      fmt.Sprintf("%s%d@%s", dom.LocalPrefix, n, dom.Domain),
				Subject: fmt.Sprintf("Scenario %s message %d", sc.Name, i+1),
				Text:    fmt.Sprintf("Message %d of scenario %s.\n", i+1, sc.Name),
				MsgType: ten.MsgType,
			},
		})
	}
	return out
}

func (sc *Scenario) durationMs() int64 { return int64(math.Round(sc.DurationS * 1000)) }
