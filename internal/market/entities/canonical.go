package entities

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// RFC 8785 (JSON Canonicalization Scheme) serialization, and the sha256 taken
// over it.
//
// The roster's digest is stamped into every row `apply` writes, so it is the
// link between an irreversible merge in the store and a diff a human
// approved. That only holds if the digest is a function of the roster's
// CONTENT and not of its formatting: a reviewer who reformats the file, a
// tool that reorders keys, or a generator that emits links in a different
// order must all produce the same bytes to hash. RFC 8785 is what makes that
// true -- object keys sorted by UTF-16 code unit, no insignificant
// whitespace, ECMAScript number formatting -- and it is the convention this
// project already commits to for hashing.

// CanonicalJSON returns v serialized as RFC 8785 canonical JSON. v is first
// marshalled by encoding/json and then re-serialized from the resulting tree,
// so struct tags decide the field names and this code decides the bytes.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep the literal; the ES6 rule below decides its form
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DigestOf returns the sha256 of v's canonical JSON.
func DigestOf(v any) ([]byte, error) {
	b, err := CanonicalJSON(v)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return fmt.Errorf("canonical json: %q: %w", t.String(), err)
		}
		s, err := es6Number(f)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// RFC 8785 sorts on UTF-16 code units, not on bytes and not on runes.
		// For ASCII keys the three agree; above U+FFFF they do not, and a
		// roster is a file humans edit.
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonical json: unsupported type %T", v)
	}
	return nil
}

func lessUTF16(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

// writeCanonicalString applies RFC 8785's escaping: the two mandatory escapes,
// the five short forms for the control characters that have them, \u00xx for
// the rest, and literal UTF-8 for everything else.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// es6Number formats f the way ECMAScript's Number::toString does, which is
// what RFC 8785 specifies: the shortest decimal that round-trips, plain
// notation between 1e-6 and 1e21, exponential outside that with no leading
// zero in the exponent, and "0" for negative zero.
func es6Number(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("canonical json: %v is not representable", f)
	}
	if f == 0 {
		return "0", nil // covers -0
	}
	abs := math.Abs(f)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
	// Go writes the exponent with at least two digits (1e-07); ECMAScript
	// writes the minimum (1e-7).
	s := strconv.FormatFloat(f, 'e', -1, 64)
	i := strings.IndexByte(s, 'e')
	mantissa, sign, digits := s[:i], s[i+1:i+2], strings.TrimLeft(s[i+2:], "0")
	if digits == "" {
		digits = "0"
	}
	return mantissa + "e" + sign + digits, nil
}
