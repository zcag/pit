package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestBoardScriptIsPublic(t *testing.T) {
	web := fstest.MapFS{"index.html": {Data: []byte("app")}, "pit.js": {Data: []byte("export {}")}}
	s := New(Config{APIKey: "k", Web: web})
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	if rec := get("/pit.js"); rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("/pit.js: got %d, CORS %q; want 200 for anyone", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec := get("/"); rec.Code != http.StatusFound {
		t.Errorf("/ without a session: got %d, want the login redirect", rec.Code)
	}
}
