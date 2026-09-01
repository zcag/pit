// Package rank turns a duel log into an order, and — more importantly — into an
// answer about whether that order can still change.
//
// Sorting everything is the wrong target: nobody needs #9 against #10, and a
// full noisy sort of n ideas costs far more comparisons than anyone will sit
// through. What is wanted is the top K as a *set*, and a defensible reason to
// stop asking. See Stability and Contested.
package rank

import "math"

// Glicko-2, per Glickman's "Example of the Glicko-2 system" (2013).
//
// Chosen over plain Elo because it carries a rating deviation per idea, which
// is what makes a stopping rule possible: without a measure of uncertainty
// there is no principled way to say the top five has settled. RD also grows
// while an idea sits untouched, so a board that has been ignored for a month
// correctly reports that it no longer knows.
const (
	scale       = 173.7178 // Glicko-2 internal scale
	DefaultR    = 1500.0
	DefaultRD   = 350.0
	DefaultVol  = 0.06
	Tau         = 0.5   // system constant; constrains volatility change over time
	convergence = 1e-6  // Illinois algorithm epsilon
	MinRD       = 30.0  // a rating is never certain enough to stop moving
	MaxRD       = 350.0 // an unplayed idea knows nothing
)

// Player is the Glicko-2 state of one idea.
type Player struct {
	R     float64
	RD    float64
	Sigma float64
}

// NewPlayer is an idea nobody has compared yet: mid-table and maximally unsure.
func NewPlayer() Player { return Player{R: DefaultR, RD: DefaultRD, Sigma: DefaultVol} }

// Result is one comparison against an opponent. Score is 1 for a win, 0 for a
// loss. pit never records draws — being unable to choose is a skip, which is
// deliberately not evidence about which is better.
type Result struct {
	Opp   Player
	Score float64
}

func g(phi float64) float64 { return 1 / math.Sqrt(1+3*phi*phi/(math.Pi*math.Pi)) }

func expected(mu, muj, phij float64) float64 {
	return 1 / (1 + math.Exp(-g(phij)*(mu-muj)))
}

// Update applies a rating period's results to p. A single duel is the
// one-element case, which is how the interactive loop uses it: the user's
// answer should move the board immediately, not at the end of some period.
func Update(p Player, results []Result) Player {
	if p.Sigma == 0 {
		p.Sigma = DefaultVol
	}
	if len(results) == 0 {
		return Decay(p, 1)
	}

	mu := (p.R - DefaultR) / scale
	phi := p.RD / scale

	// v: estimated variance of the rating based only on game outcomes.
	var vInv, dSum float64
	for _, res := range results {
		muj := (res.Opp.R - DefaultR) / scale
		phij := res.Opp.RD / scale
		gj := g(phij)
		e := expected(mu, muj, phij)
		vInv += gj * gj * e * (1 - e)
		dSum += gj * (res.Score - e)
	}
	if vInv == 0 {
		return Decay(p, 1)
	}
	v := 1 / vInv
	delta := v * dSum

	sigma := newVolatility(phi, v, delta, p.Sigma)

	phiStar := math.Sqrt(phi*phi + sigma*sigma)
	phiNew := 1 / math.Sqrt(1/(phiStar*phiStar)+1/v)
	muNew := mu + phiNew*phiNew*dSum

	return Player{
		R:     muNew*scale + DefaultR,
		RD:    clamp(phiNew*scale, MinRD, MaxRD),
		Sigma: sigma,
	}
}

// newVolatility is the Illinois-variant regula falsi from step 5 of the
// algorithm. It is the fiddly half of Glicko-2 and the reason most people stop
// at Glicko-1; it is also what keeps RD honest when a result is a genuine
// surprise, so it is worth the thirty lines.
func newVolatility(phi, v, delta, sigma float64) float64 {
	a := math.Log(sigma * sigma)
	f := func(x float64) float64 {
		ex := math.Exp(x)
		d2 := delta * delta
		p2 := phi * phi
		num := ex * (d2 - p2 - v - ex)
		den := 2 * (p2 + v + ex) * (p2 + v + ex)
		return num/den - (x-a)/(Tau*Tau)
	}

	A := a
	var B float64
	if d2, p2 := delta*delta, phi*phi; d2 > p2+v {
		B = math.Log(d2 - p2 - v)
	} else {
		k := 1.0
		for f(a-k*Tau) < 0 {
			k++
			if k > 100 { // pathological input; bail with the prior volatility
				return sigma
			}
		}
		B = a - k*Tau
	}

	fA, fB := f(A), f(B)
	for i := 0; math.Abs(B-A) > convergence && i < 100; i++ {
		C := A + (A-B)*fA/(fB-fA)
		fC := f(C)
		if fC*fB <= 0 {
			A, fA = B, fB
		} else {
			fA /= 2
		}
		B, fB = C, fC
	}
	return math.Exp(A / 2)
}

// Decay widens RD for a rating period in which an idea was not compared. This
// is what makes a stale board report that it is unsettled rather than quietly
// presenting a months-old answer as current.
func Decay(p Player, periods float64) Player {
	if p.Sigma == 0 {
		p.Sigma = DefaultVol
	}
	phi := p.RD / scale
	phi = math.Sqrt(phi*phi + periods*p.Sigma*p.Sigma)
	return Player{R: p.R, RD: clamp(phi*scale, MinRD, MaxRD), Sigma: p.Sigma}
}

// Duel applies one comparison to both sides and returns the updated pair. Both
// are computed from the pre-duel state, so order of application cannot matter.
func Duel(winner, loser Player) (Player, Player) {
	w := Update(winner, []Result{{Opp: loser, Score: 1}})
	l := Update(loser, []Result{{Opp: winner, Score: 0}})
	return w, l
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
