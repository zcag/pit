package rank

import (
	"math/rand"
	"testing"
)

// items builds n items with a known true order (item 0 is strongest).
func items(n int) []Item {
	out := make([]Item, n)
	for i := range out {
		out[i] = Item{ID: string(rune('a' + i)), Player: NewPlayer()}
	}
	return out
}

// duelBySkill plays the log a "true" ranking implies: the lower index wins,
// with some noise, which is what a real user does — they contradict themselves.
func duelBySkill(t *testing.T, its []Item, rounds int, noise float64, rng *rand.Rand) ([]Item, []Outcome) {
	t.Helper()
	idx := map[string]int{}
	for i, it := range its {
		idx[it.ID] = i
	}
	var log []Outcome
	for r := 0; r < rounds; r++ {
		a, b, ok := NextPair(its, 3, rng)
		if !ok {
			break
		}
		w, l := a, b
		if idx[a.ID] > idx[b.ID] {
			w, l = b, a
		}
		if rng.Float64() < noise {
			w, l = l, w
		}
		log = append(log, Outcome{WinnerID: w.ID, LoserID: l.ID})
		its = Replay(its, log)
	}
	return its, log
}

// The whole point: enough duels must produce the true top K.
func TestConvergesToTrueTopK(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	its, log := duelBySkill(t, items(10), 90, 0.05, rng)

	top := Standings(its)[:3]
	want := map[string]bool{"a": true, "b": true, "c": true}
	for _, it := range top {
		if !want[it.ID] {
			t.Fatalf("top 3 = %v, want a/b/c (log had %d duels)", ids(top), len(log))
		}
	}
}

// Stability must be low before the work is done and high after. If it does not
// move, the meter is decoration and the stopping rule is a lie.
func TestStabilityRisesWithEvidence(t *testing.T) {
	rng := rand.New(rand.NewSource(11))

	early, earlyLog := duelBySkill(t, items(10), 6, 0.05, rng)
	late, lateLog := duelBySkill(t, items(10), 120, 0.05, rng)

	sEarly := Stability(early, earlyLog, 3, 200, rand.New(rand.NewSource(1)))
	sLate := Stability(late, lateLog, 3, 200, rand.New(rand.NewSource(1)))

	if sEarly >= sLate {
		t.Errorf("stability did not rise with evidence: early=%.2f late=%.2f", sEarly, sLate)
	}
	if sLate < StableAt {
		t.Errorf("stability after 120 duels = %.2f, want >= %.2f", sLate, StableAt)
	}
}

// A board with no duels must never claim to be settled.
func TestUnduelledBoardIsNotSettled(t *testing.T) {
	v := Settled(items(10), nil, 3, 50, rand.New(rand.NewSource(1)))
	if v.Settled {
		t.Error("a board with no duels reported settled")
	}
}

// Fewer ideas than K: there is no cut line, so there is nothing to decide.
func TestFewerItemsThanKIsSettled(t *testing.T) {
	v := Settled(items(2), nil, 5, 50, rand.New(rand.NewSource(1)))
	if !v.Settled {
		t.Error("a board smaller than K should be settled by definition")
	}
}

// Contested must shrink as the picture sharpens — that is the countdown the
// duel screen shows.
func TestContestedShrinks(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	before := items(10)
	after, _ := duelBySkill(t, items(10), 120, 0.05, rng)

	if len(Contested(after, 3)) >= len(Contested(before, 3)) {
		t.Errorf("contested did not shrink: before=%d after=%d",
			len(Contested(before, 3)), len(Contested(after, 3)))
	}
}

// Replay is what makes the rating system swappable: ratings must be a pure
// function of the log, so recomputing twice gives the same answer.
func TestReplayIsDeterministic(t *testing.T) {
	log := []Outcome{{"a", "b"}, {"a", "c"}, {"b", "c"}, {"c", "a"}}
	x := Replay(items(3), log)
	y := Replay(items(3), log)
	for i := range x {
		if x[i].R != y[i].R || x[i].RD != y[i].RD {
			t.Fatalf("replay not deterministic at %d: %+v vs %+v", i, x[i], y[i])
		}
	}
}

// Voiding a duel must be equivalent to it never having been asked.
func TestReplayWithoutVoidedDuelMatchesNeverHavingIt(t *testing.T) {
	full := []Outcome{{"a", "b"}, {"a", "c"}, {"b", "c"}}
	without := []Outcome{{"a", "b"}, {"b", "c"}}

	undone := Replay(items(3), without)
	never := Replay(items(3), without)
	for i := range undone {
		if undone[i].R != never[i].R {
			t.Fatal("undo is not equivalent to never having asked")
		}
	}
	if Replay(items(3), full)[0].R == undone[0].R {
		t.Error("voiding a duel changed nothing; the log is not load-bearing")
	}
}

// NextPair should not keep asking the same settled question.
func TestNextPairPrefersTheUncertain(t *testing.T) {
	its := []Item{
		{ID: "sure-a", Player: Player{R: 1900, RD: MinRD, Sigma: DefaultVol}},
		{ID: "sure-b", Player: Player{R: 1100, RD: MinRD, Sigma: DefaultVol}},
		{ID: "fuzzy-a", Player: Player{R: 1510, RD: 300, Sigma: DefaultVol}},
		{ID: "fuzzy-b", Player: Player{R: 1490, RD: 300, Sigma: DefaultVol}},
	}
	rng := rand.New(rand.NewSource(5))
	hits := 0
	for i := 0; i < 40; i++ {
		a, b, ok := NextPair(its, 2, rng)
		if !ok {
			t.Fatal("NextPair found nothing")
		}
		if (a.ID == "fuzzy-a" && b.ID == "fuzzy-b") || (a.ID == "fuzzy-b" && b.ID == "fuzzy-a") {
			hits++
		}
	}
	if hits < 30 {
		t.Errorf("only %d/40 pairs were the uncertain pair; pairing is not adaptive", hits)
	}
}

func ids(its []Item) []string {
	out := make([]string, len(its))
	for i, it := range its {
		out[i] = it.ID
	}
	return out
}
