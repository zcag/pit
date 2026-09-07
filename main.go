// Command pit is an idea sifter: dump ideas in, go through them one card at a
// time, and come out with a short list of what you actually meant to keep.
//
// One binary: SQLite store, REST API, embedded web UI, and an MCP endpoint that
// hosted assistants can connect to.
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zcag/mcpauth"
	"github.com/zcag/pit/internal/api"
	"github.com/zcag/pit/internal/model"
	"github.com/zcag/pit/internal/store"
)

//go:embed all:web
var webFS embed.FS

// Version is stamped by the Makefile.
var Version = "dev"

func main() {
	var (
		addr = flag.String("addr", env("PIT_ADDR", ":8802"), "listen address")
		db   = flag.String("db", env("PIT_DB", defaultDB()), "path to the SQLite database")
		seed = flag.Bool("seed", false, "create a starter board if the database is empty")
		ver  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *ver {
		log.SetFlags(0)
		log.Println("pit", Version)
		return
	}
	api.Version = Version

	if err := os.MkdirAll(filepath.Dir(*db), 0o755); err != nil {
		log.Fatalf("pit: %v", err)
	}
	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("pit: open %s: %v", *db, err)
	}
	defer st.Close()

	if *seed {
		if err := seedIfEmpty(st); err != nil {
			log.Fatalf("pit: seed: %v", err)
		}
	}

	apiKey := os.Getenv("PIT_API_KEY")
	if apiKey == "" {
		log.Println("pit: PIT_API_KEY is unset — the API and MCP endpoint are UNAUTHENTICATED")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// OAuth is off unless both issuer and resource are set; when off, /mcp stays
	// API-key-only and advertises nothing.
	oauth := mcpauth.New(ctx, mcpauth.Config{
		Name:          "pit",
		Issuer:        os.Getenv("PIT_OAUTH_ISSUER"),
		Resource:      os.Getenv("PIT_MCP_RESOURCE"),
		AllowedEmails: os.Getenv("PIT_OAUTH_ALLOWED_EMAIL"),
		AllowedSubs:   os.Getenv("PIT_OAUTH_ALLOWED_SUB"),
	})

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("pit: web assets: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(api.Config{Store: st, APIKey: apiKey, OAuth: oauth, Web: sub}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("pit %s listening on %s (db %s)", Version, *addr, *db)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("pit: %v", err)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	log.Println("pit: stopped")
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func defaultDB() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "pit.db"
	}
	return filepath.Join(home, ".local", "share", "pit", "pit.db")
}

func seedIfEmpty(st *store.Store) error {
	bs, err := st.ListBoards()
	if err != nil || len(bs) > 0 {
		return err
	}
	b := model.Board{Name: "Ideas", Description: "Everything worth deciding about, until it earns its own board."}
	return st.CreateBoard(&b)
}
