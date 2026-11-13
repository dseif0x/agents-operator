package session

import (
	"bytes"
	"encoding/json"
	"strings"
)

// stripNUL removes U+0000 from every string (and object key) in a JSON
// document. PostgreSQL rejects NUL in text and jsonb ("unsupported Unicode
// escape sequence", SQLSTATE 22P05), so a single agent event carrying one —
// e.g. a tool result that printed raw binary such as `head -c 100 file.png` —
// failed every time it was stored. The runner keeps a failed batch and
// retries it, so the session's whole event stream stalled behind it.
//
// Documents without a `\u0000` escape (encoding/json always escapes NUL) are
// returned as is; anything that does not parse is left for the caller.
func stripNUL(doc json.RawMessage) json.RawMessage {
	if !bytes.Contains(doc, []byte(`\u0000`)) {
		return doc
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return doc
	}
	out, err := json.Marshal(stripNULValue(v))
	if err != nil {
		return doc
	}
	return out
}

func stripNULValue(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "\x00", "")
	case []any:
		for i := range t {
			t[i] = stripNULValue(t[i])
		}
		return t
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[strings.ReplaceAll(k, "\x00", "")] = stripNULValue(e)
		}
		return m
	default:
		return v
	}
}
