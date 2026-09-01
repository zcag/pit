package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// Browser sessions.
//
// The cookie value is HMAC(api key, "pit-session") — deterministic, so there is
// no session table to keep, expire or lose on restart, and the cookie never
// carries the key itself. Revoking every session is exactly "rotate the key",
// which is the behaviour you want anyway.
const sessionCookie = "pit_session"

func sessionToken(apiKey string) string {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte("pit-session"))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) hasSession(r *http.Request) bool {
	if s.cfg.APIKey == "" {
		return false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(sessionToken(s.cfg.APIKey))) == 1
}

// login accepts the key as a form field or the `p` query parameter, so it works
// from the login form and from a link. On success it sets the session cookie and
// sends the browser to the board.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("p")
	if key == "" {
		_ = r.ParseForm()
		key = r.FormValue("key")
	}
	if s.cfg.APIKey == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(key)), []byte(s.cfg.APIKey)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		writeLoginPage(w, "That key was not right.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sessionToken(s.cfg.APIKey),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   60 * 60 * 24 * 365,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func writeLoginPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>pit</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg%20xmlns%3D%22http%3A%2F%2Fwww.w3.org%2F2000%2Fsvg%22%20viewBox%3D%220%200%2032%2032%22%3E%3Crect%20width%3D%2232%22%20height%3D%2232%22%20rx%3D%227%22%20fill%3D%22%2314171A%22%2F%3E%3Crect%20x%3D%227%22%20y%3D%2214%22%20width%3D%227%22%20height%3D%2211%22%20rx%3D%222.5%22%20fill%3D%22%236A7276%22%2F%3E%3Crect%20x%3D%2218%22%20y%3D%227%22%20width%3D%227%22%20height%3D%2218%22%20rx%3D%222.5%22%20fill%3D%22%23E0A800%22%2F%3E%3C%2Fsvg%3E">
<style>
:root{color-scheme:light dark;--bg:#E7E9E6;--ink:#14171A;--muted:#6A7276;--rule:#C6CBC6;--card:#fff;--spark:#E0A800;--spark-ink:#231A00}
@media(prefers-color-scheme:dark){:root{--bg:#0D1012;--ink:#E9EBE8;--muted:#8B9397;--rule:#2A3134;--card:#1B2023;--spark:#F2C318;--spark-ink:#1A1400}}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--ink);
font-family:"Inter Tight",system-ui,-apple-system,sans-serif;font-size:14px}
form{width:min(320px,90vw);display:flex;flex-direction:column;gap:10px}
h1{font-family:ui-monospace,monospace;font-size:22px;letter-spacing:-.07em;margin:0 0 2px}
p{color:var(--muted);margin:0 0 10px;font-size:13px}
input{padding:10px 12px;border:1px solid var(--rule);border-radius:7px;background:var(--card);color:var(--ink);font:inherit}
input:focus{outline:none;border-color:var(--spark);box-shadow:0 0 0 3px color-mix(in srgb,var(--spark) 25%,transparent)}
button{padding:10px 12px;border:0;border-radius:7px;background:var(--spark);color:var(--spark-ink);font:inherit;font-weight:600;cursor:pointer}
</style>
<form method=post action=/login>
<h1>pit</h1><p>` + msg + `</p>
<input type=password name=key placeholder="Key" autofocus autocomplete=current-password>
<button type=submit>Open the board</button>
</form>`))
}
