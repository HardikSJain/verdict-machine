package cost

import (
	"fmt"
	"sort"
	"time"
)

// The dated charge schedule.
//
// Rates change, and a backtest that runs 2011 to 2026 on today's rate card is
// wrong in a way that never announces itself. Two examples this table records
// rather than papers over: stamp duty had NO single national rate before
// 2020-07-01 (it was state-wise, and the Finance Act's uniform 0.015% only
// starts then), and STT on delivery was 0.125% before it became 0.1% somewhere
// around 2012-13, so a pre-2013 backtest run on today's rate understates the
// tax by a quarter.
//
// So every entry carries an EffectiveFrom, a Source, and a Verified flag that
// means one thing only: **a contract note or ledger held in this repository
// reconciles this rate to the paisa**. A rate card is not verification. When
// Trade() uses an unverified entry it says so in Charges.Unverified, and the
// backtest report is required to print that list beside its result.
//
// The pattern for a rate verified today but unresearched historically is two
// entries with the same value: one from the start of the archive marked
// unverified with the gap described, and one from the verification date marked
// verified. The later, verified entry wins for recent dates; older dates fall
// through to the honest one.

// Basis is how a rate turns into rupees.
type Basis string

const (
	// PercentOfBase is an ad valorem rate: Value is a fraction of turnover
	// (or, for GST, of the taxable value).
	PercentOfBase Basis = "percent"
	// CappedPercent is PercentOfBase clamped to [Floor, Cap]: Angel One's
	// "Rs20 or 0.1% per executed order, whichever is lower, minimum Rs5".
	CappedPercent Basis = "capped_percent"
	// Flat is a rupee amount that ignores the base: the DP charge.
	Flat Basis = "flat"
	// Zero is not levied. It is an entry rather than an absence so that "we
	// know this is nil" is distinguishable from "nobody filled this in".
	Zero Basis = "zero"
)

// Broker names whose brokerage and DP schedule to use. The algo executes on
// Zerodha; Angel One is here because the note that verifies the statutory
// lines is an Angel One note, and a golden test has to be able to reproduce
// the document it is built from.
type Broker string

const (
	Zerodha  Broker = "zerodha"
	AngelOne Broker = "angelone"
)

// archiveStart is the earliest date any entry claims to cover. It is the first
// session in the store, so a trade before it is a bug rather than a gap.
var archiveStart = time.Date(1995, 1, 1, 0, 0, 0, 0, time.UTC)

// noteDate is the contract note every Verified entry below was reconciled
// against: Angel One, NSE cash, 2026-09-07, BUY 98 NIFTYBEES at 272.29,
// turnover 26,684.42. See TestGoldenContractNote.
var noteDate = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

type rate struct {
	Component     Component
	Product       Product // "" matches any
	Side          Side    // "" matches any
	Broker        Broker  // "" matches any
	EffectiveFrom time.Time
	Basis         Basis
	Value         float64
	Cap           float64
	Floor         float64
	Verified      bool
	Source        string
}

// on turns a rate into rupees, rounded to the paisa. Every statutory line on
// the note rounds individually and to nearest, which is what reproduces
// 0.8192 -> 0.82, 0.02668 -> 0.03 and 4.0027 -> 4.00.
func (r rate) on(base float64) float64 {
	switch r.Basis {
	case Zero:
		return 0
	case Flat:
		return round2(r.Value)
	case PercentOfBase:
		return round2(base * r.Value)
	case CappedPercent:
		v := base * r.Value
		if r.Cap > 0 && v > r.Cap {
			v = r.Cap
		}
		if v < r.Floor {
			v = r.Floor
		}
		return round2(v)
	}
	return 0
}

func (r rate) specificity() int {
	n := 0
	if r.Product != "" {
		n++
	}
	if r.Side != "" {
		n++
	}
	if r.Broker != "" {
		n++
	}
	return n
}

// Schedule is one broker's dated view of the charge table.
type Schedule struct {
	broker Broker
	rates  []rate
}

// NewSchedule returns the schedule for a broker.
func NewSchedule(b Broker) (*Schedule, error) {
	if b != Zerodha && b != AngelOne {
		return nil, fmt.Errorf("cost: unknown broker %q", b)
	}
	return &Schedule{broker: b, rates: statutory()}, nil
}

// Broker reports whose brokerage and DP charges this schedule applies.
func (s *Schedule) Broker() Broker { return s.broker }

// rateFor picks the entry in force for a component on a date: the most
// specific match, and among equally specific ones the latest EffectiveFrom at
// or before the date.
func (s *Schedule) rateFor(comp Component, p Product, side Side, date time.Time) (rate, error) {
	var best rate
	var found bool
	for _, r := range s.rates {
		if r.Component != comp {
			continue
		}
		if r.Product != "" && r.Product != p {
			continue
		}
		if r.Side != "" && r.Side != side {
			continue
		}
		if r.Broker != "" && r.Broker != s.broker {
			continue
		}
		if date.Before(r.EffectiveFrom) {
			continue
		}
		if !found ||
			r.specificity() > best.specificity() ||
			(r.specificity() == best.specificity() && r.EffectiveFrom.After(best.EffectiveFrom)) {
			best, found = r, true
		}
	}
	if !found {
		return rate{}, fmt.Errorf(
			"cost: no %s rate for %s %s on %s (broker %s); the schedule refuses to guess",
			comp, p, side, date.Format(time.DateOnly), s.broker)
	}
	return best, nil
}

// Rates returns the whole table, for a report that wants to print what it
// used. It is a copy.
func (s *Schedule) Rates() []rate {
	out := append([]rate(nil), s.rates...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		return out[i].EffectiveFrom.Before(out[j].EffectiveFrom)
	})
	return out
}

// Verified reports whether a rate is backed by a reconciled document, and its
// source. Exported so a test can assert that the unverified set has not
// quietly grown.
func (r rate) IsVerified() bool      { return r.Verified }
func (r rate) Describe() string      { return string(r.Component) + " " + r.Source }
func (r rate) Component_() Component { return r.Component }

func statutory() []rate {
	return []rate{
		// ---- Brokerage -------------------------------------------------
		{
			Component: Brokerage, Broker: Zerodha, EffectiveFrom: archiveStart,
			Basis: Zero,
			Source: "zerodha.com/charges: Rs0 on equity delivery, waived Dec 2015. " +
				"UNVERIFIED: no Zerodha contract note has been reconciled yet, and the " +
				"pre-2015 rate is not in this table at all.",
		},
		{
			Component: Brokerage, Broker: AngelOne, EffectiveFrom: noteDate,
			Basis: CappedPercent, Value: 0.001, Cap: 20, Floor: 5, Verified: true,
			Source: "contract note 2026-09-07: Rs20.00 on turnover 26,684.42, where " +
				"0.1% would be 26.68, so the Rs20 cap binds. Rule per angelone.in: " +
				"Rs20 or 0.1% per executed order whichever is lower, minimum Rs5.",
		},
		{
			Component: Brokerage, Broker: AngelOne, EffectiveFrom: archiveStart,
			Basis: CappedPercent, Value: 0.001, Cap: 20, Floor: 5,
			Source: "UNVERIFIED before 2026-09-07. Angel One charged Rs0 on delivery " +
				"until 2024-11-01 and this table does not model that, so any backtest " +
				"before then overstates Angel One brokerage.",
		},

		// ---- STT -------------------------------------------------------
		{
			Component: STT, Product: EquityDelivery, EffectiveFrom: archiveStart,
			Basis: PercentOfBase, Value: 0.001,
			Source: "UNVERIFIED. 0.1% on both sides of a delivery trade per every " +
				"published rate card, but no stock contract note has been reconciled " +
				"here -- the only note held is an ETF buy, where STT is nil. The rate " +
				"was 0.125% before a cut around 2012-13 whose effective date is not " +
				"pinned, so pre-2013 backtests understate STT by a quarter.",
		},
		{
			Component: STT, Product: FundUnits, Side: Buy, EffectiveFrom: noteDate,
			Basis: Zero, Verified: true,
			Source: "contract note 2026-09-07: STT 0.00 on a 26,684.42 ETF buy. " +
				"Buying units of an equity-oriented fund carries no STT at all, which " +
				"is a different rule from the 0.1% on a share.",
		},
		{
			Component: STT, Product: FundUnits, Side: Buy, EffectiveFrom: archiveStart,
			Basis:  Zero,
			Source: "UNVERIFIED before 2026-09-07; same nil rate assumed throughout.",
		},
		{
			Component: STT, Product: FundUnits, Side: Sell, EffectiveFrom: archiveStart,
			Basis: PercentOfBase, Value: 0.00001,
			Source: "UNVERIFIED. 0.001% payable by the seller on units of an " +
				"equity-oriented fund sold on an exchange, per published rate cards. " +
				"The note held here is a BUY, so this side is unreconciled.",
		},

		// ---- Exchange transaction charges ------------------------------
		{
			Component: ExchangeTxn, EffectiveFrom: noteDate,
			Basis: PercentOfBase, Value: 0.000030699, Verified: true,
			Source: "contract note 2026-09-07: 0.82 on 26,684.42, which is 0.0030699%. " +
				"Note this is NOT the 0.00297% widely quoted for NSE cash after the " +
				"2024-10-01 revision; that rate reconciles to 0.79 and the note says 0.82.",
		},
		{
			Component: ExchangeTxn, EffectiveFrom: archiveStart,
			Basis: PercentOfBase, Value: 0.000030699,
			Source: "UNVERIFIED before 2026-09-07. NSE has revised cash transaction " +
				"charges repeatedly (one revision 2024-10-01) and the history is not " +
				"researched; today's rate is applied throughout.",
		},

		// ---- SEBI turnover fee -----------------------------------------
		{
			Component: SEBITurnover, EffectiveFrom: noteDate,
			Basis: PercentOfBase, Value: 0.000001, Verified: true,
			Source: "contract note 2026-09-07: 0.03 on 26,684.42, i.e. Rs10 per crore.",
		},
		{
			Component: SEBITurnover, EffectiveFrom: archiveStart,
			Basis: PercentOfBase, Value: 0.000001,
			Source: "UNVERIFIED before 2026-09-07. SEBI charged Rs20 per crore before " +
				"a cut whose effective date is not pinned, so earlier backtests halve " +
				"a fee that is negligible at this size anyway.",
		},

		// ---- Investor Protection Fund ----------------------------------
		{
			Component: IPF, EffectiveFrom: noteDate, Basis: Zero, Verified: true,
			Source: "contract note 2026-09-07: the IPF column is present and reads 0.00 " +
				"on an NSE cash trade. Whether NSE cash never levies it or it merely " +
				"did not apply to this trade is not established by one note.",
		},
		{
			Component: IPF, EffectiveFrom: archiveStart, Basis: Zero,
			Source: "UNVERIFIED before 2026-09-07; assumed nil throughout.",
		},

		// ---- Stamp duty (buy side only) --------------------------------
		{
			Component: StampDuty, Side: Buy, EffectiveFrom: noteDate,
			Basis: PercentOfBase, Value: 0.00015, Verified: true,
			Source: "contract note 2026-09-07: 4.00 on 26,684.42, i.e. 0.015%. Several " +
				"public sources quote 0.002% for this line; that is the non-delivery " +
				"rate and it reconciles to 0.53, not 4.00.",
		},
		{
			Component: StampDuty, Side: Buy,
			EffectiveFrom: time.Date(2020, 7, 1, 0, 0, 0, 0, time.UTC),
			Basis:         PercentOfBase, Value: 0.00015,
			Source: "Finance Act uniform rate for demat transfers, effective 2020-07-01, " +
				"collected centrally through the depositories. UNVERIFIED: no note from " +
				"this period has been reconciled.",
		},
		{
			Component: StampDuty, Side: Buy, EffectiveFrom: archiveStart,
			Basis: PercentOfBase, Value: 0.00015,
			Source: "WRONG BY CONSTRUCTION and flagged so it cannot be forgotten: before " +
				"2020-07-01 stamp duty on securities was levied state by state and there " +
				"was no single national rate to apply. Every backtest session before " +
				"2020-07-01 carries this flag.",
		},
		{
			Component: StampDuty, Side: Sell, EffectiveFrom: archiveStart,
			Basis: Zero, Verified: true,
			Source: "Stamp duty is levied on the buyer only. The 2026-09-07 note is a " +
				"buy and carries the line; the rule itself is not in dispute.",
		},

		// ---- GST -------------------------------------------------------
		{
			Component: GST, EffectiveFrom: noteDate,
			Basis: PercentOfBase, Value: 0.18, Verified: true,
			Source: "contract note 2026-09-07: taxable value 20.85 (brokerage + exchange " +
				"+ SEBI + IPF), GST 3.75, which is 18% rounded ONCE. The note PRINTS " +
				"CGST 1.88 and SGST 1.88, summing to 3.76 -- a display split, each half " +
				"rounded independently. The obligation total proves 3.75 was debited.",
		},
		{
			Component: GST, EffectiveFrom: time.Date(2017, 7, 1, 0, 0, 0, 0, time.UTC),
			Basis: PercentOfBase, Value: 0.18,
			Source: "GST at 18% replaced service tax on 2017-07-01. UNVERIFIED for this " +
				"period: no note from it has been reconciled.",
		},
		{
			Component: GST, EffectiveFrom: archiveStart,
			Basis: PercentOfBase, Value: 0.18,
			Source: "WRONG BY CONSTRUCTION before 2017-07-01: service tax applied then, " +
				"at 15% in 2016-17 and lower earlier, and its history is not researched. " +
				"Applying 18% overstates the levy on every pre-GST session.",
		},

		// ---- DP charge (sell side, per scrip per day) ------------------
		{
			Component: DPCharge, Broker: Zerodha, Side: Sell, EffectiveFrom: archiveStart,
			Basis: Flat, Value: 15.34,
			Source: "zerodha.com/charges: Rs13 + 18% GST = Rs15.34 per scrip per sell " +
				"day (Rs12.75 + GST = Rs15.05 where the primary holder is a woman). " +
				"UNVERIFIED: the DP charge appears on no contract note -- it is on the " +
				"funds statement -- and no Zerodha ledger has been reconciled here.",
		},
		{
			Component: DPCharge, Broker: AngelOne, Side: Sell, EffectiveFrom: archiveStart,
			Basis: Flat, Value: 23.60,
			Source: "angelone.in: Rs20 + 18% GST = Rs23.60 per ISIN per sell. " +
				"UNVERIFIED: the note held here is a buy, so it carries no DP line.",
		},
	}
}
