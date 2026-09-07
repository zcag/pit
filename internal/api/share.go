package api

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/zcag/pit/internal/model"
	"github.com/zcag/pit/internal/store"
)

// Sharing.
//
// Two of these boards exist to be shown, and showing one meant a screenshot or
// reading the list aloud, because the only way in is a session cookie on a
// single-user instance. A share is one lane of one board, rendered read-only
// for anyone holding the token — no auth, no API, no way back into the rest of
// the instance. The token is the whole credential, so revoking is deleting it.

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	shares, err := s.cfg.Store.SharesFor(b.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"board": b.Slug, "shares": shares})
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	b, err := s.board(r)
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Lane string `json:"lane"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	lane := model.Lane(in.Lane)
	if lane == "" {
		lane = model.In
	}
	sh, err := s.cfg.Store.CreateShare(b.ID, lane)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sh)
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Store.DeleteShare(r.PathValue("token")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sharePage is the only route that answers without a key. It renders one lane
// and nothing else: no ids, no other lanes, no links back in.
func (s *Server) sharePage(w http.ResponseWriter, r *http.Request) {
	sh, err := s.cfg.Store.Share(strings.TrimSpace(r.PathValue("token")))
	if err != nil {
		http.Error(w, "no such link", http.StatusNotFound)
		return
	}
	b, err := s.cfg.Store.Board(sh.BoardID)
	if err != nil {
		http.Error(w, "no such link", http.StatusNotFound)
		return
	}
	ideas, err := s.cfg.Store.ListIdeas(b.ID)
	if err != nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = shareTmpl.Execute(w, map[string]any{
		"Board": b,
		"Lane":  b.Label(sh.Lane),
		"Ideas": store.InLane(ideas, sh.Lane),
	})
}

var shareTmpl = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<title>{{.Board.Name}} · {{.Lane}}</title>
<style>
:root{--bg:#faf4ed;--card:#fffaf3;--ink:#575279;--muted:#797593;--faint:#9893a5;
  --hair:#dfdad9;--spark:#ea9d34;color-scheme:light}
@media(prefers-color-scheme:dark){:root{--bg:#292c3c;--card:#414559;--ink:#c6d0f5;
  --muted:#a5adce;--faint:#838ba7;--hair:#414559;--spark:#e5c890;color-scheme:dark}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.55 "Inter Tight",
  system-ui,-apple-system,sans-serif;-webkit-font-smoothing:antialiased}
main{max-width:800px;margin:0 auto;padding:44px 20px 72px}
header{margin-bottom:30px}
h1{font-size:26px;letter-spacing:-.03em;margin:0 0 6px}
.lane{font-family:ui-monospace,monospace;font-size:11px;letter-spacing:.15em;
  text-transform:uppercase;color:var(--faint)}
.desc{color:var(--muted);margin:10px 0 0;max-width:60ch}
ol{list-style:none;counter-reset:n;margin:0;padding:0}
li{counter-increment:n;position:relative;background:var(--card);border:1px solid var(--hair);
  border-radius:9px;padding:15px 19px 14px 48px;margin-bottom:11px}
li::before{content:counter(n);position:absolute;left:0;top:16px;width:48px;text-align:center;
  font-family:ui-monospace,monospace;font-size:12px;font-weight:700;color:var(--spark)}
h2{font-size:17px;font-weight:600;line-height:1.3;letter-spacing:-.015em;margin:0;
  overflow-wrap:anywhere}
.note{font-size:13.5px;color:var(--muted);margin:4px 0 0;overflow-wrap:anywhere}
.body{font-size:13.5px;color:var(--faint);line-height:1.55;margin:10px 0 0;padding-left:12px;
  border-left:2px solid var(--hair);white-space:pre-wrap;overflow-wrap:anywhere}
.empty{color:var(--faint);border:1px dashed var(--hair);border-radius:9px;padding:32px;text-align:center}
footer{margin-top:34px;font-family:ui-monospace,monospace;font-size:10.5px;letter-spacing:.1em;
  text-transform:uppercase;color:var(--faint)}
</style></head><body><main>
<header>
  <span class="lane">{{.Lane}}</span>
  <h1>{{.Board.Name}}</h1>
  {{if .Board.Description}}<p class="desc">{{.Board.Description}}</p>{{end}}
</header>
{{if .Ideas}}<ol>
{{range .Ideas}}<li>
  <h2>{{.Title}}</h2>
  {{if .Note}}<p class="note">{{.Note}}</p>{{end}}
  {{if .Body}}<p class="body">{{.Body}}</p>{{end}}
</li>
{{end}}</ol>{{else}}<div class="empty">Nothing here.</div>{{end}}
<footer>shared from pit · read-only</footer>
</main></body></html>`))
