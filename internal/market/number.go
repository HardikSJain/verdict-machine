package market

import (
	"fmt"
	"math"
)

// A source CSV is untrusted input, and Go's number parsing is permissive in
// two ways that reach all the way into the database silently.
//
// strconv.ParseFloat accepts "nan", "inf" and "-inf" with a nil error, and
// pgx encodes those into a numeric column as Postgres NaN / Infinity rather
// than failing. Postgres then orders NaN above every real number, so a single
// poisoned close would rank first in the turnover-ranked universe the whole
// strategy is built on.
//
// Converting such a float to an integer is undefined behaviour in Go:
// int64(NaN) is 0 and int64(+Inf) is 9223372036854775807 on this platform, so
// a nonsense volume becomes a plausible-looking one with no error anywhere.
//
// Finite and Count are the one place both loaders route their parsed numbers
// through, so neither escape is reachable from either source.

// 2^63 and -2^63: the first float64 above the int64 range, and the smallest
// float64 still inside it.
const (
	countLimitExclusive = 9223372036854775808.0
	countLimitInclusive = -9223372036854775808.0
)

// Finite rejects NaN and ±Inf. field names the column for the error message.
func Finite(field string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s is %v; a bar's numbers must be finite", field, v)
	}
	return nil
}

// Count converts a parsed float quantity (a volume or a delivery quantity) to
// the int64 the store holds, rejecting non-finite and out-of-range values
// rather than letting the conversion invent one.
func Count(field string, v float64) (int64, error) {
	if err := Finite(field, v); err != nil {
		return 0, err
	}
	if v >= countLimitExclusive || v < countLimitInclusive {
		return 0, fmt.Errorf("%s is %v; outside the range of a 64-bit integer", field, v)
	}
	return int64(v), nil
}
