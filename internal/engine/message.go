package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

// Message types accepted at ingest (contract 3.3).
const (
	TypeTransactional = "transactional"
	TypeMarketing     = "marketing"
)

// Submission is the body of POST /v1/messages: one recipient (D13).
type Submission struct {
	Tenant  string `json:"tenant"`
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	MsgType string `json:"msg_type"`
}

// Header limits. A Subject line must stay under the 998-character limit of
// RFC 5322 §2.1.1 once "Subject: " is prepended.
const (
	maxSubject = 900
	maxAddress = 254
	maxTenant  = 64
	maxText    = 512 << 10
)

// Validate checks a submission before it reaches the core. Addresses are
// bare addr-specs; the subject is printable ASCII so it needs no RFC 2047
// encoding; the text is UTF-8 and travels quoted-printable.
func (s *Submission) Validate() error {
	if s.Tenant == "" || len(s.Tenant) > maxTenant || strings.IndexFunc(s.Tenant, notTenantRune) >= 0 {
		return errors.New("tenant must be 1-64 characters of [A-Za-z0-9._-]")
	}
	if err := checkAddr("from", s.From); err != nil {
		return err
	}
	if err := checkAddr("to", s.To); err != nil {
		return err
	}
	if s.Subject == "" || len(s.Subject) > maxSubject {
		return fmt.Errorf("subject must be 1-%d characters", maxSubject)
	}
	for i := 0; i < len(s.Subject); i++ {
		if c := s.Subject[i]; c < 0x20 || c > 0x7e {
			return errors.New("subject must be printable ASCII")
		}
	}
	if len(s.Text) > maxText || !utf8.ValidString(s.Text) {
		return fmt.Errorf("text must be valid UTF-8 of at most %d bytes", maxText)
	}
	if s.MsgType != TypeTransactional && s.MsgType != TypeMarketing {
		return errors.New("msg_type must be transactional or marketing")
	}
	return nil
}

func notTenantRune(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
}

// checkAddr accepts exactly one ASCII addr-spec, without display name or
// list, so the address can be used unchanged in MAIL FROM, RCPT TO and the
// header fields.
func checkAddr(field, v string) error {
	if v == "" || len(v) > maxAddress {
		return fmt.Errorf("%s must be one address of at most %d characters", field, maxAddress)
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c <= 0x20 || c > 0x7e {
			return fmt.Errorf("%s must be an ASCII addr-spec", field)
		}
	}
	a, err := mail.ParseAddress(v)
	if err != nil || a.Name != "" || a.Address != v {
		return fmt.Errorf("%s must be exactly one addr-spec", field)
	}
	return nil
}

// domainOf returns the part after the last "@", lower-cased.
func domainOf(addr string) string {
	return strings.ToLower(addr[strings.LastIndexByte(addr, '@')+1:])
}

// PayloadSHA256 hashes the parsed submission, not the request bytes, so two
// requests that differ only in JSON spacing or key order are the same
// payload for idempotency.
func (s *Submission) PayloadSHA256() string {
	b, _ := json.Marshal(s) // struct field order is fixed
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// MessageID is the Message-ID derived from msg_id (D15): the msg_id is
// dot-atom-text and the sender's domain is the right-hand side, as RFC 5322
// §3.6.4 recommends.
func MessageID(msgID, from string) string {
	return "<" + msgID + ".mtaproof@" + domainOf(from) + ">"
}

// dateLayout is the RFC 5322 §3.3 date-time with day-of-week and a numeric
// zone.
const dateLayout = "Mon, 02 Jan 2006 15:04:05 -0700"

// BuildMessage renders the RFC 5322 message for a submission. Date is the
// ingest time, the moment the message enters the delivery system
// (§3.6.1); it is rendered in UTC as "+0000". Lines end in CRLF.
func BuildMessage(msgID string, s *Submission, at time.Time) []byte {
	var b bytes.Buffer
	h := func(name, value string) { b.WriteString(name + ": " + value + "\r\n") }
	h("Date", at.UTC().Format(dateLayout))
	h("From", s.From)
	h("To", s.To)
	h("Subject", s.Subject)
	h("Message-ID", MessageID(msgID, s.From))
	h("X-Mtaproof-Id", msgID)
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	text := strings.ReplaceAll(strings.ReplaceAll(s.Text, "\r\n", "\n"), "\r", "\n")
	qp := quotedprintable.NewWriter(&b)
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			qp.Write([]byte("\r\n"))
		}
		qp.Write([]byte(line))
	}
	qp.Close()
	if !bytes.HasSuffix(b.Bytes(), []byte("\r\n")) {
		b.WriteString("\r\n")
	}
	return b.Bytes()
}
