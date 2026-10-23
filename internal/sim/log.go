package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Receiver log event kinds (contract 3.4).
const (
	EvConnect   = "connect"
	EvEHLO      = "ehlo"
	EvStartTLS  = "starttls"
	EvMail      = "mail"
	EvRcpt      = "rcpt"
	EvDataEnd   = "data_end"
	EvReply     = "reply"
	EvClose     = "close"
	EvDrop      = "drop"
	EvViolation = "violation"
)

// Event is one line of receiver.jsonl. Field order is the struct order, so
// the encoding is deterministic.
type Event struct {
	Seq      int64   `json:"seq"`
	AtMs     int64   `json:"at_ms"`
	Conn     int64   `json:"conn"`
	SrcIP    string  `json:"src_ip"`
	DstIP    string  `json:"dst_ip"`
	Host     string  `json:"host"`
	Profile  string  `json:"profile"`
	Ev       string  `json:"ev"`
	MailFrom *string `json:"mail_from,omitempty"`
	Rcpt     string  `json:"rcpt,omitempty"`
	MsgID    string  `json:"msg_id,omitempty"`
	Sha256   string  `json:"sha256,omitempty"`
	Code     int     `json:"code,omitempty"`
	Enhanced *string `json:"enhanced,omitempty"`
	Text     *string `json:"text,omitempty"`
	Rule     string  `json:"rule,omitempty"`
	Counted  *bool   `json:"counted,omitempty"`
	TLS      *TLS    `json:"tls,omitempty"`
}

// TLS is the tls field of a starttls line (the outcome of the handshake)
// and of a data_end received over TLS (the session's version and suite).
type TLS struct {
	Version string `json:"version,omitempty"`
	Cipher  string `json:"cipher,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Log appends events to receiver.jsonl. Sync is called after every
// data_end so a reply to DATA is never sent before its line is durable.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	seq  int64
	sync bool
}

// CreateLog creates path; it refuses to overwrite an existing log so a
// run directory never mixes two simulator lives.
func CreateLog(path string, sync bool) (*Log, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, sync: sync}, nil
}

// Append assigns the next seq and writes the event as one line.
func (l *Log) Append(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = l.f.Write(append(b, '\n'))
	return err
}

// Sync flushes the log to stable storage when the log was created with
// sync enabled.
func (l *Log) Sync() error {
	if !l.sync {
		return nil
	}
	return l.f.Sync()
}

// Close syncs and closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Sync(); err != nil {
		l.f.Close()
		return err
	}
	return l.f.Close()
}

// Store keeps received messages as messages/<sha256>.eml.
type Store struct {
	dir  string
	sync bool
}

// NewStore creates the messages directory. With sync, every message and
// its directory entry are durable before Put returns, so a synced
// data_end never points at a missing file.
func NewStore(dir string, sync bool) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, sync: sync}, nil
}

// Put writes the message unless a message with the same hash exists and
// returns the hex sha256.
func (s *Store) Put(msg []byte) (string, error) {
	sum := sha256.Sum256(msg)
	name := hex.EncodeToString(sum[:])
	path := filepath.Join(s.dir, name+".eml")
	if _, err := os.Stat(path); err == nil {
		return name, nil
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(msg); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if s.sync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", err
		}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("store message: %w", err)
	}
	if s.sync {
		d, err := os.Open(s.dir)
		if err != nil {
			return "", err
		}
		defer d.Close()
		if err := d.Sync(); err != nil {
			return "", err
		}
	}
	return name, nil
}
