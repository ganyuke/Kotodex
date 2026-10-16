package main

import (
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Reading activity
//
// Everything here reads what Kotatsu synced (sync.go). Each person picks who
// sees their reading and lists: everything, only manga on Kotodex, or
// nothing. What someone follows on Kotodex is visible to everyone, as before.

const (
	shareAll     = "all"
	shareKotodex = "kotodex"
	shareNone    = "none"
)

// sessionGap splits reading into sessions. The app saves your place at least
// every few minutes while you read, so a longer gap means you stopped.
const sessionGap = 30 * time.Minute

// finishedAt is Kotatsu's own threshold for a manga read to the end.
const finishedAt = 0.99999

// shareFilter limits rows to what @viewer may see. user and source name the
// columns holding the row's owner and its manga's source, and users must be
// joined as "us". Every query that shows someone's reading or lists goes
// through this, admins included.
func shareFilter(user, source string) string {
	return fmt.Sprintf(`(%s=@viewer OR us.share='all' OR (us.share='kotodex' AND %s=@kotodex))`, user, source)
}

func shareArgs(viewerID int64, more ...any) []any {
	return append([]any{sql.Named("viewer", viewerID), sql.Named("kotodex", kotodexSource)}, more...)
}

// stableID is the plugin's ID for a manga or chapter: 64-bit FNV-1a of
// "manga:<id>" or "chapter:<id>". Kotatsu keeps these as its own IDs, so they
// come back in synced history.
func stableID(s string) int64 {
	h := uint64(0xcbf29ce484222325)
	for _, b := range []byte(s) {
		h ^= uint64(b)
		h *= 0x100000001b3
	}
	return int64(h)
}

type Person struct {
	ID     int64
	Name   string
	Share  string
	Synced int64
}

func (db *DB) person(name string) (*Person, error) {
	p := &Person{}
	err := db.QueryRow(`SELECT id,name,share,synced FROM users WHERE name=?`, name).Scan(&p.ID, &p.Name, &p.Share, &p.Synced)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (db *DB) setShare(userID int64, share string) error {
	if share != shareAll && share != shareKotodex && share != shareNone {
		return shownError("Pick who sees your reading.")
	}
	_, err := db.Exec(`UPDATE users SET share=? WHERE id=?`, share, userID)
	return err
}

// Shows reports whether viewer sees any of p's reading and lists.
func (p *Person) Shows(viewer *User) bool {
	return viewer.ID == p.ID || p.Share != shareNone
}

// SyncedManga is a manga as someone's app synced it, with what Kotodex knows
// about it when it came from Kotodex.
type SyncedManga struct {
	ID       int64 // the app's ID
	Title    string
	Source   string
	URL      string
	Local    *Manga // set for manga from Kotodex
	Cover    string // link to show the cover with, when there's one
	CoverURL string // the cover's address on its source
}

func (m *SyncedManga) FromKotodex() bool { return m.Source == kotodexSource }

// SourceName is how the app's source names read on a page.
func (m *SyncedManga) SourceName() string {
	if m.FromKotodex() {
		return "Kotodex"
	}
	s := strings.TrimPrefix(m.Source, "content:")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "Other source"
	}
	s = strings.ToLower(strings.ReplaceAll(s, "_", " "))
	return strings.ToUpper(s[:1]) + s[1:]
}

// Place is where someone is in a manga.
type Place struct {
	SyncedManga
	UserID    int64
	UserName  string
	ChapterID int64
	Page      int
	Percent   float64
	Chapters  int
	Updated   int64 // milliseconds
	Chapter   *Chapter
}

func (p *Place) Finished() bool { return p.Percent >= finishedAt }

// Percent100 is the progress for <progress> bars.
func (p *Place) Percent100() int { return int(p.Percent * 100) }

// Where says where someone is in words.
func (p *Place) Where() string {
	if p.Finished() {
		return "Finished"
	}
	if p.Chapter != nil {
		return fmt.Sprintf("%s, page %d", chapterLabel(p.Chapter), p.Page+1)
	}
	if p.Chapters > 0 {
		return fmt.Sprintf("%d%% of %s", p.Percent100(), plural(p.Chapters, "chapter"))
	}
	return fmt.Sprintf("%d%%", p.Percent100())
}

// ReadSession is a stretch of reading one manga, from the app's saves.
type ReadSession struct {
	SyncedManga
	UserID      int64
	UserName    string
	Start, End  int64 // milliseconds
	First, Last int64 // chapter IDs
	FirstPct    float64
	LastPct     float64
	Saves       int
	FirstCh     *Chapter
	LastCh      *Chapter
}

func (r *ReadSession) Duration() time.Duration {
	return time.Duration(r.End-r.Start) * time.Millisecond
}

// What says what was read.
func (r *ReadSession) What() string {
	if r.FirstCh != nil && r.LastCh != nil {
		if r.LastPct >= finishedAt {
			return "Finished at " + chapterLabel(r.LastCh)
		}
		if r.First == r.Last {
			return "Read " + chapterLabel(r.LastCh)
		}
		// "Vol. 3 Ch. 11 to Ch. 14" when both are in the same volume.
		last := chapterLabel(r.LastCh)
		if r.FirstCh.Volume != "" && r.FirstCh.Volume == r.LastCh.Volume {
			last = strings.TrimPrefix(last, "Vol. "+r.LastCh.Volume+" ")
		}
		return fmt.Sprintf("Read %s to %s", chapterLabel(r.FirstCh), last)
	}
	if r.LastPct >= finishedAt {
		return "Finished"
	}
	a, b := int(r.FirstPct*100), int(r.LastPct*100)
	if a == b {
		return fmt.Sprintf("Read at %d%%", b)
	}
	return fmt.Sprintf("Read %d%% to %d%%", a, b)
}

// chapterIndex finds Kotodex chapters by the plugin's IDs, one manga at a
// time, for the length of a request.
type chapterIndex struct {
	db          *DB
	byManga     map[string]map[int64]*Chapter
	manga       map[string]*Manga
	otherCovers bool // the admin setting, read once per request
}

func newChapterIndex(db *DB) *chapterIndex {
	return &chapterIndex{db: db, byManga: map[string]map[int64]*Chapter{}, manga: map[string]*Manga{},
		otherCovers: db.settings().OtherCovers}
}

func (ci *chapterIndex) chapter(mangaID string, id int64) *Chapter {
	m, ok := ci.byManga[mangaID]
	if !ok {
		m = map[int64]*Chapter{}
		list, _ := ci.db.chapters(mangaID)
		for _, c := range list {
			m[stableID("chapter:"+c.ID)] = c
		}
		ci.byManga[mangaID] = m
	}
	return m[id]
}

func (ci *chapterIndex) local(mangaDexID string) *Manga {
	m, ok := ci.manga[mangaDexID]
	if !ok {
		m, _ = ci.db.manga(mangaDexID, 0)
		ci.manga[mangaDexID] = m
	}
	return m
}

// fill links a synced manga to Kotodex's copy, when it has one. Covers of
// manga from other sources show once they're downloaded, if an admin allowed
// that.
func (s *Server) fill(u *User, ci *chapterIndex, m *SyncedManga) {
	if !m.FromKotodex() {
		if ci.otherCovers && m.CoverURL != "" {
			if path := s.covers.cached(m.CoverURL); path != "" {
				m.Cover = s.fileURL(u, path)
			} else {
				s.covers.want(m.CoverURL)
			}
		}
		return
	}
	if l := ci.local(m.URL); l != nil {
		// Kotodex's title, so it reads the same as in the library.
		m.Local, m.Title = l, l.Title
		if l.Cover != "" {
			m.Cover = s.coverURL(u, l, thumbPath(l))
		}
	}
}

// PlaceQuery picks history rows. The viewer only ever gets what the
// owners share with them.
type PlaceQuery struct {
	Viewer    int64
	Owner     int64  // 0 for everyone
	MangaDex  string // only this Kotodex manga
	Finished  *bool  // only finished, or only unfinished
	Query     string // title contains
	Sort      string // "title", "progress", or last read
	Offset    int
	Limit     int
	CountOnly bool
}

func (db *DB) progress(q PlaceQuery) ([]*Place, int, error) {
	where := []string{`h.deleted_at=0`, shareFilter("h.user_id", "sm.source")}
	args := shareArgs(q.Viewer)
	if q.Owner != 0 {
		where = append(where, `h.user_id=@owner`)
		args = append(args, sql.Named("owner", q.Owner))
	}
	if q.MangaDex != "" {
		where = append(where, `sm.source=@kotodex AND sm.url=@mdx`)
		args = append(args, sql.Named("mdx", q.MangaDex))
	}
	if q.Finished != nil {
		if *q.Finished {
			where = append(where, `h.percent>=@done`)
		} else {
			where = append(where, `h.percent<@done`)
		}
		args = append(args, sql.Named("done", finishedAt))
	}
	if q.Query != "" {
		where = append(where, `sm.title LIKE @q ESCAPE '\'`)
		args = append(args, sql.Named("q", "%"+likeEscape(q.Query)+"%"))
	}
	from := ` FROM sync_history h JOIN users us ON us.id=h.user_id
		JOIN sync_manga sm ON sm.user_id=h.user_id AND sm.id=h.manga_id
		WHERE ` + strings.Join(where, " AND ")
	var total int
	if err := db.QueryRow(`SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if q.CountOnly {
		return nil, total, nil
	}
	order := `h.updated_at DESC`
	switch q.Sort {
	case "title":
		order = `sm.title COLLATE NOCASE`
	case "progress":
		order = `h.percent DESC, h.updated_at DESC`
	}
	limit := q.Limit
	if limit <= 0 {
		limit = -1
	}
	rows, err := db.Query(`SELECT h.user_id,us.name,h.manga_id,sm.title,sm.source,sm.url,sm.cover,h.chapter_id,h.page,h.percent,h.chapters,h.updated_at`+
		from+` ORDER BY `+order+` LIMIT @limit OFFSET @offset`, append(args, sql.Named("limit", limit), sql.Named("offset", q.Offset))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Place
	for rows.Next() {
		p := &Place{}
		if err := rows.Scan(&p.UserID, &p.UserName, &p.ID, &p.Title, &p.Source, &p.URL, &p.CoverURL, &p.ChapterID, &p.Page, &p.Percent, &p.Chapters, &p.Updated); err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// fillProgress adds chapter names and covers.
func (s *Server) fillProgress(u *User, ci *chapterIndex, list []*Place) {
	for _, p := range list {
		s.fill(u, ci, &p.SyncedManga)
		if p.Local != nil {
			p.Chapter = ci.chapter(p.Local.ID, p.ChapterID)
		}
	}
}

type readingEvent struct {
	UserID    int64
	UserName  string
	MangaID   int64
	ChapterID int64
	Percent   float64
	At        int64
	SyncedManga
}

// sessions groups reading events from since (milliseconds) into sessions,
// newest first. owner 0 means everyone the viewer may see. A session that
// started before since is cut at since.
func (db *DB) readSessions(viewer, owner int64, since int64, mangaDex string) ([]*ReadSession, error) {
	where := []string{`e.at>=@since`, shareFilter("e.user_id", "sm.source")}
	args := shareArgs(viewer, sql.Named("since", since))
	if owner != 0 {
		where = append(where, `e.user_id=@owner`)
		args = append(args, sql.Named("owner", owner))
	}
	if mangaDex != "" {
		where = append(where, `sm.source=@kotodex AND sm.url=@mdx`)
		args = append(args, sql.Named("mdx", mangaDex))
	}
	rows, err := db.Query(`SELECT e.user_id,us.name,e.manga_id,e.chapter_id,e.percent,e.at,sm.title,sm.source,sm.url,sm.cover
		FROM reading_events e JOIN users us ON us.id=e.user_id
		JOIN sync_manga sm ON sm.user_id=e.user_id AND sm.id=e.manga_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY e.user_id,e.manga_id,e.at`, args...)
	if err != nil {
		return nil, err
	}
	var events []readingEvent
	for rows.Next() {
		var e readingEvent
		if err := rows.Scan(&e.UserID, &e.UserName, &e.MangaID, &e.ChapterID, &e.Percent, &e.At, &e.Title, &e.Source, &e.URL, &e.CoverURL); err != nil {
			rows.Close()
			return nil, err
		}
		e.SyncedManga.ID = e.MangaID
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []*ReadSession
	var cur *ReadSession
	for _, e := range events {
		if cur == nil || cur.UserID != e.UserID || cur.SyncedManga.ID != e.MangaID || e.At-cur.End > sessionGap.Milliseconds() {
			cur = &ReadSession{SyncedManga: e.SyncedManga, UserID: e.UserID, UserName: e.UserName,
				Start: e.At, First: e.ChapterID, FirstPct: e.Percent}
			out = append(out, cur)
		}
		cur.End, cur.Last, cur.LastPct = e.At, e.ChapterID, e.Percent
		cur.Saves++
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].End > out[j].End })
	return out, nil
}

// readBefore reports whether owner has reading the viewer may see from
// before before (milliseconds).
func (db *DB) readBefore(viewer, owner, before int64) bool {
	var ok bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM reading_events e JOIN users us ON us.id=e.user_id
		JOIN sync_manga sm ON sm.user_id=e.user_id AND sm.id=e.manga_id
		WHERE e.user_id=@owner AND e.at<@before AND `+shareFilter("e.user_id", "sm.source")+`)`,
		shareArgs(viewer, sql.Named("owner", owner), sql.Named("before", before))...).Scan(&ok)
	return ok
}

func (s *Server) fillSessions(u *User, ci *chapterIndex, list []*ReadSession) {
	for _, r := range list {
		s.fill(u, ci, &r.SyncedManga)
		if r.Local != nil {
			r.FirstCh = ci.chapter(r.Local.ID, r.First)
			r.LastCh = ci.chapter(r.Local.ID, r.Last)
		}
	}
}

// chaptersRead counts the chapters each session moved through, at least one.
func chaptersRead(list []*ReadSession) int {
	seen := map[string]bool{}
	for _, r := range list {
		seen[fmt.Sprintf("%d:%d:%d", r.UserID, r.SyncedManga.ID, r.First)] = true
		seen[fmt.Sprintf("%d:%d:%d", r.UserID, r.SyncedManga.ID, r.Last)] = true
	}
	return len(seen)
}

func readingTime(list []*ReadSession) time.Duration {
	var d time.Duration
	for _, r := range list {
		d += r.Duration()
	}
	return d
}

// minutes formats reading time: "12 min", "2 h 05 min".
func minutes(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %02d min", m/60, m%60)
}

// Lists

type List struct {
	ID      int64
	Title   string
	Order   string
	Count   int
	Entries []*ListEntry
}

type ListEntry struct {
	SyncedManga
	Pinned bool
	Added  int64 // milliseconds
	Place  *Place
}

// lists returns owner's favorites categories with what viewer may see in
// them, in the order the owner set in the app. Categories with nothing to see
// are left out.
func (db *DB) lists(viewer, owner int64) ([]*List, error) {
	rows, err := db.Query(`SELECT c.id,c.title,c.sort_order,
		(SELECT count(*) FROM sync_favourites f JOIN users us ON us.id=f.user_id
			JOIN sync_manga sm ON sm.user_id=f.user_id AND sm.id=f.manga_id
			WHERE f.user_id=c.user_id AND f.category_id=c.id AND f.deleted_at=0 AND `+shareFilter("f.user_id", "sm.source")+`)
		FROM sync_categories c WHERE c.user_id=@owner AND c.deleted_at=0 ORDER BY c.sort_key,c.id`,
		shareArgs(viewer, sql.Named("owner", owner))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*List
	for rows.Next() {
		l := &List{}
		if err := rows.Scan(&l.ID, &l.Title, &l.Order, &l.Count); err != nil {
			return nil, err
		}
		if l.Count > 0 {
			out = append(out, l)
		}
	}
	return out, rows.Err()
}

// listOrder turns the app's sort order for a category into SQL.
func listOrder(order string) string {
	switch order {
	case "ALPHABETIC", "ALPHABETICAL":
		return `sm.title COLLATE NOCASE`
	case "ALPHABETIC_REVERSE", "ALPHABETICAL_REVERSE":
		return `sm.title COLLATE NOCASE DESC`
	case "OLDEST":
		return `f.created_at`
	case "LAST_READ", "UPDATED":
		return `coalesce(h.updated_at,0) DESC`
	case "PROGRESS":
		return `coalesce(h.percent,0) DESC`
	case "MANUAL":
		return `f.sort_key`
	}
	return `f.created_at DESC`
}

type ListQuery struct {
	Viewer, Owner, List int64
	Order               string // the app's order, used unless Sort is set
	Sort                string // "added", "title", or "" for the app's order
	Query               string
	Offset, Limit       int
}

func (db *DB) listEntries(q ListQuery) ([]*ListEntry, int, error) {
	where := []string{`f.user_id=@owner`, `f.category_id=@list`, `f.deleted_at=0`, shareFilter("f.user_id", "sm.source")}
	args := shareArgs(q.Viewer, sql.Named("owner", q.Owner), sql.Named("list", q.List))
	if q.Query != "" {
		where = append(where, `sm.title LIKE @q ESCAPE '\'`)
		args = append(args, sql.Named("q", "%"+likeEscape(q.Query)+"%"))
	}
	from := ` FROM sync_favourites f JOIN users us ON us.id=f.user_id
		JOIN sync_manga sm ON sm.user_id=f.user_id AND sm.id=f.manga_id
		LEFT JOIN sync_history h ON h.user_id=f.user_id AND h.manga_id=f.manga_id AND h.deleted_at=0
		WHERE ` + strings.Join(where, " AND ")
	var total int
	if err := db.QueryRow(`SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := listOrder(q.Order)
	switch q.Sort {
	case "added":
		order = `f.created_at DESC`
	case "title":
		order = `sm.title COLLATE NOCASE`
	}
	limit := q.Limit
	if limit <= 0 {
		limit = -1
	}
	rows, err := db.Query(`SELECT f.manga_id,sm.title,sm.source,sm.url,sm.cover,f.pinned,f.created_at,
		h.chapter_id,h.page,h.percent,h.chapters,h.updated_at`+from+` ORDER BY f.pinned DESC, `+order+` LIMIT @limit OFFSET @offset`,
		append(args, sql.Named("limit", limit), sql.Named("offset", q.Offset))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*ListEntry
	for rows.Next() {
		e := &ListEntry{}
		var ch, page, chapters, updated sql.NullInt64
		var pct sql.NullFloat64
		if err := rows.Scan(&e.ID, &e.Title, &e.Source, &e.URL, &e.CoverURL, &e.Pinned, &e.Added, &ch, &page, &pct, &chapters, &updated); err != nil {
			return nil, 0, err
		}
		if updated.Valid {
			e.Place = &Place{SyncedManga: e.SyncedManga, UserID: q.Owner, ChapterID: ch.Int64, Page: int(page.Int64),
				Percent: pct.Float64, Chapters: int(chapters.Int64), Updated: updated.Int64}
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func (s *Server) fillEntries(u *User, ci *chapterIndex, list []*ListEntry) {
	for _, e := range list {
		s.fill(u, ci, &e.SyncedManga)
		if e.Place != nil {
			e.Place.SyncedManga = e.SyncedManga
			if e.Local != nil {
				e.Place.Chapter = ci.chapter(e.Local.ID, e.Place.ChapterID)
			}
		}
	}
}

// Activity feed

type FeedItem struct {
	SyncedManga
	UserName string
	At       int64 // milliseconds
	What     string
	Time     time.Duration // reading time, for reading
	Pills    []string      // list names, for list changes
	Kind     string        // "read", "list", or "follow"

	session *ReadSession
}

type listEvent struct {
	SyncedManga
	UserID   int64
	UserName string
	Kind     string
	List     string
	At       int64
}

func (db *DB) listEvents(viewer, owner int64, since int64) ([]listEvent, error) {
	where := []string{`e.at>=@since`, shareFilter("e.user_id", "sm.source")}
	args := shareArgs(viewer, sql.Named("since", since))
	if owner != 0 {
		where = append(where, `e.user_id=@owner`)
		args = append(args, sql.Named("owner", owner))
	}
	rows, err := db.Query(`SELECT e.user_id,us.name,e.manga_id,sm.title,sm.source,sm.url,sm.cover,e.kind,coalesce(c.title,''),e.at
		FROM list_events e JOIN users us ON us.id=e.user_id
		JOIN sync_manga sm ON sm.user_id=e.user_id AND sm.id=e.manga_id
		LEFT JOIN sync_categories c ON c.user_id=e.user_id AND c.id=e.category_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY e.user_id,e.manga_id,e.at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []listEvent
	for rows.Next() {
		var e listEvent
		if err := rows.Scan(&e.UserID, &e.UserName, &e.SyncedManga.ID, &e.Title, &e.Source, &e.URL, &e.CoverURL, &e.Kind, &e.List, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// moveWindow is how close a removal and an addition of the same manga have to
// be to read as one move. The app does both in the same save.
const moveWindow = time.Minute

// listFeed turns list events into feed items, pairing removals with
// additions into moves.
func listFeed(events []listEvent) []*FeedItem {
	var out []*FeedItem
	used := make([]bool, len(events))
	for i, e := range events {
		if used[i] || e.Kind != "removed" {
			continue
		}
		for j, a := range events {
			if !used[j] && a.Kind == "added" && a.UserID == e.UserID && a.SyncedManga.ID == e.SyncedManga.ID &&
				abs(a.At-e.At) <= moveWindow.Milliseconds() {
				used[i], used[j] = true, true
				out = append(out, &FeedItem{SyncedManga: a.SyncedManga, UserName: a.UserName, At: max(a.At, e.At),
					What: "Moved from", Pills: []string{e.List, a.List}, Kind: "list"})
				break
			}
		}
	}
	for i, e := range events {
		if used[i] {
			continue
		}
		what := "Added to"
		if e.Kind == "removed" {
			what = "Removed from"
		}
		out = append(out, &FeedItem{SyncedManga: e.SyncedManga, UserName: e.UserName, At: e.At, What: what, Pills: []string{e.List}, Kind: "list"})
	}
	return out
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// follows returns recent follows on Kotodex. Everyone sees these.
func (db *DB) recentFollows(owner int64, since int64) ([]*FeedItem, error) {
	q := `SELECT us.name,m.id,m.title,f.created FROM follows f JOIN users us ON us.id=f.user_id JOIN manga m ON m.id=f.manga_id WHERE f.created>=?`
	args := []any{since / 1000}
	if owner != 0 {
		q += ` AND f.user_id=?`
		args = append(args, owner)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*FeedItem
	for rows.Next() {
		it := &FeedItem{Kind: "follow", What: "Followed"}
		var at int64
		if err := rows.Scan(&it.UserName, &it.URL, &it.Title, &at); err != nil {
			return nil, err
		}
		it.Source = kotodexSource
		it.At = at * 1000
		out = append(out, it)
	}
	return out, rows.Err()
}

// FeedDay is a day's worth of activity.
type FeedDay struct {
	Label string
	Items []*FeedItem
}

func dayLabel(t, now time.Time) string {
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return "Today"
	}
	y3, m3, d3 := now.AddDate(0, 0, -1).Date()
	if y1 == y3 && m1 == m3 && d1 == d3 {
		return "Yesterday"
	}
	if y1 == y2 {
		return t.Format("Monday, January 2")
	}
	return t.Format("Monday, January 2, 2006")
}

func groupDays(items []*FeedItem) []FeedDay {
	sort.SliceStable(items, func(i, j int) bool { return items[i].At > items[j].At })
	var out []FeedDay
	now := time.Now()
	for _, it := range items {
		label := dayLabel(time.UnixMilli(it.At), now)
		if len(out) == 0 || out[len(out)-1].Label != label {
			out = append(out, FeedDay{Label: label})
		}
		out[len(out)-1].Items = append(out[len(out)-1].Items, it)
	}
	return out
}

// MostRead is a manga ranked by chapters read across the server.
type MostRead struct {
	SyncedManga
	Chapters int
	Readers  []string
}

func mostRead(list []*ReadSession, n int) []*MostRead {
	by := map[string]*MostRead{}
	chapters := map[string]map[string]bool{}
	var order []string
	for _, r := range list {
		key := r.Source + "\x00" + r.URL
		m, ok := by[key]
		if !ok {
			m = &MostRead{SyncedManga: r.SyncedManga}
			by[key] = m
			chapters[key] = map[string]bool{}
			order = append(order, key)
		}
		chapters[key][fmt.Sprintf("%d:%d", r.UserID, r.First)] = true
		chapters[key][fmt.Sprintf("%d:%d", r.UserID, r.Last)] = true
		if !slices.Contains(m.Readers, r.UserName) {
			m.Readers = append(m.Readers, r.UserName)
		}
	}
	out := make([]*MostRead, 0, len(order))
	for _, k := range order {
		by[k].Chapters = len(chapters[k])
		slices.Sort(by[k].Readers)
		out = append(out, by[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Chapters > out[j].Chapters })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Heatmap

type HeatDay struct {
	X, Y    int
	Level   int
	Tip     string
	Hidden  bool
	Minutes int
}

type HeatMonth struct {
	X     int
	Label string
}

type Heatmap struct {
	Days   []HeatDay
	Months []HeatMonth
	Hours  int
	DaysOn int
	Width  int
	Height int
}

// heatmap lays out reading time per day for the past year, a week per
// column, as an SVG the page draws with classes.
func heatmap(list []*ReadSession, now time.Time) Heatmap {
	// A session with one save (read offline, or quickly between syncs) has
	// no length, but the day still had reading.
	perDay := map[string]time.Duration{}
	readOn := map[string]bool{}
	for _, r := range list {
		day := time.UnixMilli(r.Start).Format("2006-01-02")
		perDay[day] += r.Duration()
		readOn[day] = true
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := today.AddDate(0, 0, -int(today.Weekday())-52*7)
	const cell, gap, left, top = 11, 3, 30, 16
	h := Heatmap{Width: left + 53*(cell+gap), Height: top + 7*(cell+gap)}
	lastMonth := time.Month(0)
	var total time.Duration
	for w := range 53 {
		first := start.AddDate(0, 0, w*7)
		if first.Month() != lastMonth && first.Day() <= 7 {
			h.Months = append(h.Months, HeatMonth{X: left + w*(cell+gap), Label: first.Format("Jan")})
			lastMonth = first.Month()
		}
		for d := range 7 {
			day := first.AddDate(0, 0, d)
			hd := HeatDay{X: left + w*(cell+gap), Y: top + d*(cell+gap)}
			if day.After(today) {
				hd.Hidden = true
			} else {
				key := day.Format("2006-01-02")
				t := perDay[key]
				total += t
				hd.Minutes = int(t.Minutes())
				switch {
				case !readOn[key]:
					hd.Tip = day.Format("Jan 2") + ": no reading"
				case t < 20*time.Minute:
					hd.Level = 1
				case t < 45*time.Minute:
					hd.Level = 2
				case t < 90*time.Minute:
					hd.Level = 3
				default:
					hd.Level = 4
				}
				if readOn[key] {
					h.DaysOn++
					hd.Level = max(hd.Level, 1)
					hd.Tip = day.Format("Jan 2") + ": " + minutes(t)
					if t == 0 {
						hd.Tip = day.Format("Jan 2") + ": read"
					}
				}
			}
			h.Days = append(h.Days, hd)
		}
	}
	h.Hours = int(total.Hours())
	return h
}

// Manga page

type mangaListRow struct {
	UserID int64
	Title  string
}

// mangaLists returns which of everyone's lists a Kotodex manga is in, as far
// as viewer may see.
func (db *DB) mangaLists(viewer int64, mangaDex string) ([]mangaListRow, error) {
	rows, err := db.Query(`SELECT f.user_id,c.title FROM sync_favourites f JOIN users us ON us.id=f.user_id
		JOIN sync_manga sm ON sm.user_id=f.user_id AND sm.id=f.manga_id
		JOIN sync_categories c ON c.user_id=f.user_id AND c.id=f.category_id AND c.deleted_at=0
		WHERE f.deleted_at=0 AND sm.source=@kotodex AND sm.url=@mdx AND `+shareFilter("f.user_id", "sm.source")+`
		ORDER BY c.sort_key`, shareArgs(viewer, sql.Named("mdx", mangaDex))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mangaListRow
	for rows.Next() {
		var r mangaListRow
		if err := rows.Scan(&r.UserID, &r.Title); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// readingTimes adds up each person's reading time on a Kotodex manga, the
// same way sessions do: gaps between saves count unless they're long enough
// to start a new session. One query, since the manga page asks for it on
// every live update.
func (db *DB) readingTimes(viewer int64, mangaDex string) map[int64]time.Duration {
	out := map[int64]time.Duration{}
	rows, err := db.Query(`SELECT user_id, sum(CASE WHEN gap<=@limit THEN gap ELSE 0 END) FROM (
			SELECT e.user_id, e.at - lag(e.at) OVER (PARTITION BY e.user_id, e.manga_id ORDER BY e.at) AS gap
			FROM reading_events e JOIN users us ON us.id=e.user_id
			JOIN sync_manga sm ON sm.user_id=e.user_id AND sm.id=e.manga_id
			WHERE sm.source=@kotodex AND sm.url=@mdx AND `+shareFilter("e.user_id", "sm.source")+`
		) GROUP BY user_id`, shareArgs(viewer, sql.Named("mdx", mangaDex), sql.Named("limit", sessionGap.Milliseconds()))...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, ms int64
		if rows.Scan(&id, &ms) == nil {
			out[id] = time.Duration(ms) * time.Millisecond
		}
	}
	return out
}

// Reader is a row of the Readers table on a manga page.
type Reader struct {
	*Place
	Me    bool
	Lists []string
	Time  time.Duration
}

// ListCount is how many people have a manga in lists with this name.
type ListCount struct {
	Title string
	Count int
}

// MangaReading is the reading part of a manga page.
type MangaReading struct {
	Mine     *Place
	Readers  []Reader
	InLists  []ListCount
	Chapters map[string]string // chapter ID to "Read" or the page you're on
}

func (s *Server) mangaReading(u *User, m *Manga, all []*Chapter) MangaReading {
	var out MangaReading
	ci := newChapterIndex(s.db)
	places, _, _ := s.db.progress(PlaceQuery{Viewer: u.ID, MangaDex: m.ID, Sort: "progress"})
	s.fillProgress(u, ci, places)
	times := s.db.readingTimes(u.ID, m.ID)
	lists, _ := s.db.mangaLists(u.ID, m.ID)
	byUser := map[int64][]string{}
	counts := map[string]int{}
	var titles []string
	for _, l := range lists {
		byUser[l.UserID] = append(byUser[l.UserID], l.Title)
		if counts[l.Title] == 0 {
			titles = append(titles, l.Title)
		}
		counts[l.Title]++
	}
	for _, t := range titles {
		out.InLists = append(out.InLists, ListCount{t, counts[t]})
	}
	for _, p := range places {
		out.Readers = append(out.Readers, Reader{Place: p, Me: p.UserID == u.ID, Lists: byUser[p.UserID], Time: times[p.UserID]})
		if p.UserID == u.ID {
			out.Mine = p
		}
	}
	// Chapters before yours in reading order, in the same language, count
	// as read, as they do in the app.
	if out.Mine != nil && out.Mine.Chapter != nil {
		cur := out.Mine.Chapter
		out.Chapters = map[string]string{}
		for _, c := range all {
			if c.ID == cur.ID {
				total := len(c.Files)
				if total == 0 {
					total = c.Pages
				}
				if total > out.Mine.Page {
					out.Chapters[c.ID] = fmt.Sprintf("Page %d of %d", out.Mine.Page+1, total)
				} else {
					out.Chapters[c.ID] = fmt.Sprintf("Page %d", out.Mine.Page+1)
				}
				break
			}
			if c.Lang == cur.Lang {
				out.Chapters[c.ID] = "Read"
			}
		}
		if out.Mine.Finished() {
			for _, c := range all {
				if c.Lang == cur.Lang {
					out.Chapters[c.ID] = "Read"
				}
			}
		}
	}
	return out
}
