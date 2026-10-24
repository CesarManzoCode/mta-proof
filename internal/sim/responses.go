package sim

import (
	"fmt"
	"strings"
)

// Response is an SMTP reply as profiles declare it: code, enhanced status
// code (empty when the source gives none) and text.
type Response struct {
	Code     int    `json:"code"`
	Enhanced string `json:"enhanced"`
	Text     string `json:"text"`
}

// String renders the reply as one wire line without CRLF.
func (r Response) String() string {
	parts := []string{fmt.Sprint(r.Code)}
	if r.Enhanced != "" {
		parts = append(parts, r.Enhanced)
	}
	if r.Text != "" {
		parts = append(parts, r.Text)
	}
	return strings.Join(parts, " ")
}

// documented is one row of contract table 3.2 that a profile may use.
type documented struct {
	Response
	Source    string
	Secondary bool
	// Placeholder is the literal token of the source text that is replaced
	// with the sender domain at runtime ("" when the text has none).
	Placeholder string
}

// documentedResponses is contract table 3.2 without 556 5.1.10, which only
// the engine generates. Texts are the literal starts from the sources.
var documentedResponses = []documented{
	{Response{421, "4.7.28", "Gmail has detected an unusual rate of email originating from your IP address"}, "R-GOOGLE-ERR", false, ""},
	{Response{421, "4.7.0", "Try again later, closing connection."}, "R-GOOGLE-ERR", false, ""},
	{Response{550, "5.7.26", "This email has been blocked because the sender is unauthenticated."}, "R-GOOGLE-ERR", false, ""},
	{Response{550, "5.7.26", "Unauthenticated email from domain-name is not accepted due to domain's DMARC policy."}, "R-GOOGLE-ERR", false, "domain-name"},
	{Response{450, "4.2.1", "The user you are trying to contact is receiving email too quickly."}, "R-GOOGLE-ERR", false, ""},
	{Response{550, "5.1.1", "The email account that you tried to reach does not exist."}, "R-GOOGLE-ERR", false, ""},
	{Response{421, "4.7.0", "[TSS04] Message temporarily deferred by Yahoo"}, "R-SUPED-YAHOO", true, ""},
	{Response{421, "", "[IPTS04] Message deferred for IP policy reasons"}, "R-SUPED-YAHOO", true, ""},
	{Response{550, "5.7.515", "Access denied, sending domain <domain> does not meet the required authentication level."}, "R-MS-515", false, "<domain>"},
	{Response{550, "5.7.509", "Access denied, sending domain <domain> does not pass DMARC verification and has a DMARC policy of reject."}, "R-MS-509", true, "<domain>"},
	{Response{451, "4.3.0", "Requested action aborted: local error in processing"}, "R-5321", false, ""},
}

// lookupDocumented returns the table 3.2 row that r reproduces exactly.
func lookupDocumented(r Response) (documented, bool) {
	for _, d := range documentedResponses {
		if d.Response == r {
			return d, true
		}
	}
	return documented{}, false
}

// render substitutes the sender domain into a documented placeholder.
func render(r Response, senderDomain string) Response {
	d, ok := lookupDocumented(r)
	if ok && d.Placeholder != "" && senderDomain != "" {
		r.Text = strings.Replace(r.Text, d.Placeholder, senderDomain, 1)
	}
	return r
}
