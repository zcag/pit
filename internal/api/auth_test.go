package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zcag/mcpauth"
)

// The guard's own logic, not the JWT verification behind it — that is
// mcpauth's, and tested there. What is pit's to get wrong is which ways in it
// accepts, and whether a refusal tells the client where to authenticate.
func TestAuth(t *testing.T) {
	call := func(s *Server, authz string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/boards", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		s.auth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }).
			ServeHTTP(rec, req)
		return rec
	}

	keyed := &Server{cfg: Config{APIKey: "secret-key"}}

	if rec := call(keyed, "Bearer secret-key", nil); rec.Code != http.StatusNoContent {
		t.Errorf("static API key: got %d, want 204", rec.Code)
	}
	if rec := call(keyed, "Bearer wrong", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: got %d, want 401", rec.Code)
	}
	if rec := call(keyed, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no creds: got %d, want 401", rec.Code)
	}
	// The browser's way in.
	session := &http.Cookie{Name: sessionCookie, Value: sessionToken("secret-key")}
	if rec := call(keyed, "", session); rec.Code != http.StatusNoContent {
		t.Errorf("session cookie: got %d, want 204", rec.Code)
	}
	if rec := call(keyed, "", &http.Cookie{Name: sessionCookie, Value: "forged"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged cookie: got %d, want 401", rec.Code)
	}

	// No key and no OAuth is local development: everything is open, and the
	// binary says so on startup.
	open := &Server{cfg: Config{}}
	if rec := call(open, "", nil); rec.Code != http.StatusNoContent {
		t.Errorf("unconfigured: got %d, want 204 (open)", rec.Code)
	}

	// With OAuth off there is nothing to discover, so a 401 carries no
	// challenge; with it on, the 401 has to point at the metadata or the client
	// has no way to start the Connect flow.
	if wa := call(keyed, "", nil).Header().Get("WWW-Authenticate"); wa != "" {
		t.Errorf("challenge without OAuth configured: %q", wa)
	}
	// An empty key set is enough: nothing here mints a token, it only has to be
	// reachable so the Verifier comes up.
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer issuer.Close()
	withOAuth := &Server{cfg: Config{APIKey: "secret-key",
		OAuth: mcpauth.New(t.Context(), mcpauth.Config{
			Name: "pit", Issuer: issuer.URL, Resource: "https://pit.test/mcp"})}}
	if withOAuth.cfg.OAuth == nil {
		t.Fatal("mcpauth.New returned nil with issuer and resource set")
	}
	wa := call(withOAuth, "", nil).Header().Get("WWW-Authenticate")
	if !strings.Contains(wa, "resource_metadata=") {
		t.Errorf("401 missing resource_metadata challenge: %q", wa)
	}
}
