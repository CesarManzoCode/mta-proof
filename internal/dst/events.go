package dst

import (
	"bufio"
	"os"
	"strconv"
	"unicode/utf16"

	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// writeEvents writes the canonical export of the events table (contract
// 3.6): one row per line in seq order, a JSON object with every column,
// keys sorted, NULL as null, separators "," and ":" without spaces, and
// strings escaped to ASCII. The bytes equal Python's
// json.dumps(row, sort_keys=True, separators=(",", ":")) of the row read
// with sqlite3, so the verifier can rebuild the file from engine.db
// without code of the engine.
func writeEvents(path string, evs []store.Event) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	var b []byte
	for i := range evs {
		b = appendEvent(b[:0], &evs[i])
		if _, err := w.Write(b); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// appendEvent renders one row. The store writes empty strings and zero
// integers of the optional columns as NULL and reads NULL back as zero, so
// zero here is NULL; at_ms and seq are never NULL.
func appendEvent(b []byte, e *store.Event) []byte {
	str := func(b []byte, key, v string) []byte {
		b = append(b, ',', '"')
		b = append(b, key...)
		b = append(b, '"', ':')
		if v == "" {
			return append(b, "null"...)
		}
		return appendString(b, v)
	}
	num := func(b []byte, key string, v int64, nullZero bool) []byte {
		b = append(b, ',', '"')
		b = append(b, key...)
		b = append(b, '"', ':')
		if nullZero && v == 0 {
			return append(b, "null"...)
		}
		return strconv.AppendInt(b, v, 10)
	}
	// Keys in sorted order.
	b = append(b, `{"at_ms":`...)
	b = strconv.AppendInt(b, e.AtMs, 10)
	b = num(b, "attempt", int64(e.Attempt), true)
	b = str(b, "class", e.Class)
	b = num(b, "code", int64(e.Code), true)
	b = str(b, "detail", e.Detail)
	b = str(b, "domain", e.Domain)
	b = str(b, "enhanced", e.Enhanced)
	b = str(b, "kind", e.Kind)
	b = str(b, "msg_id", e.MsgID)
	b = str(b, "msg_type", e.MsgType)
	b = str(b, "mx_host", e.MXHost)
	b = str(b, "provider", e.Provider)
	b = str(b, "rule_id", e.RuleID)
	b = num(b, "seq", e.Seq, false)
	b = str(b, "src_ip", e.SrcIP)
	b = str(b, "subtype", e.Subtype)
	b = str(b, "tenant", e.Tenant)
	b = str(b, "text", e.Text)
	return append(b, '}', '\n')
}

const hexDigits = "0123456789abcdef"

// appendString quotes s as Python's json module does with ensure_ascii:
// '"' and '\' escaped, \n \r \t \b \f by name, every other character
// outside space..'~' as \uXXXX (surrogate pairs above U+FFFF).
func appendString(b []byte, s string) []byte {
	b = append(b, '"')
	for _, r := range s {
		switch {
		case r == '"':
			b = append(b, '\\', '"')
		case r == '\\':
			b = append(b, '\\', '\\')
		case r == '\n':
			b = append(b, '\\', 'n')
		case r == '\r':
			b = append(b, '\\', 'r')
		case r == '\t':
			b = append(b, '\\', 't')
		case r == '\b':
			b = append(b, '\\', 'b')
		case r == '\f':
			b = append(b, '\\', 'f')
		case r >= ' ' && r <= '~':
			b = append(b, byte(r))
		case r > 0xffff:
			r1, r2 := utf16.EncodeRune(r)
			b = appendU(appendU(b, r1), r2)
		default:
			b = appendU(b, r)
		}
	}
	return append(b, '"')
}

func appendU(b []byte, r rune) []byte {
	return append(b, '\\', 'u', hexDigits[r>>12&0xf], hexDigits[r>>8&0xf], hexDigits[r>>4&0xf], hexDigits[r&0xf])
}
