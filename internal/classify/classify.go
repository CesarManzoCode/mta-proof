// Package classify decides the class and subtype of a receiver's SMTP reply
// (contract 3.8) from an explicit rule table. Every decision names the rule
// that made it; a reply no rule covers is undetermined, never permanent
// (D14).
package classify

import (
	"fmt"
	"strings"
)

// Classes (contract 3.8).
const (
	Permanent    = "permanent"
	Transient    = "transient"
	Undetermined = "undetermined"
)

// Subtypes: Resend's [R-RESEND-BOUNCES] plus the project's (contract 3.8).
// An undetermined decision has no subtype.
const (
	General            = "General"
	NoEmail            = "NoEmail"
	MailboxFull        = "MailboxFull"
	MessageTooLarge    = "MessageTooLarge"
	ContentRejected    = "ContentRejected"
	AttachmentRejected = "AttachmentRejected"
	RateLimited        = "RateLimited"
	Reputation         = "Reputation"
	AuthConfig         = "AuthConfig"
	TLSPolicy          = "TLSPolicy"
	NullMX             = "NullMX"
	Connection         = "Connection"
)

// Tier orders the rules: provider rules first, then enhanced status codes,
// then the generic class 4 rule. Within a tier the first matching rule in
// table order wins.
type Tier int

const (
	TierProvider Tier = iota + 1
	TierEnhanced
	TierClass
)

// NoRuleID is the rule id of the decision for a reply no rule covers (D14).
const NoRuleID = "d14.undetermined"

// Reply is what the classifier sees of one receiver response. Provider is
// the destination's provider as routed ("" when unknown).
type Reply struct {
	Provider string
	Code     int
	Enhanced string
	Text     string
}

// Decision is a classification and the id of the rule that made it.
type Decision struct {
	Class   string
	Subtype string
	RuleID  string
}

// Rule is one row of the table. Zero-valued condition fields match
// anything; a rule must set at least one of Enhanced, Tag and Text, except
// in TierClass.
type Rule struct {
	ID      string
	Tier    Tier
	Source  string // RESEARCH identifier
	Section string // section of the source, when it has sections

	// Conditions.
	Provider  string // exact provider, "" for any
	Code      int    // exact reply code, 0 for any
	CodeClass int    // first digit of the reply code, 0 for any
	// Enhanced is an exact enhanced status code, or "X.s.d" for any class;
	// in both cases its class must agree with the reply code's.
	Enhanced string
	Tag      string // bracketed tag the text must contain, like "[TSS04]"
	// Text is the literal start of the source text the reply text must
	// begin with. When Placeholder is set, the placeholder in Text stands
	// for one domain without spaces (or the placeholder itself, as the
	// source prints it).
	Text        string
	Placeholder string

	// Result.
	Class   string
	Subtype string
}

func (r *Rule) matches(in Reply) bool {
	if r.Provider != "" && r.Provider != in.Provider {
		return false
	}
	if r.Code != 0 && r.Code != in.Code {
		return false
	}
	if r.CodeClass != 0 && r.CodeClass != in.Code/100 {
		return false
	}
	if r.Enhanced != "" && !enhancedMatches(r.Enhanced, in) {
		return false
	}
	if r.Tag != "" && !strings.Contains(in.Text, r.Tag) {
		return false
	}
	if r.Text != "" && !textMatches(r.Text, r.Placeholder, in.Text) {
		return false
	}
	return true
}

func enhancedMatches(pattern string, in Reply) bool {
	if in.Enhanced == "" || in.Enhanced[0] != byte('0'+in.Code/100) {
		return false
	}
	if rest, ok := strings.CutPrefix(pattern, "X."); ok {
		_, inRest, _ := strings.Cut(in.Enhanced, ".")
		return inRest == rest
	}
	return pattern == in.Enhanced
}

func textMatches(tmpl, placeholder, text string) bool {
	if placeholder == "" {
		return strings.HasPrefix(text, tmpl)
	}
	before, after, _ := strings.Cut(tmpl, placeholder)
	rest, ok := strings.CutPrefix(text, before)
	if !ok {
		return false
	}
	i := strings.Index(rest, after)
	return i > 0 && !strings.ContainsAny(rest[:i], " \t")
}

// Table is an ordered, validated set of rules.
type Table struct {
	rules []Rule
}

// NewTable validates rules and orders them by tier, keeping table order
// within a tier.
func NewTable(rules []Rule) (*Table, error) {
	seen := map[string]bool{NoRuleID: true}
	t := &Table{}
	for tier := TierProvider; tier <= TierClass; tier++ {
		for _, r := range rules {
			if r.Tier == tier {
				t.rules = append(t.rules, r)
			}
		}
	}
	for _, r := range rules {
		switch {
		case r.ID == "" || seen[r.ID]:
			return nil, fmt.Errorf("classify: empty or repeated rule id %q", r.ID)
		case r.Tier < TierProvider || r.Tier > TierClass:
			return nil, fmt.Errorf("classify: rule %s: tier %d", r.ID, r.Tier)
		case r.Source == "":
			return nil, fmt.Errorf("classify: rule %s without source", r.ID)
		case r.Class != Permanent && r.Class != Transient:
			return nil, fmt.Errorf("classify: rule %s: class %q", r.ID, r.Class)
		case !validSubtype(r.Subtype):
			return nil, fmt.Errorf("classify: rule %s: subtype %q", r.ID, r.Subtype)
		case r.Tier == TierClass && (r.CodeClass != 4 || r.Class != Transient || r.Provider != "" ||
			r.Enhanced != "" || r.Tag != "" || r.Text != ""):
			// D14: generic rules only exist for class 4, and are transient.
			return nil, fmt.Errorf("classify: rule %s: a class rule must be the class 4 transient rule", r.ID)
		case r.Tier != TierClass && r.Enhanced == "" && r.Tag == "" && r.Text == "":
			return nil, fmt.Errorf("classify: rule %s: needs an enhanced code, tag or text", r.ID)
		case r.Tier == TierEnhanced && r.Enhanced == "":
			return nil, fmt.Errorf("classify: rule %s: an enhanced rule needs an enhanced code", r.ID)
		case r.Class == Permanent && !(r.Code/100 == 5 || r.CodeClass == 5 || strings.HasPrefix(r.Enhanced, "5.")):
			// A permanent rule can only match 5xx replies.
			return nil, fmt.Errorf("classify: rule %s: a permanent rule must require class 5", r.ID)
		case r.Placeholder != "" && (!strings.Contains(r.Text, r.Placeholder) || strings.HasSuffix(r.Text, r.Placeholder)):
			return nil, fmt.Errorf("classify: rule %s: placeholder must be inside the text", r.ID)
		}
		seen[r.ID] = true
	}
	return t, nil
}

func validSubtype(s string) bool {
	switch s {
	case General, NoEmail, MailboxFull, MessageTooLarge, ContentRejected, AttachmentRejected,
		RateLimited, Reputation, AuthConfig, TLSPolicy, NullMX, Connection:
		return true
	}
	return false
}

// Rules returns the table in evaluation order.
func (t *Table) Rules() []Rule { return append([]Rule(nil), t.rules...) }

// Classify returns the decision of the first matching rule in evaluation
// order, or undetermined with NoRuleID.
func (t *Table) Classify(in Reply) Decision {
	for i := range t.rules {
		if r := &t.rules[i]; r.matches(in) {
			return Decision{Class: r.Class, Subtype: r.Subtype, RuleID: r.ID}
		}
	}
	return Decision{Class: Undetermined, RuleID: NoRuleID}
}

// Default is the engine's table (contract 3.8).
var Default = mustTable(defaultRules)

// Classify classifies with the Default table.
func Classify(in Reply) Decision { return Default.Classify(in) }

func mustTable(rules []Rule) *Table {
	t, err := NewTable(rules)
	if err != nil {
		panic(err)
	}
	return t
}
