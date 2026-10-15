package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "kotodex_session"

// Argon2id settings for new password hashes (the OWASP minimum, light enough
// for a small NAS). Each hash records its own settings, so changing these only
// affects passwords set afterwards.
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
)

var dummyHash = hashPassword("dummy")

// hashPassword returns an Argon2id hash in the usual PHC string format,
// $argon2id$v=19$m=...,t=...,p=...$salt$key. Unlike bcrypt, Argon2id takes
// passwords of any length.
func hashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// verifyPassword checks a password against an Argon2id hash, or a bcrypt hash
// from before Kotodex switched. upgrade is true when the password matched but
// the hash should be replaced with a fresh one from hashPassword.
func verifyPassword(hash, password string) (ok, upgrade bool) {
	if strings.HasPrefix(hash, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil, true
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, false
	}
	var version int
	var memory, passes uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &passes, &threads); err != nil {
		return false, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, false
	}
	got := argon2.IDKey([]byte(password), salt, passes, memory, threads, uint32(len(want)))
	ok = subtle.ConstantTimeCompare(got, want) == 1
	return ok, ok && (memory != argonMemory || passes != argonTime || threads != argonThreads)
}

// Sessions end after this long without use. Anything that talks to the
// server (a page load, Kotatsu opening a manga) counts as use, so devices in
// regular use stay signed in.
const sessionIdle = 90 * 24 * time.Hour

func idleCutoff() int64 { return time.Now().Add(-sessionIdle).Unix() }

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (db *DB) createUser(name, password string, admin bool) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" || password == "" {
		return 0, shownError("Name and password are required.")
	}
	// One statement, so two admins adding the same name at once can't race.
	res, err := db.Exec(`INSERT INTO users(name,pass,admin,created) VALUES(?,?,?,?) ON CONFLICT(name) DO NOTHING`,
		name, hashPassword(password), admin, now())
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, shownError(fmt.Sprintf("A user named %s already exists.", name))
	}
	return res.LastInsertId()
}

// setPassword changes the password and signs the user out everywhere, which
// also revokes every image link handed out to those sessions.
func (db *DB) setPassword(userID int64, password string) error {
	if password == "" {
		return shownError("Password is required.")
	}
	if _, err := db.Exec(`UPDATE users SET pass=? WHERE id=?`, hashPassword(password), userID); err != nil {
		return err
	}
	_, err := db.Exec(`DELETE FROM sessions WHERE user_id=?`, userID)
	return err
}

func (db *DB) checkPassword(name, password string) (*User, error) {
	var id int64
	var hash string
	err := db.QueryRow(`SELECT id,pass FROM users WHERE name=?`, strings.TrimSpace(name)).Scan(&id, &hash)
	if err != nil {
		// Spend the same time as a real check so names can't be probed.
		verifyPassword(dummyHash, password)
		return nil, errors.New("wrong username or password")
	}
	ok, upgrade := verifyPassword(hash, password)
	if !ok {
		return nil, errors.New("wrong username or password")
	}
	// Rehash old bcrypt passwords now that we have the plain text. This only
	// replaces the hash, so the user stays signed in everywhere.
	if upgrade {
		db.Exec(`UPDATE users SET pass=? WHERE id=?`, hashPassword(password), id)
	}
	return db.userByID(id)
}

func (db *DB) createSession(userID int64, kind, agent string) (string, error) {
	token := randomToken(32)
	_, err := db.Exec(`INSERT INTO sessions(hash,sid,user_id,kind,agent,created,used) VALUES(?,?,?,?,?,?,?)`,
		hashToken(token), randomToken(9), userID, kind, truncate(agent, 200), now(), now())
	return token, err
}

func (db *DB) sessionUser(token string) (*User, error) {
	if token == "" {
		return nil, errors.New("no session")
	}
	h := hashToken(token)
	var userID, used int64
	var sid string
	if err := db.QueryRow(`SELECT user_id,used,sid FROM sessions WHERE hash=?`, h).Scan(&userID, &used, &sid); err != nil {
		return nil, err
	}
	if used < idleCutoff() {
		db.Exec(`DELETE FROM sessions WHERE hash=?`, h)
		return nil, errors.New("session expired")
	}
	// Only touch the row occasionally so reading doesn't write on every request.
	if now()-used > 3600 {
		db.Exec(`UPDATE sessions SET used=? WHERE hash=?`, now(), h)
	}
	u, err := db.userByID(userID)
	if err != nil {
		return nil, err
	}
	u.SessionID = sid
	return u, nil
}

// deleteIdleSessions removes sessions past the idle limit. Expired sessions
// are refused anyway. This keeps them out of the device list.
func (db *DB) deleteIdleSessions() {
	db.Exec(`DELETE FROM sessions WHERE used < ?`, idleCutoff())
}

func (db *DB) deleteSession(token string) {
	db.Exec(`DELETE FROM sessions WHERE hash=?`, hashToken(token))
}

type Session struct {
	Hash    string
	Kind    string
	Agent   string
	Created int64
	Used    int64
	Current bool
}

func (db *DB) sessions(userID int64, currentToken string) ([]Session, error) {
	rows, err := db.Query(`SELECT hash,kind,agent,created,used FROM sessions WHERE user_id=? ORDER BY used DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cur := hashToken(currentToken)
	var out []Session
	for rows.Next() {
		var s Session
		rows.Scan(&s.Hash, &s.Kind, &s.Agent, &s.Created, &s.Used)
		s.Current = s.Hash == cur
		out = append(out, s)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Signed file links
//
// Kotatsu fetches covers and pages with its own HTTP client and can't add an
// Authorization header, so each link carries the ID of the session it was
// handed out on and an HMAC of that ID and the file path. Links don't expire
// because Kotatsu stores cover URLs in its database. Signing a device out
// deletes its session, which revokes every link it was given. Changing the
// password deletes all of the user's sessions.

var filePathRe = regexp.MustCompile(`^[0-9a-f-]{36}/(cover\.[a-z]+|thumb\.jpg|[0-9a-f-]{36}/[0-9]{3,4}\.(jpg|jpeg|png|gif|webp))$`)

func (s *Server) sign(sid, path string) string {
	mac := hmac.New(sha256.New, s.keys.sign)
	fmt.Fprintf(mac, "%s:%s", sid, path)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

func (s *Server) fileURL(u *User, path string) string {
	if path == "" {
		return ""
	}
	return fmt.Sprintf("/img/%s/%s/%s", u.SessionID, s.sign(u.SessionID, path), path)
}

// coverURL links a cover or its thumbnail. Their paths stay the same when
// MangaDex switches to another cover, and image links are cached for good,
// so the cover's file name goes in the query to make a new link.
func (s *Server) coverURL(u *User, m *Manga, path string) string {
	return s.fileURL(u, path) + "?v=" + url.QueryEscape(m.Cover)
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	sid, path := r.PathValue("sid"), r.PathValue("path")
	if sid == "" || !filePathRe.MatchString(path) {
		http.NotFound(w, r)
		return
	}
	if !hmac.Equal([]byte(s.sign(sid, path)), []byte(r.PathValue("sig"))) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var alive bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sessions WHERE sid=? AND used >= ?)`, sid, idleCutoff()).Scan(&alive)
	if !alive {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, s.mangaDir+"/"+path)
}

// Request authentication

type ctxKey int

const userKey ctxKey = 0

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(userKey).(*User)
	return u
}

func sessionToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// web wraps a browser page. Signed-out visitors go to the sign-in page, and
// form posts must come from this site.
func (s *Server) web(admin bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.db.sessionUser(sessionToken(r))
		if err != nil {
			http.Redirect(w, r, "/login?next="+urlEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if crossOrigin.Check(r) != nil {
			http.Error(w, "Cross-site request blocked", http.StatusForbidden)
			return
		}
		// Keep the cookie as long-lived as the session it carries.
		if r.Method == http.MethodGet {
			setSessionCookie(w, r, sessionToken(r))
		}
		if admin && !u.Admin {
			http.Error(w, "Only admins can do this", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

func (s *Server) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.db.sessionUser(sessionToken(r))
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Signed out"})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// crossOrigin blocks cross-site form posts. It trusts the browser's
// Sec-Fetch-Site header first and only falls back to comparing Origin with
// Host, so a reverse proxy that rewrites Host doesn't break same-site posts.
var crossOrigin = http.NewCrossOriginProtection()

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionIdle),
	})
}
