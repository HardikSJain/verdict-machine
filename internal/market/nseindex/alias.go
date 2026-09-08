package nseindex

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Canonical index identity.
//
// NSE rebranded its entire index family in stages. The Nifty 50 was published
// as "S&P CNX Nifty" until 2013, "CNX Nifty" until November 2015 and "Nifty 50"
// since; "CNX Nifty Junior" became "Nifty Next 50"; "CNX 100" became "Nifty
// 100"; and so on across roughly thirty names. Keying an index series on the
// name NSE printed would split fourteen years of the same index into three
// unrelated series -- the same failure ISIN succession caused for Tata Steel,
// reached through a different door, and with the same consequence: a 200-day
// mean would have no history to average and a trend rule would sit in cash for
// most of a year around each rename.
//
// So this is the index-side equivalent of symbol_links, and it is held to the
// same standard. **A rename is a claim about identity and it is checked against
// the data rather than believed.** VerifyContinuity walks each alias's
// changeover and asserts the level did not jump: a rebrand must leave the level
// alone, and a name change that comes with a rebasing or a constituent overhaul
// is a different index wearing an old name and must NOT be merged. Anything
// uncurated falls through to Slug and stays its own series, which is the safe
// direction -- two series that should be one is a visible gap, one series that
// should be two is a silent lie.
type alias struct {
	// Name is the string NSE printed.
	Name string
	// From and Until bound when this name meant this index. Zero means
	// open-ended. They exist so that a name NSE ever reuses for a different
	// index cannot silently merge two series.
	From  time.Time
	Until time.Time
}

// canonical maps a stable code to every name that has meant it.
//
// Only entries VerifyContinuity passes belong here. The list is deliberately
// short: it covers the indices this project reads, not every index in the file.
// Adding one means running the check against the live store and pasting what it
// measured, the same way a roster line is added.
var canonical = map[string][]alias{
	"NIFTY50": {
		{Name: "S&P CNX Nifty"},
		{Name: "CNX Nifty"},
		{Name: "Nifty 50"},
	},
	"NIFTYNEXT50": {
		{Name: "CNX Nifty Junior"},
		{Name: "Nifty Next 50"},
	},
	"NIFTY100": {
		{Name: "CNX 100"},
		{Name: "Nifty 100"},
	},
	"NIFTY200": {
		{Name: "CNX 200"},
		{Name: "Nifty 200"},
	},
	"NIFTY500": {
		{Name: "S&P CNX 500"},
		{Name: "CNX 500"},
		{Name: "Nifty 500"},
	},
	"NIFTYBANK": {
		{Name: "CNX Bank"},
		{Name: "Nifty Bank"},
	},
	"NIFTYMIDCAP100": {
		{Name: "CNX Midcap"},
		{Name: "Nifty Midcap 100"},
	},
}

// byName is the reverse lookup, built once.
var byName = func() map[string][]struct {
	code string
	a    alias
} {
	m := map[string][]struct {
		code string
		a    alias
	}{}
	for code, aliases := range canonical {
		for _, a := range aliases {
			k := normaliseName(a.Name)
			m[k] = append(m[k], struct {
				code string
				a    alias
			}{code, a})
		}
	}
	return m
}()

// CodeFor returns the canonical code for a published name on a date, or the
// name's slug when it is not curated.
func CodeFor(name string, date time.Time) string {
	for _, c := range byName[normaliseName(name)] {
		if !c.a.From.IsZero() && date.Before(c.a.From) {
			continue
		}
		if !c.a.Until.IsZero() && !date.Before(c.a.Until) {
			continue
		}
		return c.code
	}
	return Slug(name)
}

// Slug is the fallback identity for an index nobody has curated: the name,
// uppercased, with everything that is not a letter or digit removed.
//
// Two names that differ only in punctuation collapse together, which is
// intended -- "Nifty 50" and "NIFTY50" are the same string to NSE's own
// typesetting -- but two names that differ in words stay apart, so an
// uncurated rename shows up as a series that stops and another that starts.
// That is a gap somebody can see, which is the whole point of not guessing.
func Slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "UNKNOWN"
	}
	return s
}

func normaliseName(s string) string {
	return strings.Join(strings.Fields(strings.ToUpper(strings.TrimSpace(s))), " ")
}

// CuratedCodes lists the canonical codes, sorted, for a report or a check.
func CuratedCodes() []string {
	out := make([]string, 0, len(canonical))
	for c := range canonical {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// NamesFor lists the published names a code covers.
func NamesFor(code string) ([]string, error) {
	aliases, ok := canonical[code]
	if !ok {
		return nil, fmt.Errorf("nseindex: %q is not a curated index code", code)
	}
	out := make([]string, 0, len(aliases))
	for _, a := range aliases {
		out = append(out, a.Name)
	}
	return out, nil
}

// Nifty50 is the code the trend filter and the benchmark read.
const Nifty50 = "NIFTY50"
