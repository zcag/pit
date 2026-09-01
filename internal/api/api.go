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

	"github.com/zcag/pit/internal/model"
	"github.com/zcag/pit/internal/rank"
	"github.com/zcag/pit/internal/store"
)

// TopK is how many ideas the stopping rule cares about. Five is the number of
// things a person can actually hold as "what I'm focused on".
const TopK = 5

// bootstrapSamples trades accuracy for latency in the stability estimate. 200
// resamples of a personal-scale log is a couple of milliseconds.
const bootstrapSamples = 200

type Config struct {
	Store  *store.Store
	APIKey string
	OAuth  *OAuth
	Web    fs.FS // embedded UI
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

	// Public: the metadata a client reads to learn how to authenticate. Served at
	// both the bare well-known path and the path-scoped alias, because Claude and
	// ChatGPT probe different ones.
	if s.cfg.OAuth != nil {
		m.HandleFunc("GET "+WellKnownPRM, s.cfg.OAuth.ServePRM)
		m.HandleFunc("GET "+WellKnownPRM+s.cfg.OAuth.ResourcePath(), s.cfg.OAuth.ServePRM)
	}

	api := func(pattern string, h http.HandlerFunc) { m.Handle(pattern, s.auth(h)) }

	api("GET /api/boards", s.listBoards)
	api("POST /api/boards", s.createBoard)
	api("GET /api/boards/{board}", s.getBoard)
	api("PATCH /api/boards/{board}", s.patchBoard)
	api("DELETE /api/boards/{board}", s.deleteBoard)

	api("GET /api/boards/{board}/ideas", s.listIdeas)
	api("POST /api/boards/{board}/ideas", s.addIdeas)
	api("GET /api/boards/{board}/top", s.top)
	api("GET /api/boards/{board}/pair", s.pair)
	api("POST /api/boards/{board}/duels", s.recordDuel)
	api("POST /api/boards/{board}/duels/undo", s.undoDuel)

	api("GET /api/ideas/{id}", s.getIdea)
	api("PATCH /api/ideas/{id}", s.patchIdea)
	api("DELETE /api/ideas/{id}", s.deleteIdea)
	api("POST /api/ideas/{id}/place", s.placeIdea)
	api("POST /api/ideas/{id}/comments", s.addComment)

	m.Handle("/mcp", s.auth(s.mcpHandler))

	m.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if s.hasSession(r) {
			http.Redirect(w, r, "/", http.StatusFound)
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
		if s.cfg.OAuth != nil && tok != "" && s.cfg.OAuth.Verify(tok) {
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

// boardView is a board plus the one thing every caller wants next to it: is it
// settled? An agent reading a board needs to know whether the ranking is worth
// acting on, and making that a separate call guarantees it gets skipped.
type boardView struct {
	model.Board
	Ideas   int          `json:"ideas"`
	Live    int          `json:"live"`
	Verdict rank.Verdict `json:"verdict"`
}

func (s *Server) view(b model.Board) (boardView, error) {
	ideas, err := s.cfg.Store.ListIdeas(b.ID)
	if err != nil {
		return boardView{}, err
	}
	v, err := s.cfg.Store.Verdict(b, TopK, bootstrapSamples)
	if err != nil {
		return boardView{}, err
	}
	return boardView{Board: b, Ideas: len(ideas), Live: len(store.Live(b, ideas)), Verdict: v}, nil
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
	writeJSON(w, http.StatusOK, map[string]any{"boards": out})
}

func (s *Server) createBoard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string   `json:"name"`
		Slug        string   `json:"slug"`
		Description string   `json:"description"`
		Buckets     []string `json:"buckets"`
		Terminal    string   `json:"terminal"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	b := model.Board{Name: in.Name, Slug: in.Slug, Description: in.Description,
		Buckets: in.Buckets, Terminal: in.Terminal}
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
		Name        *string   `json:"name"`
		Description *string   `json:"description"`
		Buckets     *[]string `json:"buckets"`
		Terminal    *string   `json:"terminal"`
		Intake      *string   `json:"intake"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	b, err := s.cfg.Store.UpdateBoard(r.PathValue("board"), in.Name, in.Description, in.Buckets, in.Terminal, in.Intake)
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
	if bucket := r.URL.Query().Get("bucket"); bucket != "" {
		ideas = filter(ideas, func(i model.Idea) bool { return i.Bucket == bucket })
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
			Title  string `json:"title"`
			Note   string `json:"note"`
			Body   string `json:"body"`
			Bucket string `json:"bucket"`
		} `json:"ideas"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	src := sourceOf(in.Source)
	inbox := b.Intake
	batch := make([]model.Idea, 0, len(in.Ideas))
	for _, it := range in.Ideas {
		bucket := it.Bucket
		if bucket == "" {
			bucket = inbox
		}
		batch = append(batch, model.Idea{Title: it.Title, Note: it.Note, Body: it.Body,
			Bucket: bucket, Source: src})
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

func (s *Server) patchIdea(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title  *string `json:"title"`
		Note   *string `json:"note"`
		Body   *string `json:"body"`
		Bucket *string `json:"bucket"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	it, err := s.cfg.Store.UpdateIdea(r.PathValue("id"),
		store.IdeaPatch{Title: in.Title, Note: in.Note, Body: in.Body, Bucket: in.Bucket})
	if err != nil {
		fail(w, err)
		return
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

// placeIdea is a drag: a rating asserted by hand. Deliberately not exposed over
// MCP — an agent must not be able to set a rating, or the board stops being
// evidence of what the owner thinks.
func (s *Server) placeIdea(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Rating float64 `json:"rating"`
		Bucket *string `json:"bucket"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if in.Bucket != nil {
		if _, err := s.cfg.Store.UpdateIdea(r.PathValue("id"), store.IdeaPatch{Bucket: in.Bucket}); err != nil {
			fail(w, err)
			return
		}
	}
	it, err := s.cfg.Store.PlaceIdea(r.PathValue("id"), in.Rating)
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

/* --------------------------------- ranking -------------------------------- */

// top is the short answer, and it always ships the settled flag with it. A
// ranking without a confidence signal invites an agent to act on noise.
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
	live := store.Live(b, ideas)
	v, err := s.cfg.Store.Verdict(b, k, bootstrapSamples)
	if err != nil {
		fail(w, err)
		return
	}
	if k > len(live) {
		k = len(live)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"board":   b.Slug,
		"verdict": v,
		"top":     live[:k], // ListIdeas already orders by rating desc
	})
}

// pair hands back the next comparison worth asking. Putting the choice on the
// server means every client — web, phone, a future CLI — gets the same adaptive
// pairing rather than each reinventing a worse one.
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
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
	live := store.Live(b, ideas)
	if bucket := r.URL.Query().Get("bucket"); bucket != "" {
		scoped := filter(live, func(i model.Idea) bool { return i.Bucket == bucket })
		if len(scoped) >= 2 {
			live = scoped
		}
	}
	a, bb, ok := rank.NextPair(store.Items(live), TopK, nil)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"exhausted": true})
		return
	}
	byID := map[string]model.Idea{}
	for _, it := range live {
		byID[it.ID] = it
	}
	v, err := s.cfg.Store.Verdict(b, TopK, bootstrapSamples)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"a": byID[a.ID], "b": byID[bb.ID], "verdict": v,
	})
}

func (s *Server) recordDuel(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Winner string `json:"winner"`
		Loser  string `json:"loser"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	d, err := s.cfg.Store.RecordDuel(b.ID, in.Winner, in.Loser)
	if err != nil {
		fail(w, err)
		return
	}
	v, err := s.cfg.Store.Verdict(b, TopK, bootstrapSamples)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"duel": d, "verdict": v})
}

func (s *Server) undoDuel(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.cfg.Store.UndoLastDuel(b.ID); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"undone": b.Slug})
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
