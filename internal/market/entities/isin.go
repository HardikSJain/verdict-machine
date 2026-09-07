// Package entities mints and audits the canonical-entity overlay: the roster
// that says which ISINs are one company, the gates that decide what may go
// into it, and the commands that write it to the store.
//
// The decision that two ISINs are one company is made in a file in git and
// nowhere else. `propose` is read-only and produces a candidate roster plus a
// review file; a human reviews the diff; `apply` writes the rows, stamping
// the reviewed file's sha256 into every one. Nothing here runs automatically
// after an ingest, because a false positive merges two genuinely different
// companies in a store that has no DELETE.
package entities

import "strings"

// ISIN structure, per ISO 6166 and NSDL's allocation scheme. An Indian equity
// ISIN is INE + a 4-character issuer code + an issuer-type character + a
// 2-digit security type (01 for equity) + a 2-digit issue serial + a check
// digit, which is why the identifier names its own issuer and its own issue
// number:
//
//	I N E 0 8 1 A 0 1 0 1 2
//	          |     | | |  \_ check digit          (index 11)
//	          |     | |  \___ issue serial "01"    (index 9..10)
//	          |     |  \_____ security type "01"   (index 7..8)
//	          |      \_______ issuer type 'A'      (index 6)
//	           \_____________ issuer code "081A"   -- with INE, left(isin,9)
//
// left(isin, 9) is therefore the issuer, and it is what G1 compares: NSDL
// assigns an issuer code to a legal entity, so a different company cannot
// share it. Measured on this archive: 507 of 507 equity succession pairs
// agree on it, zero exceptions. That is an archive fact, not a promise from
// NSDL, and design 10 says so.
const (
	isinLength  = 12
	prefixLen   = 9  // len("INE" + issuer code + issuer type)
	serialStart = 9  // index of the 2-digit issue serial
	serialEnd   = 11 // exclusive
)

// WellFormedISIN reports whether s is a syntactically valid ISIN: twelve
// characters, a two-letter country code, nine alphanumerics and a trailing
// check digit that satisfies the ISO 6166 mod-10.
func WellFormedISIN(s string) bool {
	if len(s) != isinLength {
		return false
	}
	for i := 0; i < 2; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	for i := 2; i < isinLength-1; i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	if s[isinLength-1] < '0' || s[isinLength-1] > '9' {
		return false
	}
	return CheckDigitOK(s)
}

// CheckDigitOK reports whether s satisfies the ISO 6166 mod-10 check digit.
//
// The scheme is Luhn over the digit expansion of the identifier: every letter
// becomes its zero-based position in the alphabet plus ten (A -> 10, Z -> 35)
// and is written out as two digits, then every second digit counting from the
// right is doubled with its own digits summed, and the total must be a
// multiple of ten.
//
// It costs nothing against real data -- 4,727 of 4,727 ISINs in eod2's map
// pass -- and that is the point: it cannot reject a genuine line, and it does
// catch a typo in a hand-written roster line, which is exactly where a false
// positive would originate.
func CheckDigitOK(s string) bool {
	if len(s) != isinLength {
		return false
	}
	// Walk right to left over the expanded digits without materialising the
	// expansion, so the doubling alternates over DIGITS and not over
	// characters -- a letter contributes two digits and flips the parity of
	// everything to its left.
	sum, pos := 0, 0
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		var digits [2]int
		var n int
		switch {
		case c >= '0' && c <= '9':
			digits[0], n = int(c-'0'), 1
		case c >= 'A' && c <= 'Z':
			v := int(c-'A') + 10
			digits[0], digits[1], n = v%10, v/10, 2
		default:
			return false
		}
		for j := 0; j < n; j++ {
			d := digits[j]
			if pos%2 == 1 {
				if d *= 2; d > 9 {
					d -= 9
				}
			}
			sum += d
			pos++
		}
	}
	return sum%10 == 0
}

// IssuerPrefix returns left(isin, 9): the country code, the NSDL issuer code
// and the issuer type. It is G1's whole content.
func IssuerPrefix(s string) string {
	if len(s) < prefixLen {
		return s
	}
	return s[:prefixLen]
}

// IssueSerial returns the 2-digit issue serial as an integer. A face-value
// split mints a new ISIN for the same issuer with the serial incremented, so
// the serial reads the DIRECTION of a succession off the identifier itself
// rather than off the dates -- and G2 refuses a candidate whose dates and
// serial disagree.
func IssueSerial(s string) (int, bool) {
	if len(s) < serialEnd {
		return 0, false
	}
	n := 0
	for i := serialStart; i < serialEnd; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// IsEquityISIN reports whether s is an Indian equity ISIN (INE...). Fund
// units are INF, and G1 is meaningless for them: one INF issuer code covers
// dozens of unrelated schemes (INF732E01 spans nine ETFs, INF247L01 spans 39
// Motilal products), so an INF pair can never be auto-accepted and goes to
// the quarantine queue instead.
func IsEquityISIN(s string) bool { return strings.HasPrefix(s, "INE") }
