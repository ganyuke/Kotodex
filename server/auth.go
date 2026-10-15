package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "kotodex_session"

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)

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
		return 0, errors.New("name and password are required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO users(name,pass,admin,created) VALUES(?,?,?,?)`, name, string(hash), admin, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("a user named %s already exists", name)
		}
		return 0, err
	}
	return res.LastInsertId()
}

// setPassword changes the password and signs the user out everywhere, which
// also revokes every image link handed out to those sessions.
func (db *DB) setPassword(userID int64, password string) error {
	if password == "" {
		return errors.New("password is required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE users SET pass=? WHERE id=?`, string(hash), userID); err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM sessions WHERE user_id=?`, userID)
	return err
}

func (db *DB) checkPassword(name, password string) (*User, error) {
	var id int64
	var hash string
	err := db.QueryRow(`SELECT id,pass FROM users WHERE name=?`, strings.TrimSpace(name)).Scan(&id, &hash)
	if err != nil {
		// Spend the same time as a real check so names can't be probed.
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, errors.New("wrong username or password")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, errors.New("wrong username or password")
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
		if r.Method == http.MethodPost && !sameOrigin(r) {
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

// sameOrigin checks Origin (or Referer) against Host. Together with the
// SameSite cookie this blocks cross-site form posts.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true
	}
	origin = strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	host, _, _ := strings.Cut(origin, "/")
	return host == r.Host || host == r.Header.Get("X-Forwarded-Host")
}

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
