// Package api serves pit's REST surface, the static web UI, and mounts the MCP
// endpoint. One binary, one port, one auth story.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/zcag/mcpauth"
	"github.com/zcag/pit/internal/model"
	"github.com/zcag/pit/internal/store"
)

// TopK is how many ideas "the short answer" means. Five is the number of things
// a person can actually hold as "what I'm focused on".
const TopK = 5

type Config struct {
	Store  *store.Store
	APIKey string
	// OAuth lets /mcp also accept an authorization-server-issued JWT (the
	// Claude.ai / ChatGPT "Connect" flow) alongside the static key. nil means
	// off, and every call on it is nil-safe, so nothing here branches on it
	// except where the behaviour genuinely differs.
	OAuth *mcpauth.Verifier
	Web   fs.FS // embedded UI
}

type Server struct {
	cfg Config
	mux *http.ServeMux
}

func New(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	m := s.mux

	// Public: the metadata a client reads to learn how to authenticate.
	s.cfg.OAuth.Mount(m)

	api := func(pattern string, h http.HandlerFunc) { m.Handle(pattern, s.auth(h)) }

	api("GET /api/all", s.everything)
	api("GET /api/boards", s.listBoards)
	api("POST /api/boards", s.createBoard)
	api("GET /api/boards/{board}", s.getBoard)
	api("PATCH /api/boards/{board}", s.patchBoard)
	api("DELETE /api/boards/{board}", s.deleteBoard)

	api("GET /api/boards/{board}/ideas", s.listIdeas)
	api("POST /api/boards/{board}/ideas", s.addIdeas)
	api("GET /api/boards/{board}/top", s.top)
	api("POST /api/boards/{board}/sift", s.sift)
	api("POST /api/boards/{board}/sift/undo", s.undo)
	api("GET /api/boards/{board}/log", s.log)
	api("GET /api/boards/{board}/shares", s.listShares)
	api("POST /api/boards/{board}/shares", s.createShare)
	api("DELETE /api/shares/{token}", s.deleteShare)

	api("GET /api/ideas/{id}", s.getIdea)
	api("PATCH /api/ideas/{id}", s.patchIdea)
	api("DELETE /api/ideas/{id}", s.deleteIdea)
	api("POST /api/ideas/{id}/place", s.placeIdea)
	api("POST /api/ideas/{id}/comments", s.addComment)

	m.Handle("/mcp", s.auth(s.mcpHandler))

	// The one route that answers without a key. Registered before the catch-all
	// so it is never bounced to /login.
	m.HandleFunc("GET /s/{token}", s.sharePage)

	m.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if s.hasSession(r) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		// ?p=<key> logs in from a plain link, not just the form.
		if r.URL.Query().Get("p") != "" {
			s.login(w, r)
			return
		}
		writeLoginPage(w, "This board is yours alone.")
	})
	m.HandleFunc("POST /login", s.login)
	m.HandleFunc("GET /logout", s.logout)

	if s.cfg.Web != nil {
		files := http.FileServer(http.FS(s.cfg.Web))
		m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if s.cfg.APIKey != "" && !s.hasSession(r) {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			files.ServeHTTP(w, r)
		})
	}
}

// auth accepts the static API key (CLI, scripts, local agents) or a verified
// OAuth token (hosted connectors). On failure with OAuth enabled it emits the
// WWW-Authenticate challenge so a client can discover the authorization server
// and start the Connect flow rather than just seeing a dead 401.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.hasSession(r) { // the web UI, already logged in
			next(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok = strings.TrimSpace(tok)

		// No key configured at all: local development, everything is open.
		if s.cfg.APIKey == "" && s.cfg.OAuth == nil {
			next(w, r)
			return
		}

		if s.cfg.APIKey != "" && tok != "" &&
			subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.APIKey)) == 1 {
			next(w, r)
			return
		}
		if tok != "" && s.cfg.OAuth.Verify(tok) {
			next(w, r)
			return
		}
		if s.cfg.OAuth != nil {
			w.Header().Set("WWW-Authenticate", s.cfg.OAuth.Challenge())
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

/* --------------------------------- helpers -------------------------------- */

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		log.Printf("pit: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// sourceOf decides whether a write is attributed to the human or an agent.
// Callers may declare it; MCP always declares "agent". The default is human,
// because the web UI is the one caller that never sets it.
func sourceOf(declared string) model.Source {
	if model.Source(declared) == model.SourceAgent {
		return model.SourceAgent
	}
	return model.SourceHuman
}

func (s *Server) board(r *http.Request) (model.Board, error) {
	return s.cfg.Store.Board(r.PathValue("board"))
}

/* --------------------------------- boards --------------------------------- */

// boardView is a board plus the one thing every caller wants next to it: how
// full each lane is. Unsorted is the number that decides whether you owe this
// board a sift, and making it a second call guarantees it gets skipped.
type boardView struct {
	model.Board
	Ideas  int                `json:"ideas"`
	Counts map[model.Lane]int `json:"counts"`
}

func (s *Server) view(b model.Board) (boardView, error) {
	ideas, err := s.cfg.Store.ListIdeas(b.ID)
	if err != nil {
		return boardView{}, err
	}
	return boardView{Board: b, Ideas: len(ideas), Counts: store.Counts(ideas)}, nil
}

func (s *Server) listBoards(w http.ResponseWriter, r *http.Request) {
	bs, err := s.cfg.Store.ListBoards()
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]boardView, 0, len(bs))
	for _, b := range bs {
		v, err := s.view(b)
		if err != nil {
			fail(w, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"boards": out, "lanes": model.Lanes})
}

// everything is the one question that does not respect board boundaries: what
// have I actually committed to. Each board's `in` lane, in the owner's order,
// side by side — not merged and not ranked against each other.
func (s *Server) everything(w http.ResponseWriter, r *http.Request) {
	lane := model.Lane(r.URL.Query().Get("lane"))
	if lane == "" {
		lane = model.In
	}
	bs, err := s.cfg.Store.ListBoards()
	if err != nil {
		fail(w, err)
		return
	}
	type group struct {
		Board model.Board  `json:"board"`
		Ideas []model.Idea `json:"ideas"`
	}
	out := make([]group, 0, len(bs))
	kept := 0
	for _, b := range bs {
		ideas, err := s.cfg.Store.ListIdeas(b.ID)
		if err != nil {
			fail(w, err)
			return
		}
		g := store.InLane(ideas, lane)
		kept += len(g)
		out = append(out, group{Board: b, Ideas: g})
	}
	writeJSON(w, http.StatusOK, map[string]any{"lane": lane, "kept": kept, "boards": out})
}

func (s *Server) createBoard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string            `json:"name"`
		Slug        string            `json:"slug"`
		Description string            `json:"description"`
		Labels      map[string]string `json:"labels"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	b := model.Board{Name: in.Name, Slug: in.Slug, Description: in.Description, Labels: in.Labels}
	if err := s.cfg.Store.CreateBoard(&b); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) getBoard(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	v, err := s.view(b)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) patchBoard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        *string           `json:"name"`
		Description *string           `json:"description"`
		Labels      map[string]string `json:"labels"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	b, err := s.cfg.Store.UpdateBoard(r.PathValue("board"), in.Name, in.Description, in.Labels)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) deleteBoard(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Store.DeleteBoard(r.PathValue("board")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------------------------------- ideas --------------------------------- */

func (s *Server) listIdeas(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	ideas, err := s.cfg.Store.ListIdeas(b.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if lane := r.URL.Query().Get("lane"); lane != "" {
		ideas = store.InLane(ideas, model.Lane(lane))
	}
	if src := r.URL.Query().Get("source"); src != "" {
		ideas = filter(ideas, func(i model.Idea) bool { return string(i.Source) == src })
	}
	writeJSON(w, http.StatusOK, map[string]any{"board": b, "ideas": ideas})
}

func (s *Server) addIdeas(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Source string `json:"source"`
		Ideas  []struct {
			Title string `json:"title"`
			Note  string `json:"note"`
			Body  string `json:"body"`
			Lane  string `json:"lane"`
		} `json:"ideas"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	src := sourceOf(in.Source)
	batch := make([]model.Idea, 0, len(in.Ideas))
	for _, it := range in.Ideas {
		batch = append(batch, model.Idea{Title: it.Title, Note: it.Note, Body: it.Body,
			Lane: model.Lane(it.Lane), Source: src})
	}
	out, err := s.cfg.Store.AddIdeas(b.ID, batch)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"added": len(out), "ideas": out})
}

func (s *Server) getIdea(w http.ResponseWriter, r *http.Request) {
	it, err := s.cfg.Store.Idea(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// patchIdea edits the text of an idea. Moving it is a decision, so it goes
// through the sift log instead: POST /sift, or /place for a drag.
func (s *Server) patchIdea(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title *string `json:"title"`
		Note  *string `json:"note"`
		Body  *string `json:"body"`
		Lane  *string `json:"lane"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	it, err := s.cfg.Store.UpdateIdea(r.PathValue("id"),
		store.IdeaPatch{Title: in.Title, Note: in.Note, Body: in.Body})
	if err != nil {
		fail(w, err)
		return
	}
	if in.Lane != nil && *in.Lane != string(it.Lane) {
		it, err = s.cfg.Store.Move(it.BoardID, it.ID, model.Lane(*in.Lane), nil)
		if err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *Server) deleteIdea(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Store.DeleteIdea(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// placeIdea is a drag: a lane and an exact position, asserted by hand. Same
// write and same log entry as a sift vote — the only difference is that you
// chose where in the lane it lands.
func (s *Server) placeIdea(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Lane string   `json:"lane"`
		Pos  *float64 `json:"pos"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	it, err := s.cfg.Store.Idea(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	lane := model.Lane(in.Lane)
	if lane == "" {
		lane = it.Lane
	}
	it, err = s.cfg.Store.Move(it.BoardID, it.ID, lane, in.Pos)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
		Who  string `json:"who"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	c, err := s.cfg.Store.AddComment(r.PathValue("id"), sourceOf(in.Who), in.Text)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

/* --------------------------------- sifting -------------------------------- */

// sift is one vote. The card lands at the end of its new lane, which is what
// makes a sift session read back in the order you decided it.
func (s *Server) sift(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Idea string `json:"idea"`
		Lane string `json:"lane"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	it, err := s.cfg.Store.Move(b.ID, in.Idea, model.Lane(in.Lane), nil)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, it)
}

// undo voids the last decision and hands back the restored card, so a client
// mid-sift can show it again without reloading the board.
func (s *Server) undo(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	it, err := s.cfg.Store.UndoLast(b.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	sifts, err := s.cfg.Store.SiftLog(b.ID, n)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"board": b.Slug, "sifts": sifts})
}

// top is the short answer: the in lane, in the order you put it in.
func (s *Server) top(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	k := TopK
	if q := r.URL.Query().Get("k"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			k = n
		}
	}
	ideas, err := s.cfg.Store.ListIdeas(b.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, topPayload(b, ideas, k))
}

// topPayload is shared with the MCP tool: the in lane, trimmed to k, plus what
// is still waiting — a top five drawn from a board with forty unsorted ideas is
// a provisional answer and should say so.
func topPayload(b model.Board, ideas []model.Idea, k int) map[string]any {
	in := store.InLane(ideas, model.In)
	if k < len(in) {
		in = in[:k]
	}
	counts := store.Counts(ideas)
	return map[string]any{
		"board":    b.Slug,
		"top":      in,
		"unsorted": counts[model.Unsorted],
		"unsure":   counts[model.Unsure],
	}
}

func filter[T any](in []T, keep func(T) bool) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}
