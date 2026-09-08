package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/HardikSJain/verdict-machine/internal/market"
)

// StoreMarket is the Market over Postgres: the one a real backtest runs on.
//
// Every read carries the same pin. A run that pinned its bars but not its
// entity map, or its universe but not its returns, would be reproducible in
// some of its answers and not in others, which is worse than not being
// reproducible at all because it looks fine.
type StoreMarket struct {
	store    *market.Store
	source   string
	from, to time.Time
	lookback int
	n        int
	pin      time.Time
}

// NewStoreMarket wires the store to a date range and a universe rule.
//
// pin is the ingested_at bound: the moment the run claims to know the world
// as of. A backtest passes its own start time and records it, so a later
// backfill or a revised bhavcopy cannot change what the run saw.
func NewStoreMarket(s *market.Store, source string, from, to time.Time, lookback, n int, pin time.Time) (*StoreMarket, error) {
	if s == nil {
		return nil, fmt.Errorf("engine: a market store is required")
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("engine: from %s must be before to %s",
			from.Format(time.DateOnly), to.Format(time.DateOnly))
	}
	if lookback <= 0 || n <= 0 {
		return nil, fmt.Errorf("engine: lookback and universe size must be positive, got %d and %d", lookback, n)
	}
	if pin.IsZero() {
		return nil, fmt.Errorf("engine: a zero pin would make the run irreproducible; pass the run's start time")
	}
	return &StoreMarket{store: s, source: source, from: from, to: to, lookback: lookback, n: n, pin: pin}, nil
}

// Pin is the ingested_at bound every read uses, for a run row to record.
func (m *StoreMarket) Pin() time.Time { return m.pin }

// Source is the bar source, which is part of what makes a run reproducible:
// eod2 is split-adjusted in place and nse-bhavcopy is not, so the two answer
// different questions and a run must say which it asked.
func (m *StoreMarket) Source() string { return m.source }

func (m *StoreMarket) Sessions(ctx context.Context) ([]time.Time, error) {
	return m.store.Sessions(ctx, m.source, m.from, m.to, m.pin)
}

func (m *StoreMarket) Bars(ctx context.Context, date time.Time) (map[int64]Bar, error) {
	rows, err := m.store.BarsForDate(ctx, m.source, date, m.pin)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]Bar, len(rows))
	for _, b := range rows {
		out[b.EntityID] = Bar{
			EntityID: b.EntityID, Scrip: b.Ticker,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
		}
	}
	return out, nil
}

func (m *StoreMarket) Universe(ctx context.Context, asOf time.Time) ([]Member, error) {
	u, err := m.store.UniverseAsOf(ctx, m.source, asOf, m.lookback, m.n, m.pin)
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(u.Members))
	for i, mem := range u.Members {
		mm := Member{EntityID: mem.EntityID, Scrip: mem.Ticker, Rank: i + 1}
		if mem.LastBreak != nil {
			mm.LastBreak = *mem.LastBreak
		}
		out = append(out, mm)
	}
	return out, nil
}

// Returns forwards to the fence and keeps its three-way split intact.
func (m *StoreMarket) Returns(ctx context.Context, entityIDs []int64, from, to time.Time) (ReturnSet, error) {
	rs, err := m.store.EntityReturns(ctx, m.source, entityIDs, from, to, m.pin)
	if err != nil {
		return ReturnSet{}, err
	}
	out := ReturnSet{
		Priced:  make(map[int64]float64, len(rs.Priced)),
		Refused: make(map[int64]string, len(rs.Refused)),
		Absent:  make(map[int64]string, len(rs.Absent)),
	}
	for id, r := range rs.Priced {
		out.Priced[id] = r.Return
	}
	for id, r := range rs.Refused {
		out.Refused[id] = r.Error()
	}
	for id, why := range rs.Absent {
		out.Absent[id] = why
	}
	return out, nil
}
