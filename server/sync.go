package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Kotatsu sync
//
// Kotatsu and its forks sync history and favorites with a server set under
// Settings, Services, Synchronization settings. Kotodex answers the same
// three requests as Kotatsu's own sync server, so pointing the app there keeps
// everyone's reading on this server. The app parses replies strictly: a field
// it doesn't know, or one that's missing, fails the whole sync. So rows go
// back with exactly the fields the app sent, and each manga goes back as the
// JSON object the app sent for it.
//
// The app uploads its whole history (or favorites) table each time and adds
// or updates every row in the reply. It never deletes anything because of a
// reply. Removing something on the phone uploads the row with deleted_at set.

const syncSchema = `
-- Manga as the app described them, kept per user so one account's uploads
-- can't change what another account's phone gets back.
CREATE TABLE IF NOT EXISTS sync_manga(
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	id INTEGER NOT NULL, -- the app's manga ID, a 64-bit hash
	source TEXT NOT NULL,
	url TEXT NOT NULL,
	title TEXT NOT NULL,
	cover TEXT NOT NULL DEFAULT '', -- the cover's address on its source
	data TEXT NOT NULL, -- the manga object exactly as sent, sent back as is
	PRIMARY KEY(user_id, id)
);
CREATE TABLE IF NOT EXISTS sync_history(
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	manga_id INTEGER NOT NULL,
	created_at INTEGER NOT NULL, -- milliseconds, as the app sends them
	updated_at INTEGER NOT NULL,
	chapter_id INTEGER NOT NULL,
	page INTEGER NOT NULL,
	scroll REAL NOT NULL,
	percent REAL NOT NULL, -- of the whole manga, 0 to 1
	chapters INTEGER NOT NULL,
	deleted_at INTEGER NOT NULL,
	PRIMARY KEY(user_id, manga_id)
);
CREATE TABLE IF NOT EXISTS sync_categories(
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	id INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	sort_key INTEGER NOT NULL,
	title TEXT NOT NULL,
	sort_order TEXT NOT NULL,
	track INTEGER NOT NULL,
	show_in_lib INTEGER NOT NULL,
	deleted_at INTEGER NOT NULL,
	PRIMARY KEY(user_id, id)
);
CREATE TABLE IF NOT EXISTS sync_favourites(
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	manga_id INTEGER NOT NULL,
	category_id INTEGER NOT NULL,
	sort_key INTEGER NOT NULL,
	pinned INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	deleted_at INTEGER NOT NULL,
	PRIMARY KEY(user_id, manga_id, category_id)
);
-- One row each time a manga's reading position changed, at the time the
-- phone saved it. Reading sessions are worked out from these.
CREATE TABLE IF NOT EXISTS reading_events(
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	manga_id INTEGER NOT NULL,
	chapter_id INTEGER NOT NULL,
	page INTEGER NOT NULL,
	percent REAL NOT NULL,
	at INTEGER NOT NULL, -- milliseconds
	UNIQUE(user_id, manga_id, at)
);
CREATE INDEX IF NOT EXISTS reading_events_at ON reading_events(at);
-- Manga added to or removed from a favorites category.
CREATE TABLE IF NOT EXISTS list_events(
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	manga_id INTEGER NOT NULL,
	category_id INTEGER NOT NULL,
	kind TEXT NOT NULL, -- added or removed
	at INTEGER NOT NULL, -- milliseconds
	UNIQUE(user_id, manga_id, category_id, kind, at)
);
CREATE INDEX IF NOT EXISTS list_events_at ON list_events(at);
`

// The Kotodex plugin's source, as the app names it in synced manga:
// "content:" + the plugin's package + "/" + its content provider.
const kotodexSource = "content:org.nextcolor.kotodex/org.nextcolor.kotodex.manga"

// Rows as the app sends them (HistorySyncDto, FavouriteCategorySyncDto and
// FavouriteSyncDto in Kotatsu). Every field is always sent back.

type syncHistory struct {
	MangaID   int64           `json:"manga_id"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
	ChapterID int64           `json:"chapter_id"`
	Page      int64           `json:"page"`
	Scroll    float64         `json:"scroll"`
	Percent   float64         `json:"percent"`
	DeletedAt int64           `json:"deleted_at"`
	Chapters  int64           `json:"chapters"`
	Manga     json.RawMessage `json:"manga"`
}

type syncCategory struct {
	ID        int64  `json:"category_id"`
	CreatedAt int64  `json:"created_at"`
	SortKey   int64  `json:"sort_key"`
	Title     string `json:"title"`
	Order     string `json:"order"`
	Track     bool   `json:"track"`
	ShowInLib bool   `json:"show_in_lib"`
	DeletedAt int64  `json:"deleted_at"`
}

type syncFavourite struct {
	MangaID    int64           `json:"manga_id"`
	Manga      json.RawMessage `json:"manga"`
	CategoryID int64           `json:"category_id"`
	SortKey    int64           `json:"sort_key"`
	Pinned     bool            `json:"pinned"`
	CreatedAt  int64           `json:"created_at"`
	DeletedAt  int64           `json:"deleted_at"`
}

type syncPackage struct {
	History    []syncHistory   `json:"history,omitempty"`
	Categories []syncCategory  `json:"categories,omitempty"`
	Favourites []syncFavourite `json:"favourites,omitempty"`
	Timestamp  int64           `json:"timestamp"`
}

// syncMangaInfo is the part of the app's manga object Kotodex reads.
type syncMangaInfo struct {
	ID     int64  `json:"manga_id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Source string `json:"source"`
	Cover  string `json:"cover_url"`
}

// Uploads hold the whole table, so they can be large for big libraries.
const maxSyncBody = 32 << 20

// Rows per statement. Each history row takes 10 parameters, well under
// SQLite's limit.
const syncChunk = 200

func nowMillis() int64 { return time.Now().UnixMilli() }

// mangaRow checks a manga object from the app and returns it compacted, as
// stored and sent back.
func mangaRow(id int64, raw json.RawMessage) (syncMangaInfo, string, error) {
	var info syncMangaInfo
	if err := json.Unmarshal(raw, &info); err != nil || info.Source == "" {
		return info, "", errors.New("bad manga")
	}
	if info.ID != id {
		return info, "", errors.New("manga ID doesn't match")
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return info, "", err
	}
	return info, b.String(), nil
}

// placeholders returns "(?,?,?),(?,?,?)" for n rows of k values.
func placeholders(n, k int) string {
	row := "(" + strings.TrimSuffix(strings.Repeat("?,", k), ",") + ")"
	return strings.TrimSuffix(strings.Repeat(row+",", n), ",")
}

// lastParam names the last argument, for a value the statement uses after
// its VALUES list. Plain "?" are numbered in order of appearance, so "?1"
// there would mean the first value instead.
func lastParam(args []any) string { return fmt.Sprintf("?%d", len(args)) }

type mangaUpload struct {
	id   int64
	info syncMangaInfo
	data string
}

func (db *DB) saveSyncManga(userID int64, list []mangaUpload) error {
	for chunk := range slices.Chunk(list, syncChunk) {
		args := make([]any, 0, len(chunk)*7)
		for _, m := range chunk {
			args = append(args, userID, m.id, m.info.Source, m.info.URL, truncate(m.info.Title, 500), truncate(m.info.Cover, 2000), m.data)
		}
		_, err := db.Exec(`INSERT INTO sync_manga(user_id,id,source,url,title,cover,data) VALUES `+placeholders(len(chunk), 7)+`
			ON CONFLICT(user_id,id) DO UPDATE SET source=excluded.source, url=excluded.url, title=excluded.title,
				cover=excluded.cover, data=excluded.data`, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

// saveHistory stores an upload. A row only replaces the stored one when the
// phone saved it later, so a phone that was offline with old progress can't
// undo newer reading from another device. Each row that moves a manga's
// position forward in time is also logged as a reading event.
func (db *DB) saveHistory(userID int64, rows []syncHistory) error {
	var manga []mangaUpload
	for _, h := range rows {
		info, data, err := mangaRow(h.MangaID, h.Manga)
		if err != nil {
			return shownError(fmt.Sprintf("History for manga %d: %v", h.MangaID, err))
		}
		manga = append(manga, mangaUpload{h.MangaID, info, data})
	}
	if err := db.saveSyncManga(userID, manga); err != nil {
		return err
	}
	for chunk := range slices.Chunk(rows, syncChunk) {
		// Events first, while the stored rows still show the old times.
		var args []any
		for _, h := range chunk {
			args = append(args, h.MangaID, h.ChapterID, h.Page, h.Percent, h.UpdatedAt, h.DeletedAt)
		}
		args = append(args, userID)
		_, err := db.Exec(`WITH v(manga_id,chapter_id,page,percent,at,deleted_at) AS (VALUES `+placeholders(len(chunk), 6)+`)
			INSERT INTO reading_events(user_id,manga_id,chapter_id,page,percent,at)
			SELECT `+lastParam(args)+`,v.manga_id,v.chapter_id,v.page,v.percent,v.at FROM v
			WHERE v.deleted_at=0 AND NOT EXISTS(SELECT 1 FROM sync_history h WHERE h.user_id=`+lastParam(args)+` AND h.manga_id=v.manga_id AND h.updated_at>=v.at)
			ON CONFLICT DO NOTHING`, args...)
		if err != nil {
			return fmt.Errorf("log reading: %w", err)
		}
		args = args[:0]
		for _, h := range chunk {
			args = append(args, userID, h.MangaID, h.CreatedAt, h.UpdatedAt, h.ChapterID, h.Page, h.Scroll, h.Percent, h.Chapters, h.DeletedAt)
		}
		// A later save replaces the row. A removal saved at the same time
		// or earlier still counts, so removing on one phone sticks.
		_, err = db.Exec(`INSERT INTO sync_history(user_id,manga_id,created_at,updated_at,chapter_id,page,scroll,percent,chapters,deleted_at)
			VALUES `+placeholders(len(chunk), 10)+`
			ON CONFLICT(user_id,manga_id) DO UPDATE SET
				created_at=CASE WHEN excluded.updated_at>sync_history.updated_at THEN excluded.created_at ELSE sync_history.created_at END,
				chapter_id=CASE WHEN excluded.updated_at>sync_history.updated_at THEN excluded.chapter_id ELSE sync_history.chapter_id END,
				page=CASE WHEN excluded.updated_at>sync_history.updated_at THEN excluded.page ELSE sync_history.page END,
				scroll=CASE WHEN excluded.updated_at>sync_history.updated_at THEN excluded.scroll ELSE sync_history.scroll END,
				percent=CASE WHEN excluded.updated_at>sync_history.updated_at THEN excluded.percent ELSE sync_history.percent END,
				chapters=CASE WHEN excluded.updated_at>sync_history.updated_at THEN excluded.chapters ELSE sync_history.chapters END,
				deleted_at=CASE WHEN excluded.updated_at>sync_history.updated_at THEN excluded.deleted_at ELSE max(sync_history.deleted_at, excluded.deleted_at) END,
				updated_at=max(sync_history.updated_at, excluded.updated_at)
			WHERE excluded.updated_at>sync_history.updated_at OR excluded.deleted_at>sync_history.deleted_at`, args...)
		if err != nil {
			return fmt.Errorf("save history: %w", err)
		}
	}
	return nil
}

func (db *DB) syncedHistory(userID int64) ([]syncHistory, error) {
	rows, err := db.Query(`SELECT h.manga_id,h.created_at,h.updated_at,h.chapter_id,h.page,h.scroll,h.percent,h.deleted_at,h.chapters,m.data
		FROM sync_history h JOIN sync_manga m ON m.user_id=h.user_id AND m.id=h.manga_id
		WHERE h.user_id=? ORDER BY h.manga_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []syncHistory{}
	for rows.Next() {
		var h syncHistory
		var data string
		if err := rows.Scan(&h.MangaID, &h.CreatedAt, &h.UpdatedAt, &h.ChapterID, &h.Page, &h.Scroll, &h.Percent, &h.DeletedAt, &h.Chapters, &data); err != nil {
			return nil, err
		}
		h.Manga = json.RawMessage(data)
		out = append(out, h)
	}
	return out, rows.Err()
}

// saveFavourites stores an upload. Like Kotatsu's own server, the latest
// upload wins for favorites and categories. Manga showing up in a category,
// or being removed from one, are logged for the activity feed.
func (db *DB) saveFavourites(userID int64, cats []syncCategory, favs []syncFavourite) error {
	for chunk := range slices.Chunk(cats, syncChunk) {
		args := make([]any, 0, len(chunk)*9)
		for _, c := range chunk {
			args = append(args, userID, c.ID, c.CreatedAt, c.SortKey, truncate(c.Title, 200), truncate(c.Order, 50), c.Track, c.ShowInLib, c.DeletedAt)
		}
		_, err := db.Exec(`INSERT INTO sync_categories(user_id,id,created_at,sort_key,title,sort_order,track,show_in_lib,deleted_at)
			VALUES `+placeholders(len(chunk), 9)+`
			ON CONFLICT(user_id,id) DO UPDATE SET created_at=excluded.created_at, sort_key=excluded.sort_key, title=excluded.title,
				sort_order=excluded.sort_order, track=excluded.track, show_in_lib=excluded.show_in_lib, deleted_at=excluded.deleted_at`, args...)
		if err != nil {
			return fmt.Errorf("save categories: %w", err)
		}
	}
	var manga []mangaUpload
	for _, f := range favs {
		info, data, err := mangaRow(f.MangaID, f.Manga)
		if err != nil {
			return shownError(fmt.Sprintf("Favorite %d: %v", f.MangaID, err))
		}
		manga = append(manga, mangaUpload{f.MangaID, info, data})
	}
	if err := db.saveSyncManga(userID, manga); err != nil {
		return err
	}
	for chunk := range slices.Chunk(favs, syncChunk) {
		var args []any
		for _, f := range chunk {
			args = append(args, f.MangaID, f.CategoryID, f.CreatedAt, f.DeletedAt)
		}
		args = append(args, userID)
		// Added: live now, and not live before. Removed: the other way round.
		_, err := db.Exec(`WITH v(manga_id,category_id,created_at,deleted_at) AS (VALUES `+placeholders(len(chunk), 4)+`)
			INSERT INTO list_events(user_id,manga_id,category_id,kind,at)
			SELECT `+lastParam(args)+`,v.manga_id,v.category_id,
				CASE WHEN v.deleted_at=0 THEN 'added' ELSE 'removed' END,
				CASE WHEN v.deleted_at=0 THEN v.created_at ELSE v.deleted_at END
			FROM v LEFT JOIN sync_favourites f ON f.user_id=`+lastParam(args)+` AND f.manga_id=v.manga_id AND f.category_id=v.category_id
			WHERE (v.deleted_at=0 AND (f.manga_id IS NULL OR f.deleted_at<>0)) OR (v.deleted_at<>0 AND f.deleted_at=0)
			ON CONFLICT DO NOTHING`, args...)
		if err != nil {
			return fmt.Errorf("log lists: %w", err)
		}
		args = args[:0]
		for _, f := range chunk {
			args = append(args, userID, f.MangaID, f.CategoryID, f.SortKey, f.Pinned, f.CreatedAt, f.DeletedAt)
		}
		_, err = db.Exec(`INSERT INTO sync_favourites(user_id,manga_id,category_id,sort_key,pinned,created_at,deleted_at)
			VALUES `+placeholders(len(chunk), 7)+`
			ON CONFLICT(user_id,manga_id,category_id) DO UPDATE SET sort_key=excluded.sort_key, pinned=excluded.pinned,
				created_at=excluded.created_at, deleted_at=excluded.deleted_at`, args...)
		if err != nil {
			return fmt.Errorf("save favorites: %w", err)
		}
	}
	return nil
}

func (db *DB) syncedCategories(userID int64) ([]syncCategory, error) {
	rows, err := db.Query(`SELECT id,created_at,sort_key,title,sort_order,track,show_in_lib,deleted_at
		FROM sync_categories WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []syncCategory{}
	for rows.Next() {
		var c syncCategory
		if err := rows.Scan(&c.ID, &c.CreatedAt, &c.SortKey, &c.Title, &c.Order, &c.Track, &c.ShowInLib, &c.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) syncedFavourites(userID int64) ([]syncFavourite, error) {
	rows, err := db.Query(`SELECT f.manga_id,m.data,f.category_id,f.sort_key,f.pinned,f.created_at,f.deleted_at
		FROM sync_favourites f JOIN sync_manga m ON m.user_id=f.user_id AND m.id=f.manga_id
		WHERE f.user_id=? ORDER BY f.category_id,f.manga_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []syncFavourite{}
	for rows.Next() {
		var f syncFavourite
		var data string
		if err := rows.Scan(&f.MangaID, &data, &f.CategoryID, &f.SortKey, &f.Pinned, &f.CreatedAt, &f.DeletedAt); err != nil {
			return nil, err
		}
		f.Manga = json.RawMessage(data)
		out = append(out, f)
	}
	return out, rows.Err()
}

// sameRows reports whether the reply would hold exactly the rows the phone
// sent, in which case Kotatsu's server answers 204 and the app skips writing.
func sameRows[T any](sent, stored []T) bool {
	if len(sent) != len(stored) {
		return false
	}
	enc := func(list []T) []string {
		out := make([]string, len(list))
		for i, v := range list {
			b, _ := json.Marshal(v)
			out[i] = string(b)
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(enc(sent), enc(stored))
}

func readSyncPackage(w http.ResponseWriter, r *http.Request) (syncPackage, bool) {
	var p syncPackage
	if r.Method != http.MethodPost {
		return p, true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSyncBody)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, "Bad request")
		return p, false
	}
	return p, true
}

func (s *Server) syncFailed(w http.ResponseWriter, err error) {
	var shown shownError
	if errors.As(err, &shown) {
		writeJSON(w, http.StatusBadRequest, shown.Error())
		return
	}
	writeJSON(w, http.StatusInternalServerError, userMessage(err))
}

// syncAuth signs the app in. The app only takes an email address, so the
// part before the last "@" is the Kotodex username and the rest is ignored.
// A plain username works too, for apps that don't check.
func (s *Server) syncAuth(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	name := req.Email
	if i := strings.LastIndex(name, "@"); i >= 0 {
		name = name[:i]
	}
	keys := loginKeys(r, name)
	if !s.limiter.allow(keys) {
		writeJSON(w, http.StatusTooManyRequests, "Too many attempts. Try again later.")
		return
	}
	u, err := s.db.checkPassword(name, req.Password)
	if err != nil {
		s.limiter.fail(keys)
		writeJSON(w, http.StatusUnauthorized, "Wrong username or password")
		return
	}
	token, err := s.db.createSession(u.ID, "sync", r.UserAgent())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, userMessage(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (s *Server) syncHistory(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	req, ok := readSyncPackage(w, r)
	if !ok {
		return
	}
	if err := s.db.saveHistory(u.ID, req.History); err != nil {
		s.syncFailed(w, err)
		return
	}
	stored, err := s.db.syncedHistory(u.ID)
	if err != nil {
		s.syncFailed(w, err)
		return
	}
	s.synced(u)
	if r.Method == http.MethodPost && sameRows(req.History, stored) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, syncPackage{History: stored, Timestamp: nowMillis()})
}

func (s *Server) syncFavourites(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	req, ok := readSyncPackage(w, r)
	if !ok {
		return
	}
	if err := s.db.saveFavourites(u.ID, req.Categories, req.Favourites); err != nil {
		s.syncFailed(w, err)
		return
	}
	cats, err := s.db.syncedCategories(u.ID)
	if err != nil {
		s.syncFailed(w, err)
		return
	}
	favs, err := s.db.syncedFavourites(u.ID)
	if err != nil {
		s.syncFailed(w, err)
		return
	}
	s.synced(u)
	if r.Method == http.MethodPost && sameRows(req.Categories, cats) && sameRows(req.Favourites, favs) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, syncPackage{Categories: cats, Favourites: favs, Timestamp: nowMillis()})
}

// syncAPI wraps the sync requests. They take a token from the app's sync
// sign-in only, sent as a bearer token. A browser's cookie or the plugin's
// token don't work here, and a sync token doesn't work anywhere else.
func (s *Server) syncAPI(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, "Signed out")
			return
		}
		u, err := s.db.syncUser(strings.TrimPrefix(auth, "Bearer "))
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, "Signed out")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// synced records the time for the settings page and wakes live pages.
func (s *Server) synced(u *User) {
	s.db.Exec(`UPDATE users SET synced=? WHERE id=?`, now(), u.ID)
	s.dl.Changed()
}
