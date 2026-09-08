package engine

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Fixture is the Market's second implementation: a frozen table with no
// database behind it.
//
// The design requires every seam to ship two implementations, and this is the
// one that earns its keep twice over. It is what the CI golden backtest runs
// on -- a committed fixture whose result is committed beside it, so a change
// that quietly alters a number fails the build -- and it is what makes the
// engine's own tests assertions about the LOOP rather than about Postgres.
type Fixture struct {
	sessions []time.Time
	bars     map[string]map[int64]Bar
	universe map[string][]Member

	// refuse marks entities whose returns are refused, so a test can exercise
	// the path a real succession boundary takes without seeding one.
	refuse map[int64]string
}

// NewFixture builds an empty fixture over a session calendar.
func NewFixture(sessions []time.Time) *Fixture {
	s := append([]time.Time(nil), sessions...)
	sort.Slice(s, func(i, j int) bool { return s[i].Before(s[j]) })
	return &Fixture{
		sessions: s,
		bars:     map[string]map[int64]Bar{},
		universe: map[string][]Member{},
		refuse:   map[int64]string{},
	}
}

func key(d time.Time) string { return d.Format(time.DateOnly) }

// AddBar records one entity's session.
func (f *Fixture) AddBar(date time.Time, b Bar) *Fixture {
	k := key(date)
	if f.bars[k] == nil {
		f.bars[k] = map[int64]Bar{}
	}
	f.bars[k][b.EntityID] = b
	return f
}

// SetUniverse fixes the tradeable set as of a session. When a session has no
// explicit universe, everything that traded that day is the universe, in
// entity id order -- enough for a loop test and never enough for a real one,
// which is why StoreMarket exists.
func (f *Fixture) SetUniverse(date time.Time, members []Member) *Fixture {
	f.universe[key(date)] = append([]Member(nil), members...)
	return f
}

// Refuse marks an entity's returns as refused, standing in for a succession
// boundary inside the window.
func (f *Fixture) Refuse(entityID int64, why string) *Fixture {
	f.refuse[entityID] = why
	return f
}

func (f *Fixture) Sessions(context.Context) ([]time.Time, error) {
	if len(f.sessions) == 0 {
		return nil, fmt.Errorf("engine: fixture has no sessions")
	}
	return append([]time.Time(nil), f.sessions...), nil
}

func (f *Fixture) Bars(_ context.Context, date time.Time) (map[int64]Bar, error) {
	out := map[int64]Bar{}
	for id, b := range f.bars[key(date)] {
		out[id] = b
	}
	return out, nil
}

func (f *Fixture) Universe(_ context.Context, asOf time.Time) ([]Member, error) {
	if u, ok := f.universe[key(asOf)]; ok {
		return append([]Member(nil), u...), nil
	}
	traded := f.bars[key(asOf)]
	ids := make([]int64, 0, len(traded))
	for id := range traded {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]Member, 0, len(ids))
	for i, id := range ids {
		out = append(out, Member{EntityID: id, Scrip: traded[id].Scrip, Rank: i + 1})
	}
	return out, nil
}

// Returns prices from the fixture's own bars, using the last session at or
// before each endpoint, and keeps the three-way split so that a strategy
// tested here meets the same contract it meets in production.
func (f *Fixture) Returns(_ context.Context, entityIDs []int64, from, to time.Time) (ReturnSet, error) {
	if !from.Before(to) {
		return ReturnSet{}, fmt.Errorf("engine: fixture returns need from before to")
	}
	rs := ReturnSet{
		Priced:  map[int64]float64{},
		Refused: map[int64]string{},
		Absent:  map[int64]string{},
	}
	for _, id := range entityIDs {
		if why, ok := f.refuse[id]; ok {
			rs.Refused[id] = why
			continue
		}
		a, aok := f.closeAtOrBefore(id, from)
		b, bok := f.closeAtOrBefore(id, to)
		switch {
		case !aok || !bok:
			rs.Absent[id] = "no fixture session at an endpoint"
		case a <= 0:
			rs.Absent[id] = "non-positive close at the near endpoint"
		default:
			rs.Priced[id] = b/a - 1
		}
	}
	return rs, nil
}

func (f *Fixture) closeAtOrBefore(id int64, date time.Time) (float64, bool) {
	for i := len(f.sessions) - 1; i >= 0; i-- {
		d := f.sessions[i]
		if d.After(date) {
			continue
		}
		if b, ok := f.bars[key(d)][id]; ok {
			return b.Close, true
		}
	}
	return 0, false
}
