package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const version = "1.8.0"

type Server struct {
	db       *DB
	mdx      *MangaDex
	dl       *Downloader
	keys     *Keys
	mangaDir string
	covers   *Covers
	limiter  *loginLimiter
	pages    map[string]*template.Template
	pagesMu  sync.Mutex    // held while uploaded pages move into a chapter's folder
	closing  chan struct{} // closed when the server shuts down
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprintf(os.Stderr, `Kotodex %s

Usage:
  kotodex [serve]                          Start the server
  kotodex user add NAME PASSWORD [admin]   Add a user
  kotodex user passwd NAME PASSWORD        Change a user's password
  kotodex user list                        List users

Environment:
  KOTODEX_DATA_DIR        Where the database and downloads are kept (default ./data)
  KOTODEX_LISTEN          Address to listen on (default :8080)
  KOTODEX_ADMIN_PASSWORD  Password for the "admin" user created on first start
  KOTODEX_SECRET          Server key, instead of secret.key in the data directory
`, version)
}

func main() {
	log.SetFlags(log.LstdFlags)
	flag.Usage = usage
	flag.Parse()
	dataDir := env("KOTODEX_DATA_DIR", "./data")
	if err := os.MkdirAll(filepath.Join(dataDir, "manga"), 0o755); err != nil {
		log.Fatal(err)
	}
	db, err := openDB(filepath.Join(dataDir, "kotodex.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	args := flag.Args()
	if len(args) > 0 && args[0] == "user" {
		os.Exit(userCommand(db, args[1:]))
	}
	if len(args) > 0 && args[0] != "serve" {
		usage()
		os.Exit(2)
	}

	if db.userCount() == 0 {
		pass := os.Getenv("KOTODEX_ADMIN_PASSWORD")
		generated := pass == ""
		if generated {
			pass = randomToken(12)
		}
		if _, err := db.createUser("admin", pass, true); err != nil {
			log.Fatal(err)
		}
		if generated {
			log.Printf("Created user \"admin\" with password %s (change it under Settings)", pass)
		} else {
			log.Printf("Created user \"admin\" with the password from KOTODEX_ADMIN_PASSWORD")
		}
	}

	keys, err := loadKeys(filepath.Join(dataDir, keyFileName))
	if err != nil {
		log.Fatal(err)
	}

	mdx := newMangaDex("Kotodex/" + version)
	mangaDir := filepath.Join(dataDir, "manga")
	s := &Server{
		db:       db,
		mdx:      mdx,
		dl:       newDownloader(db, mdx, keys, mangaDir),
		keys:     keys,
		mangaDir: mangaDir,
		covers:   newCovers(db, mangaDir),
		limiter:  &loginLimiter{fails: map[string][]time.Time{}},
		closing:  make(chan struct{}),
	}
	s.loadTemplates()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, a second one exits right away.
	context.AfterFunc(ctx, stop)
	go s.dl.Run(ctx)
	go s.covers.Run(ctx)
	go func() {
		for {
			db.deleteIdleSessions()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()

	addr := env("KOTODEX_LISTEN", ":8080")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Kotodex %s listening on %s, data in %s", version, addr, dataDir)
	if err := serve(ctx, s.httpServer(), ln); err != nil {
		log.Fatal(err)
	}
	log.Print("Stopped")
}

func (s *Server) httpServer() *http.Server {
	srv := &http.Server{
		Handler:           logRequests(securityHeaders(s.routes())),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	// Live update streams never finish on their own.
	srv.RegisterOnShutdown(func() { close(s.closing) })
	return srv
}

// shutdownGrace is how long requests already running get to finish after a
// stop signal. Form posts take milliseconds. Anything slower is cut off.
// Nothing is lost by that: pages are written to a temporary file and
// renamed, and every database write is a single statement.
const shutdownGrace = 2 * time.Second

// serve runs srv on ln until ctx is done. Then it stops taking connections,
// waits up to shutdownGrace for running requests, and closes the rest.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	grace, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(grace); err != nil {
		srv.Close()
	}
	return nil
}

// securityHeaders limits what a page can load and run. Scripts and styles
// only come from /static, images from this server and MangaDex covers (search
// results), and pages can't be framed.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' https://uploads.mangadex.org; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// logRequests logs failed requests only. Image requests are too many to log.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		if rec.status >= 400 && rec.status != 404 {
			log.Printf("%s %s -> %d", r.Method, r.URL.Path, rec.status)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets live update streams flush through the recorder.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func userCommand(db *DB, args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch {
	case args[0] == "add" && (len(args) == 3 || len(args) == 4):
		if _, err := db.createUser(args[1], args[2], len(args) == 4 && args[3] == "admin"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("Added %s\n", args[1])
	case args[0] == "passwd" && len(args) == 3:
		var id int64
		if err := db.QueryRow(`SELECT id FROM users WHERE name=?`, args[1]).Scan(&id); err != nil {
			fmt.Fprintf(os.Stderr, "No user named %s\n", args[1])
			return 1
		}
		if err := db.setPassword(id, args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("Changed the password for %s\n", args[1])
	case args[0] == "list":
		users, err := db.users()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, u := range users {
			if u.Admin {
				fmt.Println(u.Name, "(admin)")
			} else {
				fmt.Println(u.Name)
			}
		}
	default:
		usage()
		return 2
	}
	return 0
}
