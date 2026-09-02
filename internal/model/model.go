// Package model holds pit's domain types.
//
// The load-bearing decision: a Duel is the record of truth and a rating is a
// derived view of the duel log. Ratings live on Idea for cheap reads, but they
// can always be recomputed from scratch, so changing the rating system is a
// recompute rather than a migration.
package model

import "time"

// Source records who put an idea on the board. Agents may propose, annotate and
// edit; only a human may produce a duel, so ratings stay evidence of what the
// owner thinks rather than what a model guessed.
type Source string

const (
	SourceHuman Source = "me"
	SourceAgent Source = "agent"
)

// Board is one pile of ideas with its own vocabulary of buckets.
type Board struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	// Buckets is the ordered bucket vocabulary, decided-most first. The default
	// set ships with every board; a board may override it (a reading list wants
	// "drafting" where a project board wants "doing").
	Buckets []string `json:"buckets"`
	// Intake is where a new idea lands when the caller does not say. It cannot be
	// derived from position: in the default vocabulary the second-from-last
	// bucket is "parked", which would quietly file every pushed idea as already
	// considered-and-deferred.
	Intake string `json:"intake"`
	// Terminal is the graveyard bucket. Ideas there are buried, never deleted —
	// a killed idea is what stops you having it again, so it keeps earning its
	// row in the dedupe index.
	Terminal  string    `json:"terminal"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DefaultBuckets is the zero-setup vocabulary for a new board.
var DefaultBuckets = []string{"shortlist", "maybe", "inbox", "parked", "dead"}

const (
	DefaultTerminal = "dead"
	DefaultIntake   = "inbox"
)

// ResolveIntake picks a landing bucket for a custom vocabulary: the one named
// "inbox" if there is one, else the last bucket that is not the graveyard.
func ResolveIntake(buckets []string, terminal string) string {
	for _, b := range buckets {
		if b == DefaultIntake {
			return b
		}
	}
	for i := len(buckets) - 1; i >= 0; i-- {
		if buckets[i] != terminal {
			return buckets[i]
		}
	}
	return terminal
}

// Idea is one card. Rating/RD/Sigma are Glicko-2 state, derived from Duels.
type Idea struct {
	ID      string `json:"id"`
	BoardID string `json:"board_id"`
	Title   string `json:"title"`
	Note    string `json:"note"`
	Body    string `json:"body"`
	Bucket  string `json:"bucket"`
	Source  Source `json:"source"`

	Rating float64 `json:"rating"`
	RD     float64 `json:"rd"`
	Sigma  float64 `json:"sigma"`
	Wins   int     `json:"wins"`
	Losses int     `json:"losses"`

	// Pinned marks a rating set by hand (a drag) rather than earned in a duel.
	Pinned bool `json:"pinned"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Comments []Comment `json:"comments,omitempty"`
	// CommentCount is populated on list responses, where loading whole threads
	// for every card would be wasteful but the card still needs to show there
	// is something to read.
	CommentCount int `json:"comment_count"`
}

func (i Idea) Duels() int { return i.Wins + i.Losses }

// Comment is a note on an idea. Agents write here to argue their case, which is
// the only channel they have for influencing a ranking.
type Comment struct {
	ID        string    `json:"id"`
	IdeaID    string    `json:"idea_id"`
	Who       Source    `json:"who"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// Duel is one comparison. Append-only: an undo writes Voided rather than
// deleting the row, so the log stays a faithful record of what was asked.
type Duel struct {
	ID        string    `json:"id"`
	BoardID   string    `json:"board_id"`
	WinnerID  string    `json:"winner_id"`
	LoserID   string    `json:"loser_id"`
	Voided    bool      `json:"voided"`
	CreatedAt time.Time `json:"created_at"`
}
