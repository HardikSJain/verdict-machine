package entities

// UnlinkedCandidates splits the candidates a store's map does not already
// carry into the ones the gates ACCEPTED and the ones they quarantined.
//
// This is the second half design §4.6 gives `verdict entities check`, and the
// reason the monthly ops item can fail. NSE reissues an ISIN 30 to 40 times a
// year; each one that the map has not been told about drops a name out of the
// point-in-time universe for months around its boundary, and nothing else in
// the system notices -- the universe is simply short one large cap, with no
// error and no missing row anywhere.
//
// The two results are different findings and must not be reported as one
// number. An accepted candidate the map does not carry means the seed has
// fallen behind the archive: the gates would link it, nothing has, and a
// roster PR is owed. A quarantined candidate is the queue -- 181 of them
// exist today, design §10 says plainly that nobody is going to work them, and
// a check that goes red every month for a backlog that will not move is a
// check an operator learns to skip.
//
// entityOf is read at the caller's pin (see market.Store.EntityMapAt). A pair
// counts as already linked only when BOTH symbols are present in that map and
// resolve to the same entity: a symbol the pinned map has never heard of is
// unknown, not linked. The two-value lookup is load-bearing rather than
// defensive -- a bare map read gives both missing symbols entity 0, compares
// 0 == 0, and reports the pair as safely linked.
func UnlinkedCandidates(cands []Candidate, entityOf map[int64]int64) (accepted, quarantined []Candidate) {
	for _, c := range cands {
		p, pok := entityOf[c.PredecessorID]
		s, sok := entityOf[c.SuccessorID]
		if pok && sok && p == s {
			continue
		}
		if c.Accepted() {
			accepted = append(accepted, c)
		} else {
			quarantined = append(quarantined, c)
		}
	}
	return accepted, quarantined
}
