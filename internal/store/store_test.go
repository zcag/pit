package store

import (
	"database/sql"
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
		in[i] = model.Idea{Title: ti, Source: model.SourceAgent}
	}
	out, err := s.AddIdeas(b.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lanes(t *testing.T, s *Store, b model.Board, lane model.Lane) []string {
	t.Helper()
	ideas, err := s.ListIdeas(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range InLane(ideas, lane) {
		out = append(out, it.Title)
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
	if got.Label(model.In) != "in" {
		t.Errorf("default label = %q, want the lane's own name", got.Label(model.In))
	}
	if _, err := s.Board(b.ID); err != nil {
		t.Errorf("lookup by id: %v", err)
	}
}

func TestUpdateBoardIsSparseAndLabelsMerge(t *testing.T) {
	s := open(t)
	b := board(t, s)

	name := "Writing"
	if _, err := s.UpdateBoard(b.Slug, &name, nil, map[string]string{"in": "drafting"}); err != nil {
		t.Fatal(err)
	}
	desc := "what to write next"
	got, err := s.UpdateBoard(b.Slug, nil, &desc, map[string]string{"maybe": "someday"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Writing" || got.Description != desc {
		t.Errorf("sparse update lost a field: %+v", got)
	}
	if got.Label(model.In) != "drafting" || got.Label(model.Maybe) != "someday" {
		t.Errorf("labels merged wrong: %v", got.Labels)
	}
	// An empty string resets one lane back to its own name.
	got, err = s.UpdateBoard(b.Slug, nil, nil, map[string]string{"maybe": ""})
	if err != nil {
		t.Fatal(err)
	}
	if got.Label(model.Maybe) != "maybe" || got.Label(model.In) != "drafting" {
		t.Errorf("clearing one label disturbed the rest: %v", got.Labels)
	}
}

func TestLanesAreFixed(t *testing.T) {
	s := open(t)
	b := board(t, s)
	if _, err := s.UpdateBoard(b.Slug, nil, nil, map[string]string{"backlog": "Backlog"}); err == nil {
		t.Error("a board invented a lane; the spine must be fixed")
	}
	if _, err := s.AddIdeas(b.ID, []model.Idea{{Title: "x", Lane: "backlog"}}); err == nil {
		t.Error("an idea landed in a lane that does not exist")
	}
}

func TestAddIdeasIsAtomicAndLandsUnsorted(t *testing.T) {
	s := open(t)
	b := board(t, s)

	_, err := s.AddIdeas(b.ID, []model.Idea{{Title: "fine"}, {Title: "   "}})
	if err == nil {
		t.Fatal("expected a titleless idea to fail the batch")
	}
	all, _ := s.ListIdeas(b.ID)
	if len(all) != 0 {
		t.Fatalf("batch was not atomic: %d rows survived", len(all))
	}

	add(t, s, b, "one", "two", "three")
	if got := lanes(t, s, b, model.Unsorted); len(got) != 3 {
		t.Fatalf("ideas did not land unsorted: %v", got)
	}
	// Position is the arrival order, so the sift queue drains oldest first.
	if got := lanes(t, s, b, model.Unsorted); got[0] != "one" || got[2] != "three" {
		t.Errorf("queue order = %v, want arrival order", got)
	}
}

func TestSiftMovesAndAppendsToTheLane(t *testing.T) {
	s := open(t)
	b := board(t, s)
	ids := add(t, s, b, "one", "two", "three")

	for _, i := range []int{2, 0} { // vote three, then one
		if _, err := s.Move(b.ID, ids[i].ID, model.In, nil, model.NoReason); err != nil {
			t.Fatal(err)
		}
	}
	if got := lanes(t, s, b, model.In); len(got) != 2 || got[0] != "three" || got[1] != "one" {
		t.Errorf("in lane = %v, want the order they were sifted", got)
	}
	if got := lanes(t, s, b, model.Unsorted); len(got) != 1 || got[0] != "two" {
		t.Errorf("queue = %v, want only the unvoted card", got)
	}

	log, err := s.SiftLog(b.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 || log[0].To != model.In || log[0].From != model.Unsorted {
		t.Errorf("sift log = %+v", log)
	}
}

func TestUndoWalksBackDecisions(t *testing.T) {
	s := open(t)
	b := board(t, s)
	ids := add(t, s, b, "one", "two")

	if _, err := s.Move(b.ID, ids[0].ID, model.Out, nil, model.NoReason); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(b.ID, ids[1].ID, model.In, nil, model.NoReason); err != nil {
		t.Fatal(err)
	}

	back, err := s.UndoLast(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != ids[1].ID || back.Lane != model.Unsorted {
		t.Fatalf("undo returned %+v, want 'two' back in the queue", back)
	}
	if back.Pos != ids[1].Pos {
		t.Errorf("pos = %v, want the position it had (%v)", back.Pos, ids[1].Pos)
	}
	if _, err := s.UndoLast(b.ID); err != nil {
		t.Fatal(err)
	}
	if got := lanes(t, s, b, model.Unsorted); len(got) != 2 {
		t.Errorf("two undos left %v, want both cards back in the queue", got)
	}
	if _, err := s.UndoLast(b.ID); err != ErrNotFound {
		t.Errorf("undo past the start = %v, want ErrNotFound", err)
	}
}

func TestPlaceOrdersWithinALane(t *testing.T) {
	s := open(t)
	b := board(t, s)
	ids := add(t, s, b, "one", "two", "three")
	for _, it := range ids {
		if _, err := s.Move(b.ID, it.ID, model.In, nil, model.NoReason); err != nil {
			t.Fatal(err)
		}
	}
	// Drag "three" between "one" and "two".
	mid := (ids[0].Pos + ids[1].Pos) / 2
	got, err := s.Move(b.ID, ids[2].ID, model.In, &mid, model.NoReason)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pos != mid {
		t.Errorf("pos = %v, want the asserted %v", got.Pos, mid)
	}
	if order := lanes(t, s, b, model.In); order[1] != "three" {
		t.Errorf("in lane = %v, want three dragged into second place", order)
	}
}

func TestMoveRejectsAForeignIdea(t *testing.T) {
	s := open(t)
	a, other := board(t, s), model.Board{Name: "Other"}
	if err := s.CreateBoard(&other); err != nil {
		t.Fatal(err)
	}
	ids := add(t, s, a, "one")
	if _, err := s.Move(other.ID, ids[0].ID, model.In, nil, model.NoReason); err == nil {
		t.Error("an idea was sifted on a board it is not on")
	}
}

func TestCommentsRoundTripAndCount(t *testing.T) {
	s := open(t)
	b := board(t, s)
	ids := add(t, s, b, "one", "two")

	if _, err := s.AddComment(ids[0].ID, model.SourceAgent, "worth it because"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComment(ids[0].ID, model.SourceHuman, "agreed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComment(ids[0].ID, model.SourceHuman, "  "); err == nil {
		t.Error("an empty comment was accepted")
	}
	it, err := s.Idea(ids[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Comments) != 2 || it.Comments[0].Who != model.SourceAgent {
		t.Errorf("comments = %+v", it.Comments)
	}
	all, _ := s.ListIdeas(b.ID)
	for _, x := range all {
		want := 0
		if x.ID == ids[0].ID {
			want = 2
		}
		if x.CommentCount != want {
			t.Errorf("%s comment_count = %d, want %d", x.Title, x.CommentCount, want)
		}
	}
}

func TestListIdeasReadsBestLaneFirst(t *testing.T) {
	s := open(t)
	b := board(t, s)
	ids := add(t, s, b, "queued", "buried", "shortlisted", "second")
	for i, lane := range map[int]model.Lane{1: model.Out, 2: model.In, 3: model.Maybe} {
		if _, err := s.Move(b.ID, ids[i].ID, lane, nil, model.NoReason); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListIdeas(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"shortlisted", "second", "queued", "buried"}
	for i, x := range all {
		if x.Title != want[i] {
			t.Fatalf("order = %v at %d, want %v", x.Title, i, want)
		}
	}
	if c := Counts(all); c[model.In] != 1 || c[model.Unsorted] != 1 || c[model.Unsure] != 0 {
		t.Errorf("counts = %v", c)
	}
}

// A v1 database is the real one in production: bucket vocabularies, Glicko ratings
// and a duel log. Opening it must land every card in a lane, keep the order the
// duels earned, and carry over a custom bucket name as a label.
func TestMigrationFromBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pit.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `
CREATE TABLE boards (id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '', buckets TEXT NOT NULL, terminal TEXT NOT NULL,
  intake TEXT NOT NULL DEFAULT 'inbox', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE ideas (id TEXT PRIMARY KEY, board_id TEXT NOT NULL, title TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '', bucket TEXT NOT NULL,
  source TEXT NOT NULL, rating REAL NOT NULL, rd REAL NOT NULL, sigma REAL NOT NULL,
  wins INTEGER NOT NULL DEFAULT 0, losses INTEGER NOT NULL DEFAULT 0,
  pinned INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE comments (id TEXT PRIMARY KEY, idea_id TEXT NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
  who TEXT NOT NULL, text TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE duels (id TEXT PRIMARY KEY, board_id TEXT NOT NULL, winner_id TEXT NOT NULL,
  loser_id TEXT NOT NULL, voided INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
INSERT INTO boards VALUES ('b1','writing','Writing','',
  '["drafting","shortlist","inbox","parked","dead"]','dead','inbox','t','t');
INSERT INTO ideas VALUES ('i1','b1','essay','','','drafting','me',1700,60,.06,3,0,0,'t','t');
INSERT INTO ideas VALUES ('i2','b1','talk','','','inbox','agent',1500,350,.06,0,0,0,'t','t');
INSERT INTO ideas VALUES ('i3','b1','rant','','','dead','me',1300,80,.06,0,2,0,'t','t');
INSERT INTO ideas VALUES ('i4','b1','notes','','','parked','me',1600,90,.06,1,1,0,'t','t');
INSERT INTO comments VALUES ('c1','i1','agent','the case for it','t');
INSERT INTO duels VALUES ('d1','b1','i1','i3',0,'t');
`)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open a v1 database: %v", err)
	}
	defer s.Close()

	b, err := s.Board("writing")
	if err != nil {
		t.Fatal(err)
	}
	if b.Label(model.In) != "drafting" {
		t.Errorf("custom bucket name lost: labels = %v", b.Labels)
	}
	if b.Label(model.Out) != "out" {
		t.Errorf("stock bucket name carried over: labels = %v", b.Labels)
	}

	all, err := s.ListIdeas(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]model.Lane{"essay": model.In, "talk": model.Unsorted,
		"rant": model.Out, "notes": model.Unsure}
	if len(all) != 4 {
		t.Fatalf("migrated %d ideas, want 4", len(all))
	}
	for _, it := range all {
		if it.Lane != want[it.Title] {
			t.Errorf("%s landed in %q, want %q", it.Title, it.Lane, want[it.Title])
		}
		if it.Pos == 0 {
			t.Errorf("%s has no position", it.Title)
		}
	}
	if all[0].CommentCount != 1 {
		t.Errorf("comments lost their idea: %+v", all[0])
	}
	// The duel log is shelved, not deleted.
	var duels int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM duels`).Scan(&duels); err != nil || duels != 1 {
		t.Errorf("duel log = %d rows (%v), want it kept", duels, err)
	}
	// Opening again must be a no-op, not a second migration.
	s.Close()
	if s2, err := Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	} else {
		defer s2.Close()
		if again, _ := s2.ListIdeas(b.ID); len(again) != 4 {
			t.Errorf("reopen changed the board: %d ideas", len(again))
		}
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Side Projects":    "side-projects",
		"  Weird   Name  ": "weird-name",
		"Ünïcode!!":        "n-code",
		"---":              "",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// A burial says how a card ended, and only a burial does: the lane says it is
// off the board, the reason says whether that is a no or a shipped thing.
func TestMoveReason(t *testing.T) {
	s := open(t)
	b := model.Board{Name: "Reasons"}
	if err := s.CreateBoard(&b); err != nil {
		t.Fatal(err)
	}
	ids, err := s.AddIdeas(b.ID, []model.Idea{{Title: "built it"}})
	if err != nil {
		t.Fatal(err)
	}
	id := ids[0].ID

	if _, err := s.Move(b.ID, id, model.In, nil, model.NoReason); err != nil {
		t.Fatal(err)
	}
	it, err := s.Move(b.ID, id, model.Out, nil, model.Done)
	if err != nil {
		t.Fatal(err)
	}
	if it.Reason != model.Done {
		t.Errorf("reason = %q, want %q", it.Reason, model.Done)
	}

	// Undo takes back the whole state, not just the lane.
	if it, err = s.UndoLast(b.ID); err != nil {
		t.Fatal(err)
	}
	if it.Lane != model.In || it.Reason != model.NoReason {
		t.Errorf("after undo: lane=%q reason=%q, want in and no reason", it.Lane, it.Reason)
	}

	// Digging a card up un-ships it: a reason is meaningless anywhere but out.
	if _, err = s.Move(b.ID, id, model.Out, nil, model.Handed); err != nil {
		t.Fatal(err)
	}
	if it, err = s.Move(b.ID, id, model.Unsorted, nil, model.Done); err != nil {
		t.Fatal(err)
	}
	if it.Reason != model.NoReason {
		t.Errorf("reason survived a move out of out: %q", it.Reason)
	}

	if _, err := s.Move(b.ID, id, model.Out, nil, model.Reason("shipped-ish")); err == nil {
		t.Error("an unknown reason was accepted")
	}

	// And the log keeps what each move recorded.
	log, err := s.SiftLog(b.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var buried int
	for _, sf := range log {
		if sf.To == model.Out && sf.Reason != model.NoReason {
			buried++
		}
	}
	if buried != 1 {
		t.Errorf("log has %d reasoned burials, want 1 (the undone one is voided)", buried)
	}
}
