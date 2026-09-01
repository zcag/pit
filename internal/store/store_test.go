package store

import (
	"path/filepath"
	"testing"

	"github.com/zcag/pit/internal/model"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func board(t *testing.T, s *Store) model.Board {
	t.Helper()
	b := model.Board{Name: "Side Projects", Description: "things I might build"}
	if err := s.CreateBoard(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func add(t *testing.T, s *Store, b model.Board, titles ...string) []model.Idea {
	t.Helper()
	in := make([]model.Idea, len(titles))
	for i, ti := range titles {
		in[i] = model.Idea{Title: ti, Bucket: "inbox", Source: model.SourceAgent}
	}
	out, err := s.AddIdeas(b.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBoardRoundTripIncludingDescription(t *testing.T) {
	s := open(t)
	b := board(t, s)

	got, err := s.Board("side-projects")
	if err != nil {
		t.Fatalf("lookup by slug: %v", err)
	}
	if got.Description != "things I might build" {
		t.Errorf("description = %q", got.Description)
	}
	if len(got.Buckets) != len(model.DefaultBuckets) {
		t.Errorf("buckets = %v", got.Buckets)
	}
	if _, err := s.Board(b.ID); err != nil {
		t.Errorf("lookup by id: %v", err)
	}
}

// A sparse patch must not blank the fields it does not mention.
func TestUpdateBoardIsSparse(t *testing.T) {
	s := open(t)
	board(t, s)
	name := "Renamed"
	got, err := s.UpdateBoard("side-projects", &name, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Description != "things I might build" {
		t.Errorf("sparse update cleared the description: %q", got.Description)
	}
}

func TestCustomBucketsAndTerminalValidation(t *testing.T) {
	s := open(t)
	b := model.Board{Name: "Writing", Buckets: []string{"drafting", "inbox", "binned"}, Terminal: "binned"}
	if err := s.CreateBoard(&b); err != nil {
		t.Fatal(err)
	}
	bad := model.Board{Name: "Bad", Buckets: []string{"a", "b"}, Terminal: "nope"}
	if err := s.CreateBoard(&bad); err == nil {
		t.Error("accepted a terminal bucket that is not in the bucket list")
	}
}

func TestAddIdeasIsAtomic(t *testing.T) {
	s := open(t)
	b := board(t, s)
	_, err := s.AddIdeas(b.ID, []model.Idea{
		{Title: "fine", Bucket: "inbox"},
		{Title: "   ", Bucket: "inbox"}, // invalid: kills the batch
	})
	if err == nil {
		t.Fatal("expected the batch to fail")
	}
	got, _ := s.ListIdeas(b.ID)
	if len(got) != 0 {
		t.Errorf("partial batch was committed: %d rows", len(got))
	}
}

func TestDuelMovesRatingsAndRecordsLog(t *testing.T) {
	s := open(t)
	b := board(t, s)
	its := add(t, s, b, "a", "b")

	if _, err := s.RecordDuel(b.ID, its[0].ID, its[1].ID); err != nil {
		t.Fatal(err)
	}
	w, _ := s.Idea(its[0].ID)
	l, _ := s.Idea(its[1].ID)
	if w.Rating <= l.Rating {
		t.Errorf("winner %v not above loser %v", w.Rating, l.Rating)
	}
	if w.Wins != 1 || l.Losses != 1 {
		t.Errorf("W/L not recorded: %d-%d vs %d-%d", w.Wins, w.Losses, l.Wins, l.Losses)
	}
	log, _ := s.DuelLog(b.ID)
	if len(log) != 1 {
		t.Fatalf("log has %d entries", len(log))
	}
}

func TestSelfDuelRejected(t *testing.T) {
	s := open(t)
	b := board(t, s)
	its := add(t, s, b, "only")
	if _, err := s.RecordDuel(b.ID, its[0].ID, its[0].ID); err == nil {
		t.Error("an idea was allowed to duel itself")
	}
}

// Undo must return the board to exactly the state before the duel, which is the
// payoff for keeping the log authoritative.
func TestUndoRestoresPreviousRatings(t *testing.T) {
	s := open(t)
	b := board(t, s)
	its := add(t, s, b, "a", "b", "c")

	if _, err := s.RecordDuel(b.ID, its[0].ID, its[1].ID); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ListIdeas(b.ID)

	if _, err := s.RecordDuel(b.ID, its[2].ID, its[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UndoLastDuel(b.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.ListIdeas(b.ID)

	byID := map[string]model.Idea{}
	for _, it := range before {
		byID[it.ID] = it
	}
	for _, it := range after {
		want := byID[it.ID]
		if diff := it.Rating - want.Rating; diff > 0.01 || diff < -0.01 {
			t.Errorf("%s rating %v after undo, want %v", it.Title, it.Rating, want.Rating)
		}
		if it.Wins != want.Wins || it.Losses != want.Losses {
			t.Errorf("%s record %d-%d after undo, want %d-%d", it.Title, it.Wins, it.Losses, want.Wins, want.Losses)
		}
	}
}

// A hand-placement is a weaker claim than evidence: the next duel overrides it.
func TestDuelClearsAHandPlacement(t *testing.T) {
	s := open(t)
	b := board(t, s)
	its := add(t, s, b, "a", "b")

	if _, err := s.PlaceIdea(its[0].ID, 1800); err != nil {
		t.Fatal(err)
	}
	placed, _ := s.Idea(its[0].ID)
	if !placed.Pinned {
		t.Fatal("placement did not pin")
	}
	if _, err := s.RecordDuel(b.ID, its[1].ID, its[0].ID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Idea(its[0].ID)
	if after.Pinned {
		t.Error("a duel did not clear the pin")
	}
}

func TestRecomputeIsIdempotent(t *testing.T) {
	s := open(t)
	b := board(t, s)
	its := add(t, s, b, "a", "b", "c")
	s.RecordDuel(b.ID, its[0].ID, its[1].ID)
	s.RecordDuel(b.ID, its[0].ID, its[2].ID)

	if err := s.Recompute(b.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := s.ListIdeas(b.ID)
	if err := s.Recompute(b.ID); err != nil {
		t.Fatal(err)
	}
	second, _ := s.ListIdeas(b.ID)

	for i := range first {
		if first[i].Rating != second[i].Rating {
			t.Errorf("recompute not idempotent for %s: %v then %v", first[i].Title, first[i].Rating, second[i].Rating)
		}
	}
}

// Buried ideas keep their row but must never appear in a ranking.
func TestBuriedIdeasDoNotRank(t *testing.T) {
	s := open(t)
	b := board(t, s)
	its := add(t, s, b, "alive", "buried")
	dead := "dead"
	if _, err := s.UpdateIdea(its[1].ID, IdeaPatch{Bucket: &dead}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListIdeas(b.ID)
	if got := len(Live(b, all)); got != 1 {
		t.Errorf("live ideas = %d, want 1", got)
	}
	if len(all) != 2 {
		t.Errorf("burying deleted the row: %d remain", len(all))
	}
}

func TestCommentsRoundTrip(t *testing.T) {
	s := open(t)
	b := board(t, s)
	its := add(t, s, b, "a")
	if _, err := s.AddComment(its[0].ID, model.SourceAgent, "I keep proposing this"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Idea(its[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Comments) != 1 || got.Comments[0].Who != model.SourceAgent {
		t.Errorf("comments = %+v", got.Comments)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Side Projects":   "side-projects",
		"  Home  Infra ":  "home-infra",
		"work / features": "work-features",
		"!!!":             "",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
