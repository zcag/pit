// Package store is pit's SQLite persistence layer.
//
// The schema keeps duels in their own append-only table rather than folding
// them into idea ratings. Ratings on the ideas table are a cache: Recompute
// rebuilds them from the log, which is what lets the rating system change
// without a migration and what makes undo a one-row write.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/zcag/pit/internal/model"
	"github.com/zcag/pit/internal/rank"
	_ "modernc.org/sqlite" // pure-Go driver: CGO_ENABLED=0 cross-compiles cleanly
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS boards (
  id          TEXT PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  buckets     TEXT NOT NULL,
  terminal    TEXT NOT NULL,
  intake      TEXT NOT NULL DEFAULT 'inbox',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ideas (
  id         TEXT PRIMARY KEY,
  board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  title      TEXT NOT NULL,
  note       TEXT NOT NULL DEFAULT '',
  body       TEXT NOT NULL DEFAULT '',
  bucket     TEXT NOT NULL,
  source     TEXT NOT NULL,
  rating     REAL NOT NULL,
  rd         REAL NOT NULL,
  sigma      REAL NOT NULL,
  wins       INTEGER NOT NULL DEFAULT 0,
  losses     INTEGER NOT NULL DEFAULT 0,
  pinned     INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ideas_board ON ideas(board_id);

CREATE TABLE IF NOT EXISTS comments (
  id         TEXT PRIMARY KEY,
  idea_id    TEXT NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
  who        TEXT NOT NULL,
  text       TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_comments_idea ON comments(idea_id);

-- Append-only. An undo sets voided = 1; nothing is ever deleted, so the log
-- stays a faithful record of every question that was actually asked.
CREATE TABLE IF NOT EXISTS duels (
  id         TEXT PRIMARY KEY,
  board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  winner_id  TEXT NOT NULL,
  loser_id   TEXT NOT NULL,
  voided     INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_duels_board ON duels(board_id, created_at);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// SQLite writes serialise anyway; one connection avoids "database is locked"
	// entirely at the cost of nothing at personal scale.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func newID() string { return ulid.Make().String() }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// Slugify makes a URL-safe board slug. Kept deliberately dumb: ASCII only, so
// a slug never depends on locale.
func Slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

/* ---------------------------------- boards --------------------------------- */

func (s *Store) CreateBoard(b *model.Board) error {
	if b.Slug == "" {
		b.Slug = Slugify(b.Name)
	}
	if b.Slug == "" {
		return errors.New("board needs a name")
	}
	if len(b.Buckets) == 0 {
		b.Buckets = model.DefaultBuckets
	}
	if b.Terminal == "" {
		b.Terminal = model.DefaultTerminal
	}
	if !contains(b.Buckets, b.Terminal) {
		return fmt.Errorf("terminal bucket %q is not in the bucket list", b.Terminal)
	}
	if b.Intake == "" || !contains(b.Buckets, b.Intake) {
		b.Intake = model.ResolveIntake(b.Buckets, b.Terminal)
	}
	b.ID, b.CreatedAt, b.UpdatedAt = newID(), time.Now().UTC(), time.Now().UTC()
	buckets, _ := json.Marshal(b.Buckets)
	_, err := s.db.Exec(
		`INSERT INTO boards (id,slug,name,description,buckets,terminal,intake,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		b.ID, b.Slug, b.Name, b.Description, string(buckets), b.Terminal, b.Intake, now(), now())
	return err
}

func (s *Store) ListBoards() ([]model.Board, error) {
	rows, err := s.db.Query(`SELECT id,slug,name,description,buckets,terminal,intake,created_at,updated_at
	                          FROM boards ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Board resolves either a slug or an id, so URLs can stay readable while the
// API stays stable.
func (s *Store) Board(ref string) (model.Board, error) {
	row := s.db.QueryRow(`SELECT id,slug,name,description,buckets,terminal,intake,created_at,updated_at
	                       FROM boards WHERE slug = ? OR id = ?`, ref, ref)
	b, err := scanBoard(row)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

type scanner interface{ Scan(...any) error }

func scanBoard(sc scanner) (model.Board, error) {
	var b model.Board
	var buckets, created, updated string
	if err := sc.Scan(&b.ID, &b.Slug, &b.Name, &b.Description, &buckets, &b.Terminal, &b.Intake, &created, &updated); err != nil {
		return b, err
	}
	_ = json.Unmarshal([]byte(buckets), &b.Buckets)
	b.CreatedAt, b.UpdatedAt = parseTime(created), parseTime(updated)
	return b, nil
}

// UpdateBoard applies non-nil fields only, so a caller editing just the
// description cannot accidentally clear the bucket list.
func (s *Store) UpdateBoard(ref string, name, description *string, buckets *[]string, terminal, intake *string) (model.Board, error) {
	b, err := s.Board(ref)
	if err != nil {
		return b, err
	}
	if name != nil {
		b.Name = *name
	}
	if description != nil {
		b.Description = *description
	}
	if buckets != nil && len(*buckets) > 0 {
		b.Buckets = *buckets
	}
	if terminal != nil && *terminal != "" {
		b.Terminal = *terminal
	}
	if intake != nil && *intake != "" {
		b.Intake = *intake
	}
	if !contains(b.Buckets, b.Terminal) {
		return b, fmt.Errorf("terminal bucket %q is not in the bucket list", b.Terminal)
	}
	if !contains(b.Buckets, b.Intake) {
		b.Intake = model.ResolveIntake(b.Buckets, b.Terminal)
	}
	bs, _ := json.Marshal(b.Buckets)
	_, err = s.db.Exec(`UPDATE boards SET name=?,description=?,buckets=?,terminal=?,intake=?,updated_at=? WHERE id=?`,
		b.Name, b.Description, string(bs), b.Terminal, b.Intake, now(), b.ID)
	b.UpdatedAt = time.Now().UTC()
	return b, err
}

func (s *Store) DeleteBoard(ref string) error {
	b, err := s.Board(ref)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM boards WHERE id = ?`, b.ID)
	return err
}

/* ---------------------------------- ideas ---------------------------------- */

// AddIdeas inserts in one transaction and returns the created rows. Bulk is the
// primary path: an agent pushing thirty ideas one HTTP call at a time is how
// you get a half-written board when the fifteenth fails.
func (s *Store) AddIdeas(boardID string, in []model.Idea) ([]model.Idea, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	out := make([]model.Idea, 0, len(in))
	for _, it := range in {
		if strings.TrimSpace(it.Title) == "" {
			return nil, errors.New("idea needs a title")
		}
		it.ID, it.BoardID = newID(), boardID
		it.Rating, it.RD, it.Sigma = rank.DefaultR, rank.DefaultRD, rank.DefaultVol
		it.Wins, it.Losses, it.Pinned = 0, 0, false
		it.CreatedAt, it.UpdatedAt = time.Now().UTC(), time.Now().UTC()
		if it.Source == "" {
			it.Source = model.SourceHuman
		}
		if _, err := tx.Exec(
			`INSERT INTO ideas (id,board_id,title,note,body,bucket,source,rating,rd,sigma,wins,losses,pinned,created_at,updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,0,0,0,?,?)`,
			it.ID, it.BoardID, it.Title, it.Note, it.Body, it.Bucket, string(it.Source),
			it.Rating, it.RD, it.Sigma, now(), now()); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, tx.Commit()
}

func (s *Store) ListIdeas(boardID string) ([]model.Idea, error) {
	rows, err := s.db.Query(
		`SELECT id,board_id,title,note,body,bucket,source,rating,rd,sigma,wins,losses,pinned,created_at,updated_at
		 FROM ideas WHERE board_id = ? ORDER BY rating DESC, id`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Idea
	for rows.Next() {
		it, err := scanIdea(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// One extra query for the whole board rather than one per card.
	counts, err := s.commentCounts(boardID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].CommentCount = counts[out[i].ID]
	}
	return out, nil
}

func (s *Store) commentCounts(boardID string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT c.idea_id, COUNT(*) FROM comments c
	                          JOIN ideas i ON i.id = c.idea_id
	                          WHERE i.board_id = ? GROUP BY c.idea_id`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *Store) Idea(id string) (model.Idea, error) {
	row := s.db.QueryRow(
		`SELECT id,board_id,title,note,body,bucket,source,rating,rd,sigma,wins,losses,pinned,created_at,updated_at
		 FROM ideas WHERE id = ?`, id)
	it, err := scanIdea(row)
	if errors.Is(err, sql.ErrNoRows) {
		return it, ErrNotFound
	}
	if err != nil {
		return it, err
	}
	it.Comments, err = s.Comments(id)
	it.CommentCount = len(it.Comments)
	return it, err
}

func scanIdea(sc scanner) (model.Idea, error) {
	var it model.Idea
	var src, created, updated string
	var pinned int
	if err := sc.Scan(&it.ID, &it.BoardID, &it.Title, &it.Note, &it.Body, &it.Bucket, &src,
		&it.Rating, &it.RD, &it.Sigma, &it.Wins, &it.Losses, &pinned, &created, &updated); err != nil {
		return it, err
	}
	it.Source = model.Source(src)
	it.Pinned = pinned == 1
	it.CreatedAt, it.UpdatedAt = parseTime(created), parseTime(updated)
	return it, nil
}

// IdeaPatch is a sparse edit. Nil means "leave alone" — an agent fixing a typo
// in the body must not blank the note.
type IdeaPatch struct {
	Title  *string
	Note   *string
	Body   *string
	Bucket *string
}

func (s *Store) UpdateIdea(id string, p IdeaPatch) (model.Idea, error) {
	it, err := s.Idea(id)
	if err != nil {
		return it, err
	}
	if p.Title != nil {
		it.Title = *p.Title
	}
	if p.Note != nil {
		it.Note = *p.Note
	}
	if p.Body != nil {
		it.Body = *p.Body
	}
	if p.Bucket != nil {
		it.Bucket = *p.Bucket
	}
	_, err = s.db.Exec(`UPDATE ideas SET title=?,note=?,body=?,bucket=?,updated_at=? WHERE id=?`,
		it.Title, it.Note, it.Body, it.Bucket, now(), it.ID)
	it.UpdatedAt = time.Now().UTC()
	return it, err
}

// PlaceIdea sets a rating by hand — a drag. It marks the idea pinned so the UI
// can show that this position was asserted rather than earned.
func (s *Store) PlaceIdea(id string, rating float64) (model.Idea, error) {
	it, err := s.Idea(id)
	if err != nil {
		return it, err
	}
	it.Rating, it.Pinned = rating, true
	// A hand-placed rating is a weaker claim than a dueled one: widen RD so the
	// board still knows it has not been tested here.
	if it.RD < 90 {
		it.RD = 90
	}
	_, err = s.db.Exec(`UPDATE ideas SET rating=?,rd=?,pinned=1,updated_at=? WHERE id=?`,
		it.Rating, it.RD, now(), it.ID)
	return it, err
}

func (s *Store) DeleteIdea(id string) error {
	res, err := s.db.Exec(`DELETE FROM ideas WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

/* --------------------------------- comments -------------------------------- */

func (s *Store) AddComment(ideaID string, who model.Source, text string) (model.Comment, error) {
	if strings.TrimSpace(text) == "" {
		return model.Comment{}, errors.New("comment needs text")
	}
	c := model.Comment{ID: newID(), IdeaID: ideaID, Who: who, Text: text, CreatedAt: time.Now().UTC()}
	_, err := s.db.Exec(`INSERT INTO comments (id,idea_id,who,text,created_at) VALUES (?,?,?,?,?)`,
		c.ID, c.IdeaID, string(c.Who), c.Text, now())
	return c, err
}

func (s *Store) Comments(ideaID string) ([]model.Comment, error) {
	rows, err := s.db.Query(`SELECT id,idea_id,who,text,created_at FROM comments
	                          WHERE idea_id = ? ORDER BY created_at`, ideaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Comment
	for rows.Next() {
		var c model.Comment
		var who, created string
		if err := rows.Scan(&c.ID, &c.IdeaID, &who, &c.Text, &created); err != nil {
			return nil, err
		}
		c.Who, c.CreatedAt = model.Source(who), parseTime(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

/* ---------------------------------- duels ---------------------------------- */

// RecordDuel appends to the log and updates both ratings. The two happen in one
// transaction so a crash can never leave a rating that no duel explains.
func (s *Store) RecordDuel(boardID, winnerID, loserID string) (model.Duel, error) {
	var d model.Duel
	if winnerID == loserID {
		return d, errors.New("an idea cannot duel itself")
	}
	w, err := s.Idea(winnerID)
	if err != nil {
		return d, err
	}
	l, err := s.Idea(loserID)
	if err != nil {
		return d, err
	}
	if w.BoardID != boardID || l.BoardID != boardID {
		return d, errors.New("both ideas must be on the same board")
	}

	wp, lp := rank.Duel(
		rank.Player{R: w.Rating, RD: w.RD, Sigma: w.Sigma},
		rank.Player{R: l.Rating, RD: l.RD, Sigma: l.Sigma},
	)

	tx, err := s.db.Begin()
	if err != nil {
		return d, err
	}
	defer tx.Rollback()

	d = model.Duel{ID: newID(), BoardID: boardID, WinnerID: winnerID, LoserID: loserID, CreatedAt: time.Now().UTC()}
	if _, err := tx.Exec(`INSERT INTO duels (id,board_id,winner_id,loser_id,voided,created_at) VALUES (?,?,?,?,0,?)`,
		d.ID, d.BoardID, d.WinnerID, d.LoserID, now()); err != nil {
		return d, err
	}
	// A duel result overrides a hand-placement: the pin was a guess, this is evidence.
	if _, err := tx.Exec(`UPDATE ideas SET rating=?,rd=?,sigma=?,wins=wins+1,pinned=0,updated_at=? WHERE id=?`,
		wp.R, wp.RD, wp.Sigma, now(), winnerID); err != nil {
		return d, err
	}
	if _, err := tx.Exec(`UPDATE ideas SET rating=?,rd=?,sigma=?,losses=losses+1,pinned=0,updated_at=? WHERE id=?`,
		lp.R, lp.RD, lp.Sigma, now(), loserID); err != nil {
		return d, err
	}
	return d, tx.Commit()
}

// DuelLog returns the live (non-voided) log for a board, oldest first.
func (s *Store) DuelLog(boardID string) ([]rank.Outcome, error) {
	rows, err := s.db.Query(`SELECT winner_id,loser_id FROM duels
	                          WHERE board_id = ? AND voided = 0 ORDER BY created_at`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rank.Outcome
	for rows.Next() {
		var o rank.Outcome
		if err := rows.Scan(&o.WinnerID, &o.LoserID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UndoLastDuel voids the most recent duel on a board and recomputes. Cheap
// precisely because ratings are derived: no inverse-Glicko required.
func (s *Store) UndoLastDuel(boardID string) error {
	var id string
	err := s.db.QueryRow(`SELECT id FROM duels WHERE board_id = ? AND voided = 0
	                       ORDER BY created_at DESC LIMIT 1`, boardID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE duels SET voided = 1 WHERE id = ?`, id); err != nil {
		return err
	}
	return s.Recompute(boardID)
}

// Recompute rebuilds every rating on a board from the duel log. This is the
// function that makes the rating system replaceable and the log authoritative;
// it also repairs any drift from a hand-placement.
func (s *Store) Recompute(boardID string) error {
	ideas, err := s.ListIdeas(boardID)
	if err != nil {
		return err
	}
	log, err := s.DuelLog(boardID)
	if err != nil {
		return err
	}

	items := make([]rank.Item, len(ideas))
	for i, it := range ideas {
		items[i] = rank.Item{ID: it.ID, Player: rank.NewPlayer()}
	}
	items = rank.Replay(items, log)

	wins, losses := map[string]int{}, map[string]int{}
	for _, o := range log {
		wins[o.WinnerID]++
		losses[o.LoserID]++
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, it := range items {
		if _, err := tx.Exec(`UPDATE ideas SET rating=?,rd=?,sigma=?,wins=?,losses=?,pinned=0 WHERE id=?`,
			it.R, it.RD, it.Sigma, wins[it.ID], losses[it.ID], it.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

/* --------------------------------- ranking --------------------------------- */

// Live returns the ideas that are still in contention — everything outside the
// board's terminal bucket. Buried ideas keep their row but never rank.
func Live(b model.Board, ideas []model.Idea) []model.Idea {
	out := make([]model.Idea, 0, len(ideas))
	for _, it := range ideas {
		if it.Bucket != b.Terminal {
			out = append(out, it)
		}
	}
	return out
}

// Items adapts ideas to the rank package's view of them.
func Items(ideas []model.Idea) []rank.Item {
	out := make([]rank.Item, len(ideas))
	for i, it := range ideas {
		out[i] = rank.Item{ID: it.ID, Player: rank.Player{R: it.Rating, RD: it.RD, Sigma: it.Sigma}}
	}
	return out
}

// Verdict answers "is this board settled?" for the given top-K.
func (s *Store) Verdict(b model.Board, k, samples int) (rank.Verdict, error) {
	ideas, err := s.ListIdeas(b.ID)
	if err != nil {
		return rank.Verdict{}, err
	}
	log, err := s.DuelLog(b.ID)
	if err != nil {
		return rank.Verdict{}, err
	}
	return rank.Settled(Items(Live(b, ideas)), log, k, samples, rand.New(rand.NewSource(time.Now().UnixNano()))), nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
