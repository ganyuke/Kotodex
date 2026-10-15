package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Chapter states. Chapters that are known but not wanted stay at stateSkipped
// so the back catalog of a newly followed manga isn't downloaded by surprise.
const (
	stateSkipped = 0
	stateQueued  = 1
	stateDone    = 2
	stateFailed  = 3
)

const schema = `
CREATE TABLE IF NOT EXISTS users(
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	pass TEXT NOT NULL,
	admin INTEGER NOT NULL DEFAULT 0,
	langs TEXT NOT NULL DEFAULT 'en',
	created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions(
	hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	sid TEXT NOT NULL,
	kind TEXT NOT NULL,
	agent TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	used INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS manga(
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	alt_titles TEXT NOT NULL DEFAULT '[]',
	description TEXT NOT NULL DEFAULT '',
	authors TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL DEFAULT '',
	rating TEXT NOT NULL DEFAULT '',
	tags TEXT NOT NULL DEFAULT '[]',
	year INTEGER NOT NULL DEFAULT 0,
	cover TEXT NOT NULL DEFAULT '',
	added INTEGER NOT NULL,
	refreshed INTEGER NOT NULL DEFAULT 0,
	checked INTEGER NOT NULL DEFAULT 0,
	latest INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS follows(
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	manga_id TEXT NOT NULL REFERENCES manga(id),
	langs TEXT NOT NULL DEFAULT '', -- '' means the user's default languages
	created INTEGER NOT NULL,
	PRIMARY KEY(user_id, manga_id)
);
CREATE TABLE IF NOT EXISTS chapters(
	id TEXT PRIMARY KEY,
	manga_id TEXT NOT NULL REFERENCES manga(id),
	volume TEXT NOT NULL DEFAULT '',
	number TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	lang TEXT NOT NULL,
	grp TEXT NOT NULL DEFAULT '',
	published INTEGER NOT NULL DEFAULT 0,
	pages INTEGER NOT NULL DEFAULT 0,
	state INTEGER NOT NULL DEFAULT 0,
	tries INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	files TEXT NOT NULL DEFAULT '[]',
	size INTEGER NOT NULL DEFAULT 0,
	downloaded INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS sessions_sid ON sessions(sid);
CREATE INDEX IF NOT EXISTS chapters_manga ON chapters(manga_id);
CREATE INDEX IF NOT EXISTS chapters_state ON chapters(state);
-- What each linked MangaDex account follows, as of the last import.
CREATE TABLE IF NOT EXISTS mdx_follows(
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	manga_id TEXT NOT NULL,
	PRIMARY KEY(user_id, manga_id)
);
-- Follows and unfollows made in Kotodex that differ from MangaDex.
CREATE TABLE IF NOT EXISTS follow_choices(
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	manga_id TEXT NOT NULL,
	following INTEGER NOT NULL,
	PRIMARY KEY(user_id, manga_id)
);
CREATE TABLE IF NOT EXISTS settings(
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mdx_accounts(
	user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	username TEXT NOT NULL,
	client_id TEXT NOT NULL,
	client_secret TEXT NOT NULL,
	refresh TEXT NOT NULL,
	synced INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
);
`

type DB struct {
	*sql.DB
	signedOut changes // sessions or users were removed, so live streams re-check theirs
}

func openDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// One connection keeps writes serialized without SQLITE_BUSY juggling.
	// Every query here is tiny, so this is not a bottleneck.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &DB{DB: db}, nil
}

func now() int64 { return time.Now().Unix() }

// usersChanged wraps an Exec that removes sessions or users. When it removed
// any, live streams wake and re-check theirs.
func (db *DB) usersChanged(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		db.signedOut.notify()
	}
	return n, nil
}

// Settings

type Settings struct {
	Interval       time.Duration
	DataSaver      bool
	ContentRatings []string
}

var defaultSettings = map[string]string{
	"interval":        "60",
	"data_saver":      "0",
	"content_ratings": "safe,suggestive,erotica",
}

func (db *DB) setting(key string) string {
	var v string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return defaultSettings[key]
	}
	return v
}

func (db *DB) setSetting(key, value string) error {
	_, err := db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (db *DB) settings() Settings {
	minutes, _ := strconv.Atoi(db.setting("interval"))
	if minutes < 10 {
		minutes = 10
	}
	return Settings{
		Interval:       time.Duration(minutes) * time.Minute,
		DataSaver:      db.setting("data_saver") == "1",
		ContentRatings: splitList(db.setting("content_ratings")),
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Users

type User struct {
	ID    int64
	Name  string
	Admin bool

	// Session the request came in on, which signs the image links handed
	// out on it. Empty outside a request.
	SessionID string
}

func (db *DB) userByID(id int64) (*User, error) {
	u := &User{}
	err := db.QueryRow(`SELECT id,name,admin FROM users WHERE id=?`, id).Scan(&u.ID, &u.Name, &u.Admin)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// deleteUser deletes a user unless they're the last admin. One statement, so
// two admins deleting each other can't both succeed.
func (db *DB) deleteUser(id int64) (bool, error) {
	n, err := db.usersChanged(db.Exec(`DELETE FROM users WHERE id=? AND (admin=0 OR (SELECT count(*) FROM users WHERE admin=1) > 1)`, id))
	return n > 0, err
}

func (db *DB) users() ([]User, error) {
	rows, err := db.Query(`SELECT id,name,admin FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Admin); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (db *DB) userCount() int {
	var n int
	db.QueryRow(`SELECT count(*) FROM users`).Scan(&n)
	return n
}

// Manga

type Manga struct {
	ID          string
	Title       string
	AltTitles   []string
	Description string
	Authors     []string
	Status      string
	Rating      string
	Tags        []string
	Year        int
	Cover       string
	Added       int64
	Refreshed   int64
	Checked     int64
	Latest      int64

	// Filled by list queries.
	Downloaded int
	Known      int
	Queued     int
	Followers  int
	Following  bool
}

const mangaCols = `m.id,m.title,m.alt_titles,m.description,m.authors,m.status,m.rating,m.tags,m.year,m.cover,m.added,m.refreshed,m.checked,m.latest`

func scanManga(sc interface{ Scan(...any) error }, extra ...any) (*Manga, error) {
	m := &Manga{}
	var alt, authors, tags string
	dest := append([]any{&m.ID, &m.Title, &alt, &m.Description, &authors, &m.Status, &m.Rating, &tags, &m.Year, &m.Cover, &m.Added, &m.Refreshed, &m.Checked, &m.Latest}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(alt), &m.AltTitles)
	json.Unmarshal([]byte(authors), &m.Authors)
	json.Unmarshal([]byte(tags), &m.Tags)
	return m, nil
}

func (db *DB) upsertManga(m *Manga) error {
	alt, _ := json.Marshal(nonNil(m.AltTitles))
	authors, _ := json.Marshal(nonNil(m.Authors))
	tags, _ := json.Marshal(nonNil(m.Tags))
	_, err := db.Exec(`INSERT INTO manga(id,title,alt_titles,description,authors,status,rating,tags,year,cover,added,refreshed)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET title=excluded.title, alt_titles=excluded.alt_titles, description=excluded.description,
		authors=excluded.authors, status=excluded.status, rating=excluded.rating, tags=excluded.tags, year=excluded.year,
		cover=excluded.cover, refreshed=excluded.refreshed`,
		m.ID, m.Title, string(alt), m.Description, string(authors), m.Status, m.Rating, string(tags), m.Year, m.Cover, now(), now())
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (db *DB) manga(id string, userID int64) (*Manga, error) {
	row := db.QueryRow(`SELECT `+mangaCols+`,
		(SELECT count(*) FROM chapters c WHERE c.manga_id=m.id AND c.state=2),
		(SELECT count(*) FROM chapters c WHERE c.manga_id=m.id),
		(SELECT count(*) FROM chapters c WHERE c.manga_id=m.id AND c.state=1),
		(SELECT count(*) FROM follows f WHERE f.manga_id=m.id),
		EXISTS(SELECT 1 FROM follows f WHERE f.manga_id=m.id AND f.user_id=?)
		FROM manga m WHERE m.id=?`, userID, id)
	var d, k, qd, fl int
	var fw bool
	m, err := scanManga(row, &d, &k, &qd, &fl, &fw)
	if err != nil {
		return nil, err
	}
	m.Downloaded, m.Known, m.Queued, m.Followers, m.Following = d, k, qd, fl, fw
	return m, nil
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

type LibraryQuery struct {
	UserID   int64
	Query    string
	Tags     []string
	Sort     string // updated, title, added
	Offset   int
	Limit    int
	Followed bool // only manga this user follows
	Readable bool // only manga with downloaded chapters
}

// library lists manga that someone follows or that have downloads.
func (db *DB) library(q LibraryQuery) ([]*Manga, error) {
	where := []string{`(EXISTS(SELECT 1 FROM follows f WHERE f.manga_id=m.id) OR EXISTS(SELECT 1 FROM chapters c WHERE c.manga_id=m.id AND c.state=2))`}
	args := []any{q.UserID}
	if q.Followed {
		where = append(where, `EXISTS(SELECT 1 FROM follows f WHERE f.manga_id=m.id AND f.user_id=?)`)
		args = append(args, q.UserID)
	}
	if q.Readable {
		where = append(where, `EXISTS(SELECT 1 FROM chapters c WHERE c.manga_id=m.id AND c.state=2)`)
	}
	if q.Query != "" {
		where = append(where, `(m.title LIKE ? ESCAPE '\' OR EXISTS(SELECT 1 FROM json_each(m.alt_titles) WHERE value LIKE ? ESCAPE '\'))`)
		like := "%" + likeEscaper.Replace(q.Query) + "%"
		args = append(args, like, like)
	}
	for _, t := range q.Tags {
		where = append(where, `EXISTS(SELECT 1 FROM json_each(m.tags) WHERE value=?)`)
		args = append(args, t)
	}
	order := `m.latest DESC, m.title`
	switch q.Sort {
	case "title":
		order = `m.title COLLATE NOCASE`
	case "added":
		order = `m.added DESC`
	}
	if q.Limit == 0 {
		q.Limit = -1
	}
	args = append(args, q.Limit, q.Offset)
	rows, err := db.Query(`SELECT `+mangaCols+`,
		(SELECT count(*) FROM chapters c WHERE c.manga_id=m.id AND c.state=2),
		(SELECT count(*) FROM chapters c WHERE c.manga_id=m.id),
		(SELECT count(*) FROM chapters c WHERE c.manga_id=m.id AND c.state=1),
		(SELECT count(*) FROM follows f WHERE f.manga_id=m.id),
		EXISTS(SELECT 1 FROM follows f WHERE f.manga_id=m.id AND f.user_id=?1)
		FROM manga m WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Manga
	for rows.Next() {
		var d, k, qd, fl int
		var fw bool
		m, err := scanManga(rows, &d, &k, &qd, &fl, &fw)
		if err != nil {
			return nil, err
		}
		m.Downloaded, m.Known, m.Queued, m.Followers, m.Following = d, k, qd, fl, fw
		out = append(out, m)
	}
	return out, rows.Err()
}

func (db *DB) allTags() ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT j.value FROM manga m, json_each(m.tags) j ORDER BY j.value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		rows.Scan(&t)
		out = append(out, t)
	}
	return out, rows.Err()
}

// followedManga returns IDs of every manga at least one user follows.
func (db *DB) followedManga() ([]*Manga, error) {
	rows, err := db.Query(`SELECT ` + mangaCols + ` FROM manga m WHERE EXISTS(SELECT 1 FROM follows f WHERE f.manga_id=m.id) ORDER BY m.checked`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Manga
	for rows.Next() {
		m, err := scanManga(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Following
//
// What a user follows on MangaDex (mdx_follows, empty without a linked
// account) is the default. Following or unfollowing in Kotodex is kept in
// follow_choices only when it differs from that default, and resetting
// drops the choice. The follows table holds the result and is what the rest
// of the server reads.

// setFollow records the user following or unfollowing in Kotodex.
func (db *DB) setFollow(userID int64, mangaID string, following bool) error {
	if following == db.onMangaDex(userID, mangaID) {
		return db.resetFollow(userID, mangaID)
	}
	if _, err := db.Exec(`INSERT INTO follow_choices(user_id,manga_id,following) VALUES(?,?,?)
		ON CONFLICT(user_id,manga_id) DO UPDATE SET following=excluded.following`, userID, mangaID, following); err != nil {
		return err
	}
	return db.applyFollow(userID, mangaID)
}

// resetFollow drops the user's choice so the follow matches MangaDex again.
func (db *DB) resetFollow(userID int64, mangaID string) error {
	if _, err := db.Exec(`DELETE FROM follow_choices WHERE user_id=? AND manga_id=?`, userID, mangaID); err != nil {
		return err
	}
	return db.applyFollow(userID, mangaID)
}

func (db *DB) onMangaDex(userID int64, mangaID string) bool {
	var v bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM mdx_follows WHERE user_id=? AND manga_id=?)`, userID, mangaID).Scan(&v)
	return v
}

// followChoice reports whether the user made a choice that differs from
// MangaDex for this manga.
func (db *DB) followChoice(userID int64, mangaID string) bool {
	var v bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM follow_choices WHERE user_id=? AND manga_id=?)`, userID, mangaID).Scan(&v)
	return v
}

// applyFollow brings the follows table in line with the choice, or with
// MangaDex when there is no choice.
//
// Each statement reads the current choice itself, so a sync or unlink running
// at the same time can't leave the follows table on a stale answer.
func (db *DB) applyFollow(userID int64, mangaID string) error {
	const want = `coalesce((SELECT following FROM follow_choices WHERE user_id=?1 AND manga_id=?2),
		EXISTS(SELECT 1 FROM mdx_follows WHERE user_id=?1 AND manga_id=?2))`
	if _, err := db.Exec(`INSERT INTO follows(user_id,manga_id,created) SELECT ?1,?2,?3 WHERE `+want+` ON CONFLICT DO NOTHING`,
		userID, mangaID, now()); err != nil {
		return err
	}
	_, err := db.Exec(`DELETE FROM follows WHERE user_id=?1 AND manga_id=?2 AND NOT `+want, userID, mangaID)
	return err
}

// setMangaDexFollows replaces the mirror of a user's MangaDex follows and
// applies whatever changed. Manga new to the server are added first.
func (db *DB) setMangaDexFollows(userID int64, list []*Manga) error {
	current := map[string]bool{}
	for _, m := range list {
		current[m.ID] = true
		var exists bool
		db.QueryRow(`SELECT EXISTS(SELECT 1 FROM manga WHERE id=?)`, m.ID).Scan(&exists)
		if !exists {
			if err := db.upsertManga(m); err != nil {
				return err
			}
		}
	}
	before := map[string]bool{}
	for _, id := range db.column(`SELECT manga_id FROM mdx_follows WHERE user_id=?`, userID) {
		before[id] = true
	}
	changed := []string{}
	for id := range current {
		if !before[id] {
			// Only while the account is still linked, in case it was
			// unlinked during the import.
			if _, err := db.Exec(`INSERT INTO mdx_follows(user_id,manga_id) SELECT ?1,?2
				WHERE EXISTS(SELECT 1 FROM mdx_accounts WHERE user_id=?1) ON CONFLICT DO NOTHING`, userID, id); err != nil {
				return err
			}
			changed = append(changed, id)
		}
	}
	for id := range before {
		if !current[id] {
			if _, err := db.Exec(`DELETE FROM mdx_follows WHERE user_id=? AND manga_id=?`, userID, id); err != nil {
				return err
			}
			changed = append(changed, id)
		}
	}
	for _, id := range changed {
		if err := db.applyFollow(userID, id); err != nil {
			return err
		}
	}
	return nil
}

// unlinkMangaDex forgets a user's MangaDex follows. With keep, manga that
// were followed only because of MangaDex stay followed, as choices.
func (db *DB) unlinkMangaDex(userID int64, keep bool) error {
	// Remove the account first so an import running now stops adding follows.
	if _, err := db.Exec(`DELETE FROM mdx_accounts WHERE user_id=?`, userID); err != nil {
		return err
	}
	ids := db.column(`SELECT manga_id FROM mdx_follows WHERE user_id=?`, userID)
	if keep {
		if _, err := db.Exec(`INSERT INTO follow_choices(user_id,manga_id,following)
			SELECT user_id, manga_id, 1 FROM mdx_follows WHERE user_id=? ON CONFLICT DO NOTHING`, userID); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`DELETE FROM mdx_follows WHERE user_id=?`, userID); err != nil {
		return err
	}
	// Without MangaDex, unfollowing is the default, so those choices no
	// longer differ from anything.
	if _, err := db.Exec(`DELETE FROM follow_choices WHERE user_id=? AND following=0`, userID); err != nil {
		return err
	}
	for _, id := range ids {
		if err := db.applyFollow(userID, id); err != nil {
			return err
		}
	}
	return nil
}

// column runs a query that returns one text column.
func (db *DB) column(query string, args ...any) []string {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		rows.Scan(&v)
		out = append(out, v)
	}
	return out
}

// Languages
//
// A follow with no languages of its own uses the user's default languages,
// as they are now. Setting languages for one manga overrides that, and
// resetting goes back to the defaults.

func (db *DB) userLangs(userID int64) []string {
	var v string
	db.QueryRow(`SELECT langs FROM users WHERE id=?`, userID).Scan(&v)
	return splitList(v)
}

// followLangs returns the languages in effect for one follow, and whether
// they were set for this manga instead of coming from the defaults.
func (db *DB) followLangs(userID int64, mangaID string) (langs []string, custom bool) {
	var own, def string
	db.QueryRow(`SELECT f.langs, u.langs FROM follows f JOIN users u ON u.id=f.user_id WHERE f.user_id=? AND f.manga_id=?`,
		userID, mangaID).Scan(&own, &def)
	if own != "" {
		return splitList(own), true
	}
	return splitList(def), false
}

// setFollowLangs sets languages for one manga. Picking exactly the defaults
// is the same as resetting.
func (db *DB) setFollowLangs(userID int64, mangaID string, langs []string) error {
	v := strings.Join(langs, ",")
	if sameSet(langs, db.userLangs(userID)) {
		v = ""
	}
	_, err := db.Exec(`UPDATE follows SET langs=? WHERE user_id=? AND manga_id=?`, v, userID, mangaID)
	return err
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

// wantedLangs is every language at least one follower wants new chapters in.
func (db *DB) wantedLangs(mangaID string) map[string]bool {
	out := map[string]bool{}
	for _, v := range db.column(`SELECT CASE WHEN f.langs='' THEN u.langs ELSE f.langs END
		FROM follows f JOIN users u ON u.id=f.user_id WHERE f.manga_id=?`, mangaID) {
		for _, l := range splitList(v) {
			out[l] = true
		}
	}
	return out
}

type LangCount struct {
	Code       string
	Chapters   int
	Downloaded int
}

// mangaLangs lists the languages a manga has chapters in, most chapters first.
func (db *DB) mangaLangs(mangaID string) []LangCount {
	rows, err := db.Query(`SELECT lang, count(*), sum(state=2) FROM chapters WHERE manga_id=? GROUP BY lang ORDER BY count(*) DESC, lang`, mangaID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []LangCount
	for rows.Next() {
		var l LangCount
		rows.Scan(&l.Code, &l.Chapters, &l.Downloaded)
		out = append(out, l)
	}
	return out
}

// Chapters

type Chapter struct {
	ID         string
	MangaID    string
	Volume     string
	Number     string
	Title      string
	Lang       string
	Group      string
	Published  int64
	Pages      int
	State      int
	Tries      int
	Error      string
	Files      []string
	Size       int64
	Downloaded int64

	MangaTitle string // filled by queue queries
}

const chapterCols = `c.id,c.manga_id,c.volume,c.number,c.title,c.lang,c.grp,c.published,c.pages,c.state,c.tries,c.error,c.files,c.size,c.downloaded`

func scanChapter(sc interface{ Scan(...any) error }, extra ...any) (*Chapter, error) {
	c := &Chapter{}
	var files string
	dest := append([]any{&c.ID, &c.MangaID, &c.Volume, &c.Number, &c.Title, &c.Lang, &c.Group, &c.Published, &c.Pages, &c.State, &c.Tries, &c.Error, &files, &c.Size, &c.Downloaded}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(files), &c.Files)
	return c, nil
}

func (db *DB) queryChapters(query string, args ...any) ([]*Chapter, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chapter
	for rows.Next() {
		c, err := scanChapter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// chapterOrder sorts chapters oldest first by volume, then chapter number.
// Numbers are text on MangaDex ("10.5", "" for oneshots), so cast for sorting.
const chapterOrder = `CAST(NULLIF(c.volume,'') AS REAL) IS NULL, CAST(NULLIF(c.volume,'') AS REAL), CAST(NULLIF(c.number,'') AS REAL), c.published`

func (db *DB) chapters(mangaID string) ([]*Chapter, error) {
	return db.queryChapters(`SELECT `+chapterCols+` FROM chapters c WHERE c.manga_id=? ORDER BY `+chapterOrder, mangaID)
}

func (db *DB) readableChapters(mangaID string) ([]*Chapter, error) {
	return db.queryChapters(`SELECT `+chapterCols+` FROM chapters c WHERE c.manga_id=? AND c.state=2 ORDER BY `+chapterOrder, mangaID)
}

func (db *DB) chapter(id string) (*Chapter, error) {
	return scanChapter(db.QueryRow(`SELECT `+chapterCols+` FROM chapters c WHERE c.id=?`, id))
}

func (db *DB) queue() ([]*Chapter, error) {
	rows, err := db.Query(`SELECT ` + chapterCols + `, m.title FROM chapters c JOIN manga m ON m.id=c.manga_id
		WHERE c.state IN (1,3) ORDER BY c.state, c.published LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chapter
	for rows.Next() {
		var title string
		c, err := scanChapter(rows, &title)
		if err != nil {
			return nil, err
		}
		c.MangaTitle = title
		out = append(out, c)
	}
	return out, rows.Err()
}

// history lists downloaded chapters, most recent download first.
func (db *DB) history(limit, offset int) ([]*Chapter, error) {
	rows, err := db.Query(`SELECT `+chapterCols+`, m.title FROM chapters c JOIN manga m ON m.id=c.manga_id
		WHERE c.state=2 ORDER BY c.downloaded DESC, c.rowid DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chapter
	for rows.Next() {
		var title string
		c, err := scanChapter(rows, &title)
		if err != nil {
			return nil, err
		}
		c.MangaTitle = title
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) nextQueued() (*Chapter, error) {
	return scanChapter(db.QueryRow(`SELECT ` + chapterCols + ` FROM chapters c WHERE c.state=1 ORDER BY c.tries, c.published LIMIT 1`))
}

// insertChapter adds a chapter MangaDex reported. Existing rows only get their
// metadata refreshed and keep their download state.
func (db *DB) insertChapter(c *Chapter) (bool, error) {
	res, err := db.Exec(`INSERT INTO chapters(id,manga_id,volume,number,title,lang,grp,published,pages,state)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		c.ID, c.MangaID, c.Volume, c.Number, c.Title, c.Lang, c.Group, c.Published, c.Pages, c.State)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_, err = db.Exec(`UPDATE chapters SET volume=?,number=?,title=?,grp=?,published=?,pages=? WHERE id=?`,
			c.Volume, c.Number, c.Title, c.Group, c.Published, c.Pages, c.ID)
	}
	return n > 0, err
}

// hasChapterNumber reports whether a chapter with this number and language is
// already downloaded or queued, which means another group's upload of the
// same chapter isn't needed.
func (db *DB) hasChapterNumber(mangaID, lang, volume, number string) bool {
	if number == "" {
		return false
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM chapters WHERE manga_id=? AND lang=? AND number=? AND (volume=? OR volume='' OR ?='') AND state IN (1,2)`,
		mangaID, lang, number, volume, volume).Scan(&n)
	return n > 0
}

func (db *DB) setChapterState(id string, state int) error {
	_, err := db.Exec(`UPDATE chapters SET state=?, tries=0, error='' WHERE id=?`, state, id)
	return err
}
