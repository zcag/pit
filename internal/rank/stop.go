package rank

import (
	"math"
	"math/rand"
	"sort"
)

// Item is one rankable thing: an id plus its Glicko-2 state. rank deliberately
// knows nothing about ideas, boards or buckets.
type Item struct {
	ID string
	Player
}

// Outcome is one entry from the duel log.
type Outcome struct{ WinnerID, LoserID string }

// StableAt is the bootstrap agreement above which we call the top K settled.
const StableAt = 0.95

// Standings orders items best-first. Ties break on id so the order is stable
// across calls — a list that reshuffles on every render is unreadable.
func Standings(items []Item) []Item {
	out := append([]Item(nil), items...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].R != out[j].R {
			return out[i].R > out[j].R
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// TopK returns the ids of the best K, sorted, as a comparable set key.
func topKey(items []Item, k int) string {
	s := Standings(items)
	if k > len(s) {
		k = len(s)
	}
	ids := make([]string, k)
	for i := 0; i < k; i++ {
		ids[i] = s[i].ID
	}
	sort.Strings(ids)
	key := ""
	for _, id := range ids {
		key += id + "\x00"
	}
	return key
}

// Stability answers the only question that matters when deciding whether to
// keep dueling: if I had asked a slightly different set of questions, would I
// have landed on the same top K?
//
// It resamples the duel log with replacement, recomputes ratings from scratch
// for each resample, and reports the fraction of resamples whose top K matches
// the one the real log produced. 1.0 means the answer is robust to which
// comparisons happened to get asked; a low number means you got lucky, not
// finished.
//
// Note the resampling discards the log's ordering, which sequential Glicko-2
// does depend on. That is the standard bootstrap trade and it is conservative
// here: shuffled evidence produces slightly noisier ratings, so this under-
// reports stability rather than over-reporting it. Being told to do a few more
// duels than strictly necessary is the failure we want.
func Stability(items []Item, log []Outcome, k, samples int, rng *rand.Rand) float64 {
	if len(items) <= k {
		return 1 // nothing to be uncertain about: everything is in the top K
	}
	if len(log) < k {
		return 0 // not enough evidence to have an opinion at all
	}
	if samples <= 0 {
		samples = 200
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(1))
	}

	truth := topKey(items, k)
	hits := 0
	for s := 0; s < samples; s++ {
		draw := make([]Outcome, len(log))
		for i := range draw {
			draw[i] = log[rng.Intn(len(log))]
		}
		if topKey(replay(items, draw), k) == truth {
			hits++
		}
	}
	return float64(hits) / float64(samples)
}

// replay recomputes every rating from a duel log, starting from scratch. This
// is the function that makes the rating system swappable: ratings stored on an
// idea are a cache of this, never the source of truth.
func replay(items []Item, log []Outcome) []Item {
	state := make(map[string]Player, len(items))
	for _, it := range items {
		state[it.ID] = NewPlayer()
	}
	for _, o := range log {
		w, wok := state[o.WinnerID]
		l, lok := state[o.LoserID]
		if !wok || !lok {
			continue // an idea that has since been removed
		}
		state[o.WinnerID], state[o.LoserID] = Duel(w, l)
	}
	out := make([]Item, len(items))
	for i, it := range items {
		out[i] = Item{ID: it.ID, Player: state[it.ID]}
	}
	return out
}

// Replay is the exported recompute, used when the rating system changes or an
// undo voids a duel.
func Replay(items []Item, log []Outcome) []Item { return replay(items, log) }

// Contested returns the ideas whose uncertainty still straddles the top-K cut —
// the ones a further duel could actually reorder across the line. Everything
// else is either safely in or safely out, and asking about it is a wasted click.
func Contested(items []Item, k int) []Item {
	s := Standings(items)
	if len(s) <= k {
		return nil
	}
	kth := s[k-1]
	lo, hi := kth.R-kth.RD, kth.R+kth.RD
	var out []Item
	for _, it := range s {
		if it.R+it.RD > lo && it.R-it.RD < hi {
			out = append(out, it)
		}
	}
	return out
}

// Verdict is the answer to "can I stop now?".
type Verdict struct {
	Settled   bool    `json:"settled"`
	Stability float64 `json:"stability"`
	Contested int     `json:"contested"`
}

// Settled combines the two honest reasons to stop: nothing straddles the cut
// any more, or the top K survives resampling. It does not cover the third
// reason — a run of duels that changed nothing — because that is a property of
// a session rather than of the board, and belongs to the caller.
func Settled(items []Item, log []Outcome, k, samples int, rng *rand.Rand) Verdict {
	c := Contested(items, k)
	if len(c) == 0 {
		return Verdict{Settled: true, Stability: 1, Contested: 0}
	}
	st := Stability(items, log, k, samples, rng)
	return Verdict{Settled: st >= StableAt, Stability: st, Contested: len(c)}
}

// NextPair chooses the most informative comparison to put in front of the user.
//
// Most pairs teach nothing: you already know the top idea beats the bottom one,
// so asking wastes the scarcest resource in the whole system, which is the
// user's willingness to answer. The score prefers pairs that are close in
// rating, uncertain, near the top-K boundary, and not already over-asked.
//
// Returns false when there is nothing useful left to compare.
func NextPair(items []Item, k int, rng *rand.Rand) (Item, Item, bool) {
	if len(items) < 2 {
		return Item{}, Item{}, false
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(1))
	}
	s := Standings(items)
	cut := s[0].R
	if len(s) > k {
		cut = (s[k-1].R + s[k].R) / 2
	}

	best := math.Inf(-1)
	var a, b Item
	found := false
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			x, y := s[i], s[j]
			score := -math.Abs(x.R-y.R)/40 + // near-equal teaches most
				(x.RD+y.RD)/60 - // prefer the ideas we know least about
				math.Abs((x.R+y.R)/2-cut)/120 - // near the boundary that matters
				float64(duelCount(x)+duelCount(y))/24 + // spread the load
				rng.Float64()*0.6 // break ruts, keep it from feeling mechanical
			if score > best {
				best, a, b, found = score, x, y, true
			}
		}
	}
	return a, b, found
}

// duelCount approximates how much an item has been asked about from how narrow
// its RD has become; rank has no access to win/loss counts by design.
func duelCount(i Item) float64 {
	if i.RD <= MinRD {
		return 30
	}
	return math.Max(0, (DefaultRD-i.RD)/10)
}
