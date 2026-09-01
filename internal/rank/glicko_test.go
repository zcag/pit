package rank

import (
	"math"
	"testing"
)

// The canonical check: Glickman's own worked example from "Example of the
// Glicko-2 system" (2013). A player at 1500/200 with volatility 0.06 plays
// three opponents, winning the first and losing the next two, with tau = 0.5.
// Expected: r' ~= 1464.06, RD' ~= 151.52, sigma' ~= 0.05999.
//
// If this test fails, the volatility iteration is wrong and every stopping-rule
// decision downstream is built on sand.
func TestGlickmanWorkedExample(t *testing.T) {
	p := Player{R: 1500, RD: 200, Sigma: 0.06}
	got := Update(p, []Result{
		{Opp: Player{R: 1400, RD: 30, Sigma: 0.06}, Score: 1},
		{Opp: Player{R: 1550, RD: 100, Sigma: 0.06}, Score: 0},
		{Opp: Player{R: 1700, RD: 300, Sigma: 0.06}, Score: 0},
	})

	near(t, "rating", got.R, 1464.06, 0.02)
	near(t, "rd", got.RD, 151.52, 0.02)
	near(t, "sigma", got.Sigma, 0.059996, 1e-5)
}

func TestDuelIsSymmetricAndZeroSumInDirection(t *testing.T) {
	a, b := NewPlayer(), NewPlayer()
	w, l := Duel(a, b)
	if w.R <= a.R {
		t.Errorf("winner rating did not rise: %v -> %v", a.R, w.R)
	}
	if l.R >= b.R {
		t.Errorf("loser rating did not fall: %v -> %v", b.R, l.R)
	}
	// Equal players, equal and opposite movement.
	near(t, "symmetry", (w.R-a.R)-(b.R-l.R), 0, 1e-9)
}

// A duel must always teach us something: RD narrows after evidence arrives.
func TestDuelNarrowsUncertainty(t *testing.T) {
	a, b := NewPlayer(), NewPlayer()
	w, _ := Duel(a, b)
	if w.RD >= a.RD {
		t.Errorf("RD did not narrow after a duel: %v -> %v", a.RD, w.RD)
	}
}

// Beating someone far weaker should barely move you; beating someone far
// stronger should move you a lot. This is the property that makes adaptive
// pairing worth doing at all.
func TestUpsetMovesMoreThanExpectedResult(t *testing.T) {
	me := Player{R: 1500, RD: 80, Sigma: 0.06}
	weak := Player{R: 1100, RD: 80, Sigma: 0.06}
	strong := Player{R: 1900, RD: 80, Sigma: 0.06}

	vsWeak, _ := Duel(me, weak)
	vsStrong, _ := Duel(me, strong)

	expectedGain := vsWeak.R - me.R
	upsetGain := vsStrong.R - me.R
	if upsetGain <= expectedGain {
		t.Errorf("upset (%.2f) should move more than the expected result (%.2f)", upsetGain, expectedGain)
	}
}

// An idea nobody touches must get *less* certain, not stay frozen — otherwise a
// board reports a stale answer with false confidence.
func TestDecayWidensRD(t *testing.T) {
	p := Player{R: 1500, RD: 60, Sigma: 0.06}
	d := Decay(p, 30)
	if d.RD <= p.RD {
		t.Errorf("RD did not widen over idle periods: %v -> %v", p.RD, d.RD)
	}
	if d.R != p.R {
		t.Errorf("decay must not move the rating: %v -> %v", p.R, d.R)
	}
}

func TestRDIsClamped(t *testing.T) {
	p := Player{R: 1500, RD: 350, Sigma: 0.06}
	if got := Decay(p, 1000); got.RD > MaxRD {
		t.Errorf("RD exceeded MaxRD: %v", got.RD)
	}
	hot := Player{R: 1500, RD: MinRD, Sigma: 0.06}
	for i := 0; i < 200; i++ {
		hot, _ = Duel(hot, Player{R: 1000, RD: MinRD, Sigma: 0.06})
	}
	if hot.RD < MinRD {
		t.Errorf("RD fell below MinRD: %v", hot.RD)
	}
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f (+/- %g)", what, got, want, tol)
	}
}
