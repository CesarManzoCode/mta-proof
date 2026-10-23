package classify

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"testing"
)

type corpusCase struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	Code            int    `json:"code"`
	Enhanced        string `json:"enhanced"`
	Text            string `json:"text"`
	ExpectedClass   string `json:"expected_class"`
	ExpectedSubtype string `json:"expected_subtype"`
	Source          string `json:"source"`
}

func loadCorpus(t *testing.T) []corpusCase {
	t.Helper()
	b, err := os.ReadFile("../../corpus/classification.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var cs []corpusCase
	if err := dec.Decode(&cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// researchIDs returns the identifiers that RESEARCH lists in the first
// column of its source tables.
func researchIDs(t *testing.T) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\| (R-[A-Z0-9-]+) \|`).FindAllStringSubmatch(readFile(t, "../../vault/RESEARCH.md"), -1) {
		ids[m[1]] = true
	}
	if len(ids) == 0 {
		t.Fatal("no source ids found in RESEARCH")
	}
	return ids
}

func TestCorpus(t *testing.T) {
	cs := loadCorpus(t)
	seen := map[string]bool{}
	for _, c := range cs {
		if seen[c.ID] {
			t.Errorf("repeated case id %s", c.ID)
		}
		seen[c.ID] = true
		d := Classify(Reply{Provider: c.Provider, Code: c.Code, Enhanced: c.Enhanced, Text: c.Text})
		if d.RuleID == "" {
			t.Errorf("%s: decision without rule_id", c.ID)
		}
		if d.Class != c.ExpectedClass || d.Subtype != c.ExpectedSubtype {
			t.Errorf("%s: got %s/%s (%s), want %s/%s", c.ID, d.Class, d.Subtype, d.RuleID,
				c.ExpectedClass, c.ExpectedSubtype)
		}
	}
	if !hasUndetermined(cs) {
		t.Error("corpus has no case that expects undetermined")
	}
}

func hasUndetermined(cs []corpusCase) bool {
	for _, c := range cs {
		if c.ExpectedClass == Undetermined {
			return true
		}
	}
	return false
}

func TestCorpusSources(t *testing.T) {
	ids := researchIDs(t)
	for _, c := range loadCorpus(t) {
		if !ids[c.Source] {
			t.Errorf("case %s: source %q is not a RESEARCH id", c.ID, c.Source)
		}
	}
	for _, r := range Default.Rules() {
		if !ids[r.Source] {
			t.Errorf("rule %s: source %q is not a RESEARCH id", r.ID, r.Source)
		}
	}
}

// table32 parses the rows of contract table 3.2 into the reply the corpus
// must hold: the tag, when the code column has one, starts the text.
func table32(t *testing.T) []corpusCase {
	t.Helper()
	contract := readFile(t, "../../vault/CONTRACT.md")
	start := strings.Index(contract, "### 3.2 ")
	end := strings.Index(contract, "### 3.3 ")
	if start < 0 || end < start {
		t.Fatal("contract section 3.2 not found")
	}
	row := regexp.MustCompile("(?m)^\\| `(\\d{3})( \\d\\.\\d{1,3}\\.\\d{1,3})?( \\[[A-Z0-9]+\\])?` \\| `([^`]+)` \\| (R-[A-Z0-9-]+)")
	var rows []corpusCase
	for _, m := range row.FindAllStringSubmatch(contract[start:end], -1) {
		var code int
		fmt.Sscan(m[1], &code)
		text := m[4]
		if tag := strings.TrimSpace(m[3]); tag != "" {
			text = tag + " " + text
		}
		rows = append(rows, corpusCase{Code: code, Enhanced: strings.TrimSpace(m[2]), Text: text, Source: m[5]})
	}
	if len(rows) != 12 {
		t.Fatalf("parsed %d rows of table 3.2, want 12", len(rows))
	}
	return rows
}

func TestCorpusHasEveryTable32Row(t *testing.T) {
	cs := loadCorpus(t)
	for _, r := range table32(t) {
		found := false
		for _, c := range cs {
			if c.Code == r.Code && c.Enhanced == r.Enhanced && c.Text == r.Text && c.Source == r.Source {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("corpus lacks table 3.2 row %d %s %q (%s)", r.Code, r.Enhanced, r.Text, r.Source)
		}
	}
}

func TestPrecedence(t *testing.T) {
	for _, tc := range []struct {
		in   Reply
		rule string
	}{
		// A provider rule wins over the enhanced code rule with the same code.
		{Reply{Code: 550, Enhanced: "5.1.1", Text: "The email account that you tried to reach does not exist. Learn more"}, "google.no_account"},
		{Reply{Code: 550, Enhanced: "5.1.1", Text: "No such user"}, "rfc3463.bad_mailbox"},
		// Provider and enhanced rules win over the class 4 rule.
		{Reply{Code: 421, Enhanced: "4.7.0", Text: "[TSS04] Messages from 127.0.2.1 temporarily deferred"}, "yahoo.tss04"},
		{Reply{Code: 452, Enhanced: "4.2.2", Text: "Over quota"}, "rfc3463.mailbox_full"},
		{Reply{Code: 451, Enhanced: "4.3.0", Text: "anything"}, "rfc3463.class4"},
		{Reply{Code: 421, Text: ""}, "rfc3463.class4"},
		// The enhanced class must agree with the reply code.
		{Reply{Code: 450, Enhanced: "5.1.1", Text: "No such user"}, "rfc3463.class4"},
		{Reply{Code: 550, Enhanced: "4.2.2", Text: "Mailbox full"}, NoRuleID},
		// The placeholder stands for one domain, not for any words.
		{Reply{Code: 550, Enhanced: "5.7.515", Text: "Access denied, sending domain a.test does not meet the required authentication level."}, "microsoft.auth_level"},
		{Reply{Code: 550, Enhanced: "5.7.515", Text: "Access denied, sending domain does not meet the required authentication level."}, NoRuleID},
		{Reply{Code: 550, Enhanced: "5.7.515", Text: "Access denied, sending domain a b does not meet the required authentication level."}, NoRuleID},
		// A known code with another text has no rule.
		{Reply{Code: 550, Enhanced: "5.7.26", Text: "Something else"}, NoRuleID},
		{Reply{Code: 250, Enhanced: "2.0.0", Text: "OK"}, NoRuleID},
	} {
		if d := Classify(tc.in); d.RuleID != tc.rule {
			t.Errorf("%+v: rule %s (%s/%s), want %s", tc.in, d.RuleID, d.Class, d.Subtype, tc.rule)
		}
	}
}

func TestProviderCondition(t *testing.T) {
	tab, err := NewTable([]Rule{{ID: "p.only", Tier: TierProvider, Source: "R-X", Provider: "gmail-model",
		Code: 550, Enhanced: "5.7.1", Class: Permanent, Subtype: ContentRejected}})
	if err != nil {
		t.Fatal(err)
	}
	in := Reply{Provider: "gmail-model", Code: 550, Enhanced: "5.7.1"}
	if d := tab.Classify(in); d.RuleID != "p.only" {
		t.Fatalf("same provider: %+v", d)
	}
	in.Provider = "yahoo-model"
	if d := tab.Classify(in); d.Class != Undetermined || d.RuleID != NoRuleID {
		t.Fatalf("other provider: %+v", d)
	}
}

func TestTableRejectsBadRules(t *testing.T) {
	ok := Rule{ID: "a", Tier: TierEnhanced, Source: "R-3463", Enhanced: "5.1.1", Class: Permanent, Subtype: NoEmail}
	for name, mutate := range map[string]func(*Rule){
		"no id":                  func(r *Rule) { r.ID = "" },
		"reserved id":            func(r *Rule) { r.ID = NoRuleID },
		"no source":              func(r *Rule) { r.Source = "" },
		"undetermined class":     func(r *Rule) { r.Class = Undetermined },
		"unknown subtype":        func(r *Rule) { r.Subtype = "Bounce" },
		"enhanced rule no code":  func(r *Rule) { r.Enhanced = ""; r.Text = "x" },
		"provider rule no match": func(r *Rule) { r.Tier = TierProvider; r.Enhanced = "" },
		"class 5 rule": func(r *Rule) {
			*r = Rule{ID: "c5", Tier: TierClass, Source: "R-3463", CodeClass: 5, Class: Permanent, Subtype: General}
		},
		"permanent class 4 rule": func(r *Rule) {
			*r = Rule{ID: "c4", Tier: TierClass, Source: "R-3463", CodeClass: 4, Class: Permanent, Subtype: General}
		},
		"permanent on any class":  func(r *Rule) { r.Enhanced = "X.1.1" },
		"placeholder not in text": func(r *Rule) { r.Text, r.Placeholder = "abc", "<domain>" },
		"placeholder at the end":  func(r *Rule) { r.Text, r.Placeholder = "from <domain>", "<domain>" },
	} {
		r := ok
		mutate(&r)
		if _, err := NewTable([]Rule{r}); err == nil {
			t.Errorf("%s: accepted %+v", name, r)
		}
	}
	if _, err := NewTable([]Rule{ok, ok}); err == nil {
		t.Error("repeated id accepted")
	}
	if _, err := NewTable([]Rule{ok}); err != nil {
		t.Errorf("valid rule rejected: %v", err)
	}
}

// TestNoPermanentWithoutSpecificRule feeds seeded random replies, mixed with
// fragments of documented ones, and checks against an oracle written apart
// from the table: a reply is permanent only if its enhanced code is one of
// the permanent codes the contract lists, with a matching reply class, or
// its text starts with a documented permanent text. A 5xx with 5.2.2 is
// transient (mailbox full), every other 5xx is undetermined and every 4xx
// is transient.
func TestNoPermanentWithoutSpecificRule(t *testing.T) {
	permEnhanced := map[string]bool{"5.1.1": true, "5.2.3": true, "5.3.4": true}
	var permTexts []string
	var texts []string
	for _, c := range loadCorpus(t) {
		texts = append(texts, c.Text)
		if c.ExpectedClass == Permanent && c.Source != "R-3463" && c.Source != "R-7505" {
			permTexts = append(permTexts, c.Text)
		}
	}
	enhanced := []string{"", "4.7.0", "4.2.2", "5.2.2", "5.1.1", "5.1.10", "5.2.3", "5.3.4", "5.7.1",
		"5.7.26", "5.7.515", "5.7.509", "4.7.28", "2.0.0"}
	codes := []int{250, 421, 450, 451, 452, 550, 552, 553, 554, 556}
	providers := []string{"", "gmail-model", "yahoo-model", "outlook-model", "baseline"}
	const seed, n = 20261022, 200_000
	rng := rand.New(rand.NewPCG(seed, 0))
	randText := func() string {
		switch rng.IntN(4) {
		case 0:
			b := make([]byte, rng.IntN(40))
			for i := range b {
				b[i] = byte(' ' + rng.IntN(95))
			}
			return string(b)
		case 1:
			return texts[rng.IntN(len(texts))]
		case 2:
			s := texts[rng.IntN(len(texts))]
			return s[:rng.IntN(len(s)+1)]
		default:
			return []string{"[TSS04] ", "[IPTS04] ", "x "}[rng.IntN(3)] + texts[rng.IntN(len(texts))]
		}
	}
	var perm, undet int
	for i := 0; i < n; i++ {
		in := Reply{Provider: providers[rng.IntN(len(providers))]}
		if rng.IntN(2) == 0 {
			in.Code = codes[rng.IntN(len(codes))]
		} else {
			in.Code = 200 + rng.IntN(400)
		}
		if rng.IntN(3) == 0 {
			in.Enhanced = fmt.Sprintf("%d.%d.%d", 2+rng.IntN(4), rng.IntN(9), rng.IntN(20))
		} else {
			in.Enhanced = enhanced[rng.IntN(len(enhanced))]
		}
		in.Text = randText()
		d := Classify(in)
		if d.RuleID == "" {
			t.Fatalf("seed %d case %d %+v: no rule_id", seed, i, in)
		}
		specific := in.Code/100 == 5 && strings.HasPrefix(in.Enhanced, "5.") && permEnhanced[in.Enhanced] ||
			in.Code == 556 && in.Enhanced == "5.1.10"
		for _, p := range permTexts {
			specific = specific || in.Code/100 == 5 && strings.HasPrefix(in.Text, p)
		}
		switch {
		case d.Class == Permanent:
			perm++
			if !specific {
				t.Fatalf("seed %d case %d %+v: permanent by %s without a specific rule", seed, i, in, d.RuleID)
			}
		case in.Code/100 == 4 && d.Class != Transient:
			t.Fatalf("seed %d case %d %+v: 4xx is %s", seed, i, in, d.Class)
		case in.Code/100 == 5 && in.Enhanced == "5.2.2":
			if d.Class != Transient || d.Subtype != MailboxFull {
				t.Fatalf("seed %d case %d %+v: mailbox full is %+v", seed, i, in, d)
			}
		case in.Code/100 == 5 && !specific && d.Class != Undetermined:
			t.Fatalf("seed %d case %d %+v: 5xx without rule is %s", seed, i, in, d.Class)
		}
		if d.Class == Undetermined {
			undet++
			if d.RuleID != NoRuleID || d.Subtype != "" {
				t.Fatalf("seed %d case %d %+v: undetermined %+v", seed, i, in, d)
			}
		}
	}
	if perm == 0 || undet == 0 {
		t.Fatalf("degenerate sample: %d permanent, %d undetermined of %d", perm, undet, n)
	}
	t.Logf("seed %d: %d replies, %d permanent, %d undetermined", seed, n, perm, undet)
}
