package engine

import (
	"context"
	"time"
)

// Bar is one session for one entity, as the engine needs it. It is a local
// type rather than market.StoredBar so that the fixture loader the CI golden
// backtest runs on does not have to fabricate a whole store row.
type Bar struct {
	EntityID int64
	Scrip    string
	Open     float64
	High     float64
	Low      float64
	Close    float64
}

// Member is one name in the point-in-time universe, in rank order.
type Member struct {
	EntityID int64
	Scrip    string
	Rank     int

	// LastBreak is the most recent succession boundary at or before the as-of
	// date, or the zero time. A strategy does not need it to be safe --
	// Returns already refuses across a boundary -- but a report that wants to
	// say which names carry one needs it here.
	LastBreak time.Time
}

// ReturnSet mirrors market.ReturnSet at the seam, and mirrors it deliberately:
// the three-way split is what stops a caller getting at a priced return
// without walking past the refusals, and flattening it here would undo the
// fence one layer above where it was built.
type ReturnSet struct {
	Priced  map[int64]float64
	Refused map[int64]string
	Absent  map[int64]string
}

// Market is everything the engine and its strategies read. It is the Data seam
// from the design, and it ships with two implementations: Store, over
// Postgres, and Fixture, over a frozen table the CI golden backtest uses.
type Market interface {
	// Sessions are the trading days to step, ascending.
	Sessions(ctx context.Context) ([]time.Time, error)

	// Bars is every entity that traded on a session, keyed by entity id.
	Bars(ctx context.Context, date time.Time) (map[int64]Bar, error)

	// Universe is the point-in-time tradeable set as of a session, in rank
	// order. It must be computed from what was knowable on that date.
	Universe(ctx context.Context, asOf time.Time) ([]Member, error)

	// Returns prices a window for a set of entities, refusing any whose window
	// touches a succession boundary's guard band.
	Returns(ctx context.Context, entityIDs []int64, from, to time.Time) (ReturnSet, error)

	// Closes is one entity's close series over a range, ascending. It errors
	// rather than returning a series that spans a succession boundary: a moving
	// average taken across an unadjusted split is not a wrong number, it is a
	// meaningless one, sitting above every post-split price and below every
	// pre-split one.
	Closes(ctx context.Context, entityID int64, from, to time.Time) ([]DatedClose, error)
}

// DatedClose is one session's close.
type DatedClose struct {
	Date  time.Time
	Close float64
}
