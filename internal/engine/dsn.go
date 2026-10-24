package engine

import (
	"bytes"
	"fmt"
	"regexp"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// DSN is what a delivery status notification for an expired message says.
type DSN struct {
	ID         string         // msg_id of the DSN itself
	Orig       *store.Message // the expired message
	Hostname   string         // Reporting-MTA
	Postmaster string         // From
	At         time.Time      // when the engine gave up
	Last       *Reply         // last reply of the receiver, if any
	RemoteMTA  string         // host that gave Last
	// LastAttemptMs is when the last attempt ended, 0 when the engine gave
	// up without attempting (Last-Attempt-Date is then omitted, §2.3.7).
	LastAttemptMs int64
}

var statusRe = regexp.MustCompile(`^[45]\.[0-9]{1,3}\.[0-9]{1,3}$`)

// dsnStatus is the Status of the DSN: the enhanced code of the last reply
// when it has one of class 4 or 5, since R-3463 §3.5 asks for the code of
// the actual problem rather than X.4.7; otherwise 4.4.7, "Delivery time
// expired".
func dsnStatus(last *Reply) string {
	if last != nil && statusRe.MatchString(last.Enhanced) {
		return last.Enhanced
	}
	return "4.4.7"
}

// fieldText keeps a value printable ASCII on one line and short enough
// for the 998-character line limit.
func fieldText(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	if len(b) > 800 {
		b = b[:800]
	}
	return string(b)
}

// BuildDSN renders the DSN of an expired message [R-3464 §2]: a
// multipart/report with report-type delivery-status, a human-readable
// first part and a message/delivery-status second part in 7bit, addressed
// to the original reverse-path. The original message is not returned (the
// third part is optional). Original-Envelope-Id is the msg_id, the
// envelope identifier the engine gave the sender at ingest (§2.2.1).
func BuildDSN(d DSN) []byte {
	o := d.Orig
	boundary := "mtaproof-dsn-" + d.ID
	var b bytes.Buffer
	h := func(name, value string) { b.WriteString(name + ": " + value + "\r\n") }
	line := func(s string) { b.WriteString(s + "\r\n") }
	h("Date", d.At.UTC().Format(dateLayout))
	h("From", d.Postmaster)
	h("To", o.From)
	h("Subject", "Delivery Status Notification (Failure)")
	h("Message-ID", MessageID(d.ID, d.Postmaster))
	h("X-Mtaproof-Id", d.ID)
	h("MIME-Version", "1.0")
	h("Content-Type", `multipart/report; report-type=delivery-status; boundary="`+boundary+`"`)
	line("")
	line("--" + boundary)
	h("Content-Type", "text/plain; charset=us-ascii")
	h("Content-Transfer-Encoding", "7bit")
	line("")
	line("This is the mail system at " + d.Hostname + ".")
	line("")
	line("Your message " + o.ID + " to " + fieldText(o.To) + " could not be delivered:")
	line(fmt.Sprintf("the engine gave up after %d attempts.", o.Attempts))
	if d.Last != nil {
		line("Last reply: " + fieldText(d.Last.String()))
	}
	line("--" + boundary)
	h("Content-Type", "message/delivery-status")
	h("Content-Transfer-Encoding", "7bit")
	line("")
	h("Original-Envelope-Id", o.ID)
	h("Reporting-MTA", "dns; "+d.Hostname)
	h("Arrival-Date", time.UnixMilli(o.AcceptedAtMs).UTC().Format(dateLayout))
	line("")
	h("Final-Recipient", "rfc822; "+fieldText(o.To))
	h("Action", "failed")
	h("Status", dsnStatus(d.Last))
	if d.Last != nil && d.RemoteMTA != "" {
		h("Remote-MTA", "dns; "+fieldText(d.RemoteMTA))
	}
	if d.Last != nil {
		h("Diagnostic-Code", "smtp; "+fieldText(d.Last.String()))
	}
	if d.LastAttemptMs > 0 {
		h("Last-Attempt-Date", time.UnixMilli(d.LastAttemptMs).UTC().Format(dateLayout))
	}
	line("")
	line("--" + boundary + "--")
	return b.Bytes()
}
