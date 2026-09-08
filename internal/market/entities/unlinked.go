package entities

// UnlinkedCandidates splits the generated candidates three ways against a
// store's map: the ones the gates ACCEPTED that the map does not carry, the
// ones the gates quarantined that it does not carry either, and the ones it
// DOES carry that the gates no longer accept.
//
// This is the second half design §4.6 gives `verdict entities check`, and the
// reason the monthly ops item can fail. NSE reissues an ISIN 30 to 40 times a
// year; each one that the map has not been told about drops a name out of the
// point-in-time universe for months around its boundary, and nothing else in
// the system notices -- the universe is simply short one large cap, with no
// error and no missing row anywhere.
//
// The three results are different findings and must not be reported as one
// number. An accepted candidate the map does not carry means the seed has
// fallen behind the archive: the gates would link it, nothing has, and a
// roster PR is owed. A quarantined candidate is the queue -- 181 of them
// exist today, design §10 says plainly that nobody is going to work them, and
// a check that goes red every month for a backlog that will not move is a
// check an operator learns to skip.
//
// The third result is the RE-CHECK leg, and it is why the split is not
// two-way. Design §5.5 makes ratifying 444 irreversible merges wholesale
// acceptable on two grounds, and the second is that "every gate is
// independently re-checkable (`verdict entities check` re-evaluates them
// against the store)". A pair the store has already merged whose gates the
// current archive REJECTS is neither a seed gap nor a queue entry, so the
// earlier two-way version of this function dropped it on the floor before it
// ever consulted Accepted() -- and §5.6 establishes there is no other
// detector, so nothing in the system could say that a standing merge had
// stopped passing its own gates. The class is not hypothetical: §5.3 calls G4
// "the gate that kills demergers" and it counts sessions out of `bars`, so a
// backfill filling a hole inside a boundary gap moves a standing link from
// accepted to rejected without touching one row of `symbol_links`.
//
// stale is REPORTED and does not fail the run, which is a judgement call
// recorded in docs/DESIGN.md: a hand-written `manual` line exists precisely
// because it fails a gate -- the 52 `INF` fund-unit transfers fail G0 and G1
// by construction -- so once applied it lands here every month, forever, and
// a permanently red check is one an operator stops reading.
//
// entityOf is read at the caller's pin (see market.Store.EntityMapAt). A pair
// counts as already linked only when BOTH symbols are present in that map and
// resolve to the same entity: a symbol the pinned map has never heard of is
// unknown, not linked. The two-value lookup is load-bearing rather than
// defensive -- a bare map read gives both missing symbols entity 0, compares
// 0 == 0, and reports the pair as safely linked.
func UnlinkedCandidates(cands []Candidate, entityOf map[int64]int64) (accepted, quarantined, stale []Candidate) {
	for _, c := range cands {
		p, pok := entityOf[c.PredecessorID]
		s, sok := entityOf[c.SuccessorID]
		if pok && sok && p == s {
			if !c.Accepted() {
				stale = append(stale, c)
			}
			continue
		}
		if c.Accepted() {
			accepted = append(accepted, c)
		} else {
			quarantined = append(quarantined, c)
		}
	}
	return accepted, quarantined, stale
}
