// Package model holds pit's domain types.
//
// The load-bearing decision: lanes are a fixed spine, not a vocabulary. Every
// board has the same five states, because the sift has exactly four keys and a
// queue to draw from — a bucket you cannot vote into is a bucket that fills up
// and never empties. Boards may rename a lane; they may not invent one.
package model

import "time"

// Source records who put an idea on the board. Agents may propose, annotate and
// edit; only a human may sift, so a lane stays evidence of what the owner
// thinks rather than what a model guessed.
type Source string

const (
	SourceHuman Source = "me"
	SourceAgent Source = "agent"
)

// Lane is where an idea rests. Unsorted is the queue the sift draws from;
// the other four are the four votes.
type Lane string

const (
	Unsorted Lane = "unsorted" // not yet looked at — the sift queue
	In       Lane = "in"       // yes
	Maybe    Lane = "maybe"    // a real idea, second tier
	Unsure   Lane = "unsure"   // looked at it, could not call it — owed a second pass
	Out      Lane = "out"      // no; buried, never deleted
)

// Lanes is every lane in board order, queue first.
var Lanes = []Lane{Unsorted, In, Maybe, Unsure, Out}

// Votes is what a sift can put a card into — Lanes minus the queue itself.
var Votes = []Lane{In, Maybe, Unsure, Out}

// Reason is how a card ended, which only `out` needs: the lane says a card is
// off the board, and this says whether that is because you said no or because
// it became real. Empty is the plain no — what out has always meant, and what
// a sift's left arrow still writes without asking.
//
// It is deliberately not a lane. A lane is somewhere the sift can put a card
// with one key, and "I built this" is not a judgement you make in a queue; it
// is something you record afterwards about a card you already kept.
type Reason string

const (
	NoReason Reason = ""       // no
	Done     Reason = "done"   // built it
	Handed   Reason = "handed" // it became work somewhere else
)

var Reasons = []Reason{NoReason, Done, Handed}

func ValidReason(r Reason) bool {
	for _, x := range Reasons {
		if x == r {
			return true
		}
	}
	return false
}

// Label is how a reason reads to a person.
func (r Reason) Label() string {
	switch r {
	case Done:
		return "done"
	case Handed:
		return "handed off"
	}
	return ""
}

func ValidLane(l Lane) bool {
	for _, x := range Lanes {
		if x == l {
			return true
		}
	}
	return false
}

// Board is one pile of ideas.
type Board struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Labels renames lanes for display. A writing board calls "in" drafting; the
	// lane is still "in", so the sift, the API and every agent keep one
	// vocabulary. Missing key means the lane's own name.
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

func (b Board) Label(l Lane) string {
	if s, ok := b.Labels[string(l)]; ok && s != "" {
		return s
	}
	return string(l)
}

// Idea is one card. Pos orders it within its lane, ascending: an idea sifted
// later lands below one sifted earlier, and a drag writes a value between its
// new neighbours.
type Idea struct {
	ID      string  `json:"id"`
	BoardID string  `json:"board_id"`
	Title   string  `json:"title"`
	Note    string  `json:"note"`
	Body    string  `json:"body"`
	Lane    Lane    `json:"lane"`
	Source  Source  `json:"source"`
	Pos     float64 `json:"pos"`
	// Reason is meaningful only while Lane is Out, and is cleared on the way to
	// any other lane — digging a card up un-ships it.
	Reason Reason `json:"reason"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Comments []Comment `json:"comments,omitempty"`
	// CommentCount is populated on list responses, where loading whole threads
	// for every card would be wasteful but the card still needs to show there
	// is something to read.
	CommentCount int `json:"comment_count"`
}

// Comment is a note on an idea. Agents write here to argue their case, which is
// the only channel they have for influencing where a card lands.
type Comment struct {
	ID        string    `json:"id"`
	IdeaID    string    `json:"idea_id"`
	Who       Source    `json:"who"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// Share is a read-only public view of one lane of one board — the way a
// shortlist leaves this machine without becoming a screenshot. The token is
// the whole credential, so it is long and random, and revoking one is deleting
// its row.
type Share struct {
	Token     string    `json:"token"`
	BoardID   string    `json:"board_id"`
	Lane      Lane      `json:"lane"`
	CreatedAt time.Time `json:"created_at"`
}

// Sift is one decision: a vote, or a drag. Append-only — an undo writes Voided
// and restores From/FromPos, so the log stays a faithful record of what was
// actually decided and undo is a one-row write.
type Sift struct {
	ID      string  `json:"id"`
	BoardID string  `json:"board_id"`
	IdeaID  string  `json:"idea_id"`
	From    Lane    `json:"from"`
	To      Lane    `json:"to"`
	FromPos float64 `json:"from_pos"`
	// Reason is what this move recorded; FromReason is what it replaced, so an
	// undo puts back the whole state and not just the lane.
	Reason     Reason    `json:"reason"`
	FromReason Reason    `json:"from_reason"`
	Voided     bool      `json:"voided"`
	CreatedAt  time.Time `json:"created_at"`
}
