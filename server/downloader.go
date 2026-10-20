package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Downloader struct {
	db      *DB
	mdx     *MangaDex
	sources map[string]ChapterSource
	keys    *Keys
	dir     string

	wake    chan struct{}
	check   chan string // manga ID, or "" for everything
	resched chan struct{}

	mu        sync.Mutex
	checking  string // what the chapter check is doing, "" when idle
	lastCheck time.Time
	nextCheck time.Time
	current   *Progress // chapter being downloaded, nil when idle

	changed    changes // what the live pages show
	progressed changes // pages of the chapter being downloaded
}

// Progress of the chapter being downloaded.
type Progress struct {
	ChapterID  string
	MangaID    string
	MangaTitle string
	Label      string
	Done       int
	Total      int
	Bytes      int64
}

// Status is a snapshot for the web UI.
type Status struct {
	Checking  string
	LastCheck time.Time
	NextCheck time.Time
	Current   *Progress
}

func newDownloader(db *DB, mdx *MangaDex, keys *Keys, dir string) *Downloader {
	return &Downloader{db: db, mdx: mdx, sources: map[string]ChapterSource{"mangadex": mangaDexSource{mdx}, "json": &jsonSource{client: coverClient()}, "mangafire": newMangaFireSource()}, keys: keys, dir: dir, wake: make(chan struct{}, 1), check: make(chan string, 64), resched: make(chan struct{}, 1)}
}

// Changed tells open pages to update.
func (d *Downloader) Changed() { d.changed.notify() }

func (d *Downloader) setChecking(s string) {
	d.mu.Lock()
	d.checking = s
	d.mu.Unlock()
	d.Changed()
}

func (d *Downloader) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := Status{Checking: d.checking, LastCheck: d.lastCheck, NextCheck: d.nextCheck}
	if d.current != nil {
		p := *d.current
		st.Current = &p
	}
	return st
}

func (d *Downloader) progress(f func(p *Progress)) {
	d.mu.Lock()
	if d.current != nil {
		f(d.current)
	}
	d.mu.Unlock()
	d.progressed.notify()
}

// Wake is called whenever the queue changed, so open pages update too.
func (d *Downloader) Wake() {
	d.Changed()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Reschedule moves the next check to the current interval after the last one.
func (d *Downloader) Reschedule() {
	select {
	case d.resched <- struct{}{}:
	default:
	}
}

// Check schedules a check of one manga, or of everything when id is "".
func (d *Downloader) Check(id string) {
	select {
	case d.check <- id:
	default:
	}
}

func (d *Downloader) Run(ctx context.Context) {
	go d.worker(ctx)
	timer := time.NewTimer(5 * time.Second)
	d.mu.Lock()
	d.nextCheck = time.Now().Add(5 * time.Second)
	d.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-d.check:
			if id == "" {
				d.checkAll(ctx)
			} else if m, err := d.db.manga(id, 0); err == nil {
				d.setChecking(m.Title)
				d.checkManga(ctx, m)
				d.setChecking("")
			}
		case <-d.resched:
			d.mu.Lock()
			last := d.lastCheck
			d.mu.Unlock()
			if last.IsZero() {
				continue // the first check is still coming up
			}
			next := last.Add(d.db.settings().Interval)
			timer.Reset(max(time.Until(next), 0))
			d.mu.Lock()
			d.nextCheck = next
			d.mu.Unlock()
			d.Changed()
		case <-timer.C:
			d.checkAll(ctx)
			interval := d.db.settings().Interval
			timer.Reset(interval)
			d.mu.Lock()
			d.nextCheck = time.Now().Add(interval)
			d.mu.Unlock()
			d.Changed()
		}
	}
}

func (d *Downloader) checkAll(ctx context.Context) {
	d.syncAccounts(ctx)
	list, err := d.db.followedManga()
	if err != nil {
		log.Printf("list followed manga: %v", err)
		return
	}
	for i, m := range list {
		if ctx.Err() != nil {
			return
		}
		d.setChecking(fmt.Sprintf("%s (%d of %d)", m.Title, i+1, len(list)))
		d.checkManga(ctx, m)
	}
	d.mu.Lock()
	d.lastCheck = time.Now()
	d.mu.Unlock()
	d.setChecking("")
}

// checkManga refreshes metadata once a day and looks for new chapters.
//
// Chapters in every language are recorded, so the manga page can offer them
// all. The first check records the whole back catalog as skipped. After
// that, a chapter that showed up since the previous check is queued when a
// follower wants its language, unless the same chapter number is already
// downloaded or queued in that language (another group's upload). Chapters
// that are new to the database but were published well before the last
// check are recorded as skipped too.
func (d *Downloader) checkManga(ctx context.Context, m *Manga) {
	// Alternate sources remain usable even if MangaDex is unavailable.
	var candidates []*Chapter
	defer func() {
		candidates = append(candidates, d.checkFeeds(ctx, m)...)
		queued, err := d.db.queueMissingChapters(candidates, nil)
		if err != nil {
			log.Printf("queue chapters for %s: %v", m.Title, err)
		}
		if queued > 0 {
			log.Printf("%s: %d new chapter(s)", m.Title, queued)
		}
		d.Wake()
	}()
	// Once a day, refresh the metadata and fetch the full chapter list, which
	// picks up older uploads the quick check (newest 100) can't see.
	daily := time.Since(time.Unix(m.Refreshed, 0)) > 24*time.Hour
	if daily {
		if fresh, err := d.mdx.manga(ctx, m.ID); err != nil {
			log.Printf("refresh %s: %v", m.Title, err)
		} else if err := d.db.upsertManga(fresh); err == nil {
			m = fresh
		}
	}
	if err := d.ensureCover(ctx, m); err != nil {
		log.Printf("cover for %s: %v", m.Title, err)
	}

	var checked int64
	d.db.QueryRow(`SELECT checked FROM manga WHERE id=?`, m.ID).Scan(&checked)
	initial := checked == 0
	feed, err := d.sources["mangadex"].Chapters(ctx, m.ID, initial || daily)
	if err != nil {
		log.Printf("check %s: %v", m.Title, err)
		return
	}
	wanted := d.db.wantedLangs(m.ID)

	// Record each source before choosing which new releases to queue.
	for i := len(feed) - 1; i >= 0; i-- {
		c := feed[i]
		c.State = stateSkipped
		eligible := !initial && wanted[c.Lang] && c.Published >= checked-24*3600
		inserted, err := d.db.insertChapter(c)
		if err != nil {
			log.Printf("save chapter %s: %v", c.ID, err)
			continue
		}
		if inserted && eligible {
			candidates = append(candidates, c)
		}
	}
	d.db.Exec(`UPDATE manga SET checked=? WHERE id=?`, now(), m.ID)
}

func coverPath(m *Manga) string {
	ext := strings.ToLower(path.Ext(m.Cover))
	if ext == "" {
		ext = ".jpg"
	}
	return m.ID + "/cover" + ext
}

func thumbPath(m *Manga) string { return m.ID + "/thumb.jpg" }

// ensureCover downloads the cover and its 512px thumbnail when missing or
// when MangaDex switched to another cover.
func (d *Downloader) ensureCover(ctx context.Context, m *Manga) error {
	if m.Cover == "" {
		return nil
	}
	full := filepath.Join(d.dir, coverPath(m))
	marker := filepath.Join(d.dir, m.ID, ".cover")
	if old, err := os.ReadFile(marker); err == nil && string(old) == m.Cover {
		if _, err := os.Stat(full); err == nil {
			return nil
		}
	}
	base := fmt.Sprintf("%s/covers/%s/%s", mdxUploads, m.ID, m.Cover)
	if err := d.download(ctx, base, full); err != nil {
		return err
	}
	if err := d.download(ctx, base+".512.jpg", filepath.Join(d.dir, thumbPath(m))); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte(m.Cover), 0o644)
}

func (d *Downloader) download(ctx context.Context, url, dest string) error {
	return d.downloadFrom(ctx, d.sources["mangadex"], url, dest)
}

func (d *Downloader) downloadFrom(ctx context.Context, source ChapterSource, url, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = source.Fetch(ctx, url, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func (d *Downloader) worker(ctx context.Context) {
	for {
		c, err := d.db.nextQueued()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
			case <-time.After(time.Minute):
			}
			continue
		}
		var title string
		d.db.QueryRow(`SELECT title FROM manga WHERE id=?`, c.MangaID).Scan(&title)
		d.mu.Lock()
		d.current = &Progress{ChapterID: c.ID, MangaID: c.MangaID, MangaTitle: title, Label: chapterLabel(c), Total: c.Pages}
		d.mu.Unlock()
		d.Changed()
		err = d.downloadChapter(ctx, c)
		d.mu.Lock()
		d.current = nil
		d.mu.Unlock()
		d.Changed()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			state := stateQueued
			if c.Tries+1 >= 3 {
				state = stateFailed
			}
			log.Printf("download %s %s: %v", title, chapterLabel(c), err)
			// Only if it's still queued. Canceling or deleting it meanwhile wins.
			d.db.Exec(`UPDATE chapters SET tries=tries+1, error=?, state=? WHERE id=? AND state=?`, err.Error(), state, c.ID, stateQueued)
			d.Changed()
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	}
}

func (d *Downloader) downloadChapter(ctx context.Context, c *Chapter) error {
	source := d.sources[c.Source]
	if c.Source == "" {
		source = d.sources["mangadex"]
	}
	if source == nil {
		return fmt.Errorf("unknown chapter source %q", c.Source)
	}
	files, err := source.Pages(ctx, c, d.db.settings().DataSaver)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("source returned no pages")
	}
	dir := filepath.Join(d.dir, c.MangaID, c.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := make([]string, len(files))
	for i, f := range files {
		ext := strings.ToLower(f.Extension)
		if ext != ".img" && !imageExtension(ext) {
			ext = ".jpg"
		}
		names[i] = fmt.Sprintf("%03d%s", i+1, ext)
	}

	d.progress(func(p *Progress) { p.Total = len(files) })

	// A few pages at a time. Pages already on disk from an earlier,
	// interrupted attempt are kept.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, 3)
	for i, f := range files {
		dest := filepath.Join(dir, names[i])
		if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
			d.progress(func(p *Progress) { p.Done++; p.Bytes += st.Size() })
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(url, dest string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := d.downloadFrom(ctx, source, url, dest); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			if st, err := os.Stat(dest); err == nil {
				d.progress(func(p *Progress) { p.Done++; p.Bytes += st.Size() })
			}
		}(f.URL, dest)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	// Deleting the chapter while it downloads can remove pages already done.
	var size int64
	for _, n := range names {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			return fmt.Errorf("page %s is missing", n)
		}
		size += st.Size()
	}
	filesJSON, _ := json.Marshal(names)
	res, err := d.db.Exec(`UPDATE chapters SET state=2, files=?, size=?, pages=?, downloaded=?, error='' WHERE id=? AND state=?`,
		string(filesJSON), size, len(names), now(), c.ID, stateQueued)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Canceled or deleted while downloading. Don't keep the pages.
		return os.RemoveAll(dir)
	}
	if err := d.db.repairChapterPrimaries(c.MangaID); err != nil {
		return err
	}
	_, err = d.db.Exec(`UPDATE manga SET latest=? WHERE id=?`, now(), c.MangaID)
	return err
}

// deleteChapterFiles removes a chapter's pages and marks it skipped.
func (d *Downloader) deleteChapterFiles(c *Chapter) error {
	if err := os.RemoveAll(filepath.Join(d.dir, c.MangaID, c.ID)); err != nil {
		return err
	}
	// Uploaded files have no remote copy to retry; remove their catalog row.
	if c.Source == "upload" {
		_, err := d.db.Exec(`DELETE FROM chapters WHERE id=? AND source='upload'`, c.ID)
		d.Changed()
		return err
	}
	_, err := d.db.Exec(`UPDATE chapters SET state=0, files='[]', size=0, downloaded=0, tries=0, error='' WHERE id=?`, c.ID)
	d.Changed()
	return err
}

// syncAccounts imports each linked MangaDex account's follows, which are
// the default for what that user follows here (see setFollow).
func (d *Downloader) syncAccounts(ctx context.Context) {
	rows, err := d.db.Query(`SELECT user_id FROM mdx_accounts`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		d.setChecking("Importing MangaDex follows")
		if err := d.syncAccount(ctx, id); err != nil {
			log.Printf("MangaDex follows for user %d: %v", id, err)
			d.db.Exec(`UPDATE mdx_accounts SET error=? WHERE user_id=?`, userMessage(err), id)
		}
	}
}

func (d *Downloader) syncAccount(ctx context.Context, userID int64) error {
	var clientID, sealedSecret, sealedRefresh string
	if err := d.db.QueryRow(`SELECT client_id,client_secret,refresh FROM mdx_accounts WHERE user_id=?`, userID).Scan(&clientID, &sealedSecret, &sealedRefresh); err != nil {
		return err
	}
	secret, err := d.keys.open(sealedSecret)
	if err != nil {
		return err
	}
	refresh, err := d.keys.open(sealedRefresh)
	if err != nil {
		return err
	}
	tok, err := d.mdx.refresh(ctx, refresh, clientID, secret)
	if err != nil {
		return err
	}
	if tok.Refresh != "" {
		d.db.Exec(`UPDATE mdx_accounts SET refresh=? WHERE user_id=?`, d.keys.seal(tok.Refresh), userID)
	}
	list, err := d.mdx.follows(ctx, tok.Access)
	if err != nil {
		return err
	}
	if err := d.db.setMangaDexFollows(userID, list); err != nil {
		return err
	}
	_, err = d.db.Exec(`UPDATE mdx_accounts SET synced=?, error='' WHERE user_id=?`, now(), userID)
	return err
}

func chapterLabel(c *Chapter) string {
	var parts []string
	if c.Volume != "" {
		parts = append(parts, "Vol. "+c.Volume)
	}
	if c.Number != "" {
		parts = append(parts, "Ch. "+c.Number)
	}
	if len(parts) == 0 {
		return "Oneshot"
	}
	return strings.Join(parts, " ")
}
