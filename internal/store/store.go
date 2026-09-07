// Package store is pit's SQLite persistence layer.
//
// The schema keeps decisions in their own append-only table rather than only in
// the lane column on ideas. An idea's lane and position are the current state;
// the sifts log is how it got there, which is what makes undo a one-row write
// and what lets a board tell you what you decided last Tuesday.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/zcag/pit/internal/model"
	_ "modernc.org/sqlite" // pure-Go driver: CGO_ENABLED=0 cross-compiles cleanly
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

// schemaVersion 2 replaced the per-board bucket vocabulary with the fixed lane
// spine and made the sift, not the duel, the thing that moves a card.
const schemaVersion = 2

const schema = `
CREATE TABLE IF NOT EXISTS boards (
  id          TEXT PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  labels      TEXT NOT NULL DEFAULT '{}',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ideas (
  id         TEXT PRIMARY KEY,
  board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  title      TEXT NOT NULL,
  note       TEXT NOT NULL DEFAULT '',
  body       TEXT NOT NULL DEFAULT '',
  lane       TEXT NOT NULL,
  source     TEXT NOT NULL,
  pos        REAL NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ideas_board ON ideas(board_id, lane, pos);

CREATE TABLE IF NOT EXISTS comments (
  id         TEXT PRIMARY KEY,
  idea_id    TEXT NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
  who        TEXT NOT NULL,
  text       TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_comments_idea ON comments(idea_id);

-- Append-only. An undo sets voided = 1 and puts the card back where it was;
-- nothing is ever deleted, so the log stays a record of every call you made.
CREATE TABLE IF NOT EXISTS sifts (
  id         TEXT PRIMARY KEY,
  board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  idea_id    TEXT NOT NULL,
  from_lane  TEXT NOT NULL,
  to_lane    TEXT NOT NULL,
  from_pos   REAL NOT NULL,
  voided     INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sifts_board ON sifts(board_id, created_at);

-- Shelved with the duel (see README). The rows are kept: if pairwise ranking
-- comes back it comes back with its history, not from zero.
CREATE TABLE IF NOT EXISTS duels (
  id         TEXT PRIMARY KEY,
  board_id   TEXT NOT NULL,
  winner_id  TEXT NOT NULL,
  loser_id   TEXT NOT NULL,
  voided     INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
`

// laneOrder sorts a board best-first for anything that reads it as a list.
const laneOrder = `CASE lane WHEN 'in' THEN 0 WHEN 'maybe' THEN 1 WHEN 'unsure' THEN 2
                            WHEN 'unsorted' THEN 3 ELSE 4 END, pos, id`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// SQLite writes serialise anyway; one connection avoids "database is locked"
	// entirely at the cost of nothing at personal scale.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

/* -------------------------------- migration -------------------------------- */

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return nil
	}
	legacy, err := s.hasColumn("boards", "buckets")
	if err != nil {
		return err
	}
	if legacy {
		if err := s.migrateBucketsToLanes(); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

func (s *Store) hasColumn(table, col string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// migrateBucketsToLanes rebuilds boards and ideas on the lane spine. Ratings
// become positions in rank order, so the ordering a duel earned survives as the
// hand order of the new board; the duel log itself is left alone.
func (s *Store) migrateBucketsToLanes() error {
	type oldBoard struct{ id, buckets, terminal, intake string }
	var obs []oldBoard
	rows, err := s.db.Query(`SELECT id,buckets,terminal,intake FROM boards`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var o oldBoard
		if err := rows.Scan(&o.id, &o.buckets, &o.terminal, &o.intake); err != nil {
			rows.Close()
			return err
		}
		obs = append(obs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	labels := map[string]string{}    // board id -> labels JSON
	lanes := map[string]model.Lane{} // idea id -> lane
	for _, o := range obs {
		var buckets []string
		_ = json.Unmarshal([]byte(o.buckets), &buckets)
		lbl := map[string]string{}
		for _, bk := range buckets {
			l := laneOf(bk, o.intake, o.terminal)
			// Keep a genuinely custom name ("drafting"); drop the defaults, which
			// the new vocabulary replaces outright.
			if _, taken := lbl[string(l)]; !taken && !stock(bk) {
				lbl[string(l)] = bk
			}
		}
		j, _ := json.Marshal(lbl)
		labels[o.id] = string(j)

		ir, err := s.db.Query(`SELECT id,bucket FROM ideas WHERE board_id = ? ORDER BY rating DESC, id`, o.id)
		if err != nil {
			return err
		}
		for ir.Next() {
			var id, bucket string
			if err := ir.Scan(&id, &bucket); err != nil {
				ir.Close()
				return err
			}
			lanes[id] = laneOf(bucket, o.intake, o.terminal)
		}
		ir.Close()
		if err := ir.Err(); err != nil {
			return err
		}
	}

	// The rename must not rewrite the foreign keys that point at these tables:
	// comments references ideas(id) and has to keep pointing at the table we are
	// about to recreate under that name, not follow it to the archive copy.
	if _, err := s.db.Exec(`PRAGMA legacy_alter_table = ON; PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer s.db.Exec(`PRAGMA legacy_alter_table = OFF`)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`ALTER TABLE boards RENAME TO boards_v1;
	                      ALTER TABLE ideas  RENAME TO ideas_v1`); err != nil {
		return err
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	for id, lbl := range labels {
		if _, err := tx.Exec(
			`INSERT INTO boards (id,slug,name,description,labels,created_at,updated_at)
			 SELECT id,slug,name,description,?,created_at,updated_at FROM boards_v1 WHERE id = ?`,
			lbl, id); err != nil {
			return err
		}
	}
	// One position sequence per board, in the order the old ratings gave.
	seq := map[string]float64{}
	ir, err := tx.Query(`SELECT id,board_id FROM ideas_v1 ORDER BY board_id, rating DESC, id`)
	if err != nil {
		return err
	}
	type row struct{ id, board string }
	var all []row
	for ir.Next() {
		var r row
		if err := ir.Scan(&r.id, &r.board); err != nil {
			ir.Close()
			return err
		}
		all = append(all, r)
	}
	ir.Close()
	if err := ir.Err(); err != nil {
		return err
	}
	for _, r := range all {
		seq[r.board]++
		lane := lanes[r.id]
		if lane == "" {
			lane = model.Unsorted
		}
		if _, err := tx.Exec(
			`INSERT INTO ideas (id,board_id,title,note,body,lane,source,pos,created_at,updated_at)
			 SELECT id,board_id,title,note,body,?,source,?,created_at,updated_at FROM ideas_v1 WHERE id = ?`,
			string(lane), seq[r.board], r.id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP TABLE ideas_v1; DROP TABLE boards_v1`); err != nil {
		return err
	}
	return tx.Commit()
}

// laneOf maps a v1 bucket onto the spine. The board's own intake and terminal
// win over the name, because a board could rename either.
func laneOf(bucket, intake, terminal string) model.Lane {
	switch bucket {
	case terminal:
		return model.Out
	case intake:
		return model.Unsorted
	}
	switch strings.ToLower(bucket) {
	case "shortlist", "drafting", "doing", "in", "yes", "now":
		return model.In
	case "parked", "unsure", "someday", "later":
		return model.Unsure
	case "inbox", "unsorted":
		return model.Unsorted
	case "dead", "out", "no":
		return model.Out
	}
	return model.Maybe
}

// stock reports whether a v1 bucket name is one the new spine already says
// better, and so is not worth carrying over as a label.
func stock(bucket string) bool {
	switch strings.ToLower(bucket) {
	case "shortlist", "maybe", "inbox", "parked", "dead", "in", "out", "unsure", "unsorted":
		return true
	}
	return false
}

/* --------------------------------- helpers --------------------------------- */

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

type scanner interface{ Scan(...any) error }

/* ---------------------------------- boards --------------------------------- */

func (s *Store) CreateBoard(b *model.Board) error {
	if b.Slug == "" {
		b.Slug = Slugify(b.Name)
	}
	if b.Slug == "" {
		return errors.New("board needs a name")
	}
	if err := checkLabels(b.Labels); err != nil {
		return err
	}
	b.ID, b.CreatedAt, b.UpdatedAt = newID(), time.Now().UTC(), time.Now().UTC()
	labels, _ := json.Marshal(nonNil(b.Labels))
	_, err := s.db.Exec(
		`INSERT INTO boards (id,slug,name,description,labels,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?)`,
		b.ID, b.Slug, b.Name, b.Description, string(labels), now(), now())
	return err
}

func (s *Store) ListBoards() ([]model.Board, error) {
	rows, err := s.db.Query(`SELECT id,slug,name,description,labels,created_at,updated_at
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
	row := s.db.QueryRow(`SELECT id,slug,name,description,labels,created_at,updated_at
	                       FROM boards WHERE slug = ? OR id = ?`, ref, ref)
	b, err := scanBoard(row)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

func scanBoard(sc scanner) (model.Board, error) {
	var b model.Board
	var labels, created, updated string
	if err := sc.Scan(&b.ID, &b.Slug, &b.Name, &b.Description, &labels, &created, &updated); err != nil {
		return b, err
	}
	_ = json.Unmarshal([]byte(labels), &b.Labels)
	b.Labels = nonNil(b.Labels)
	b.CreatedAt, b.UpdatedAt = parseTime(created), parseTime(updated)
	return b, nil
}

// UpdateBoard applies non-nil fields only. Labels merge rather than replace, and
// an empty string clears one lane's label back to its own name.
func (s *Store) UpdateBoard(ref string, name, description *string, labels map[string]string) (model.Board, error) {
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
	if err := checkLabels(labels); err != nil {
		return b, err
	}
	for k, v := range labels {
		if v == "" {
			delete(b.Labels, k)
		} else {
			b.Labels[k] = v
		}
	}
	j, _ := json.Marshal(nonNil(b.Labels))
	_, err = s.db.Exec(`UPDATE boards SET name=?,description=?,labels=?,updated_at=? WHERE id=?`,
		b.Name, b.Description, string(j), now(), b.ID)
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

func checkLabels(l map[string]string) error {
	for k := range l {
		if !model.ValidLane(model.Lane(k)) {
			return fmt.Errorf("%q is not a lane; lanes are unsorted, in, maybe, unsure, out", k)
		}
	}
	return nil
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
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

	next := map[model.Lane]float64{}
	out := make([]model.Idea, 0, len(in))
	for _, it := range in {
		if strings.TrimSpace(it.Title) == "" {
			return nil, errors.New("idea needs a title")
		}
		if it.Lane == "" {
			it.Lane = model.Unsorted
		}
		if !model.ValidLane(it.Lane) {
			return nil, fmt.Errorf("%q is not a lane", it.Lane)
		}
		if _, ok := next[it.Lane]; !ok {
			p, err := tailPos(tx, boardID, it.Lane)
			if err != nil {
				return nil, err
			}
			next[it.Lane] = p
		}
		it.ID, it.BoardID = newID(), boardID
		it.Pos = next[it.Lane]
		next[it.Lane]++
		it.CreatedAt, it.UpdatedAt = time.Now().UTC(), time.Now().UTC()
		if it.Source == "" {
			it.Source = model.SourceHuman
		}
		if _, err := tx.Exec(
			`INSERT INTO ideas (id,board_id,title,note,body,lane,source,pos,created_at,updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			it.ID, it.BoardID, it.Title, it.Note, it.Body, string(it.Lane), string(it.Source),
			it.Pos, now(), now()); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, tx.Commit()
}

type execQuerier interface {
	QueryRow(string, ...any) *sql.Row
	Exec(string, ...any) (sql.Result, error)
}

// tailPos is the position just past the end of a lane — where a fresh card or a
// just-sifted one lands.
func tailPos(q execQuerier, boardID string, lane model.Lane) (float64, error) {
	var p sql.NullFloat64
	err := q.QueryRow(`SELECT MAX(pos) FROM ideas WHERE board_id = ? AND lane = ?`,
		boardID, string(lane)).Scan(&p)
	if err != nil {
		return 0, err
	}
	if !p.Valid {
		return 1, nil
	}
	return p.Float64 + 1, nil
}

func (s *Store) ListIdeas(boardID string) ([]model.Idea, error) {
	rows, err := s.db.Query(
		`SELECT id,board_id,title,note,body,lane,source,pos,created_at,updated_at
		 FROM ideas WHERE board_id = ? ORDER BY `+laneOrder, boardID)
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
	// One extra query for the whole board rather than one per card, and the
	// threads themselves rather than a tally of them: the case for an idea is
	// what the sift decides on, so it has to arrive with the card.
	threads, err := s.threads(boardID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Comments = threads[out[i].ID]
		out[i].CommentCount = len(out[i].Comments)
	}
	return out, nil
}

// threads is every comment on a board in one query, keyed by idea.
func (s *Store) threads(boardID string) (map[string][]model.Comment, error) {
	rows, err := s.db.Query(`SELECT c.id,c.idea_id,c.who,c.text,c.created_at FROM comments c
	                          JOIN ideas i ON i.id = c.idea_id
	                          WHERE i.board_id = ? ORDER BY c.created_at`, boardID)
	if err != nil {
		return nil, err
	}
	all, err := scanComments(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]model.Comment, len(all))
	for _, c := range all {
		out[c.IdeaID] = append(out[c.IdeaID], c)
	}
	return out, nil
}

func (s *Store) Idea(id string) (model.Idea, error) {
	row := s.db.QueryRow(
		`SELECT id,board_id,title,note,body,lane,source,pos,created_at,updated_at
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
	var lane, src, created, updated string
	if err := sc.Scan(&it.ID, &it.BoardID, &it.Title, &it.Note, &it.Body, &lane, &src,
		&it.Pos, &created, &updated); err != nil {
		return it, err
	}
	it.Lane, it.Source = model.Lane(lane), model.Source(src)
	it.CreatedAt, it.UpdatedAt = parseTime(created), parseTime(updated)
	return it, nil
}

// IdeaPatch is a sparse edit. Nil means "leave alone" — an agent fixing a typo
// in the body must not blank the note.
type IdeaPatch struct {
	Title *string
	Note  *string
	Body  *string
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
	_, err = s.db.Exec(`UPDATE ideas SET title=?,note=?,body=?,updated_at=? WHERE id=?`,
		it.Title, it.Note, it.Body, now(), it.ID)
	it.UpdatedAt = time.Now().UTC()
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

/* ---------------------------------- sifts ---------------------------------- */

// Move is every decision there is: a sift vote (pos nil — the card lands at the
// end of its new lane, so a session reads in the order you decided it) and a
// drag (pos given — between its new neighbours) are the same write, logged the
// same way, undone the same way.
func (s *Store) Move(boardID, ideaID string, lane model.Lane, pos *float64) (model.Idea, error) {
	it, err := s.Idea(ideaID)
	if err != nil {
		return it, err
	}
	if it.BoardID != boardID {
		return it, errors.New("idea is not on this board")
	}
	if !model.ValidLane(lane) {
		return it, fmt.Errorf("%q is not a lane; lanes are unsorted, in, maybe, unsure, out", lane)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return it, err
	}
	defer tx.Rollback()

	from, fromPos := it.Lane, it.Pos
	it.Lane = lane
	if pos != nil {
		it.Pos = *pos
	} else {
		if it.Pos, err = tailPos(tx, boardID, lane); err != nil {
			return it, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO sifts (id,board_id,idea_id,from_lane,to_lane,from_pos,voided,created_at)
	                      VALUES (?,?,?,?,?,?,0,?)`,
		newID(), boardID, ideaID, string(from), string(lane), fromPos, now()); err != nil {
		return it, err
	}
	if _, err := tx.Exec(`UPDATE ideas SET lane=?,pos=?,updated_at=? WHERE id=?`,
		string(it.Lane), it.Pos, now(), it.ID); err != nil {
		return it, err
	}
	it.UpdatedAt = time.Now().UTC()
	return it, tx.Commit()
}

// UndoLast voids the most recent decision on a board and puts the card back
// exactly where it was. It returns the restored idea so a client mid-sift can
// show it again without reloading the world.
func (s *Store) UndoLast(boardID string) (model.Idea, error) {
	var id, ideaID, from string
	var fromPos float64
	err := s.db.QueryRow(`SELECT id,idea_id,from_lane,from_pos FROM sifts
	                       WHERE board_id = ? AND voided = 0
	                       ORDER BY created_at DESC, id DESC LIMIT 1`, boardID).
		Scan(&id, &ideaID, &from, &fromPos)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Idea{}, ErrNotFound
	}
	if err != nil {
		return model.Idea{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return model.Idea{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE sifts SET voided = 1 WHERE id = ?`, id); err != nil {
		return model.Idea{}, err
	}
	if _, err := tx.Exec(`UPDATE ideas SET lane=?,pos=?,updated_at=? WHERE id=?`,
		from, fromPos, now(), ideaID); err != nil {
		return model.Idea{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Idea{}, err
	}
	return s.Idea(ideaID)
}

// SiftLog is the live (non-voided) decision log for a board, newest first.
func (s *Store) SiftLog(boardID string, limit int) ([]model.Sift, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,board_id,idea_id,from_lane,to_lane,from_pos,created_at
	                          FROM sifts WHERE board_id = ? AND voided = 0
	                          ORDER BY created_at DESC, id DESC LIMIT ?`, boardID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Sift
	for rows.Next() {
		var sf model.Sift
		var from, to, created string
		if err := rows.Scan(&sf.ID, &sf.BoardID, &sf.IdeaID, &from, &to, &sf.FromPos, &created); err != nil {
			return nil, err
		}
		sf.From, sf.To, sf.CreatedAt = model.Lane(from), model.Lane(to), parseTime(created)
		out = append(out, sf)
	}
	return out, rows.Err()
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
	return scanComments(rows)
}

func scanComments(rows *sql.Rows) ([]model.Comment, error) {
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

/* ---------------------------------- views ---------------------------------- */

// InLane picks one lane out of an already-loaded board, order preserved.
func InLane(ideas []model.Idea, lane model.Lane) []model.Idea {
	out := make([]model.Idea, 0, len(ideas))
	for _, it := range ideas {
		if it.Lane == lane {
			out = append(out, it)
		}
	}
	return out
}

// Counts is how full each lane is — the one number every caller wants next to a
// board, because it says whether there is anything to sift.
func Counts(ideas []model.Idea) map[model.Lane]int {
	out := map[model.Lane]int{}
	for _, l := range model.Lanes {
		out[l] = 0
	}
	for _, it := range ideas {
		out[it.Lane]++
	}
	return out
}
