package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReaderPrimarySwitchKeepsIdentityAndPagesReference(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s)
	original, _ := s.db.chapter(chapter1)
	if original.ReaderID == 0 || original.ReaderID == stableID("chapter:"+chapter1) {
		t.Fatal("new chapter must have an allocated numeric identity")
	}
	alt := &Chapter{ID: "alternate", MangaID: mangaA, Number: "010.0", Volume: "2", Lang: "en", Source: "mangafire", State: stateDone}
	if _, err := s.db.insertChapter(alt); err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE chapters SET files='["other.jpg"]',downloaded=200 WHERE id=?`, alt.ID)
	alt, _ = s.db.chapter(alt.ID)
	if alt.ReaderID != original.ReaderID {
		t.Fatal("copies with and without a volume must share the allocated identity")
	}
	token, _ := s.db.createSession(uid, "kotatsu", "test")
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w
	}
	chapter := func() apiChapter {
		t.Helper()
		w := get("/api/v1/manga/" + mangaA)
		var response struct {
			ChapterList []apiChapter
			Chapters    int
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.ChapterList) != 1 {
			t.Fatalf("one primary chapter expected: %d %s (%v)", w.Code, w.Body, err)
		}
		if response.Chapters != 1 {
			t.Fatal("reader chapter count must exclude alternate copies")
		}
		return response.ChapterList[0]
	}
	before := chapter()
	if before.ID != original.ID {
		t.Fatal("downloading an alternate must not replace an available primary")
	}
	pagesRef := fmt.Sprintf("/api/v1/chapters/reader:%d/pages", before.ReaderID)
	if w := get(pagesRef); w.Code != 200 || !strings.Contains(w.Body.String(), chapter1+"/001.jpg") {
		t.Fatalf("original primary pages: %d %s", w.Code, w.Body)
	}
	webToken, _ := s.db.createSession(uid, "web", "test")
	req := httptest.NewRequest("POST", "/chapter/alternate/primary", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: webToken})
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("member selecting primary: %d %s", w.Code, w.Body)
	}
	after := chapter()
	if after.ReaderID != before.ReaderID || after.ID != alt.ID || after.Source != "mangafire" {
		t.Fatalf("primary changed identity or lost provenance: %+v -> %+v", before, after)
	}
	if w := get(pagesRef); w.Code != 200 || !strings.Contains(w.Body.String(), "alternate/other.jpg") {
		t.Fatalf("same reference must serve selected primary: %d %s", w.Code, w.Body)
	}
	if w := get("/api/v1/chapters/" + chapter1 + "/pages"); w.Code != 200 {
		t.Fatal("previous connector page references must continue working")
	}
	if c := newChapterIndex(s.db).chapter(mangaA, before.ReaderID); c == nil || c.ID != alt.ID {
		t.Fatal("synced numeric progress must resolve to the selected primary")
	}
	if err := s.db.setPrimaryChapter(chapter2); err == nil {
		t.Fatal("an undownloaded copy cannot become primary")
	}
	alt.Number, alt.Volume = "10.00", "7"
	if _, err := s.db.insertChapter(alt); err != nil {
		t.Fatal(err)
	}
	if got := chapter(); got.ReaderID != before.ReaderID || got.ID != alt.ID {
		t.Fatal("metadata refresh must retain both identity and selected primary")
	}
	if err := s.dl.deleteChapterFiles(alt); err != nil {
		t.Fatal(err)
	}
	if got := chapter(); got.ReaderID != before.ReaderID || got.ID != chapter1 {
		t.Fatal("deleting primary must fall back under the same identity")
	}
	if err := s.dl.deleteChapterFiles(original); err != nil {
		t.Fatal(err)
	}
	if c := newChapterIndex(s.db).chapter(mangaA, before.ReaderID); c == nil {
		t.Fatal("progress must still resolve while no copy is downloaded")
	}
	if w := get("/api/v1/chapters/reader:garbage/pages"); w.Code != 404 {
		t.Fatal("malformed reader references must be rejected")
	}
}

func TestReaderIdentityMigrationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.upsertManga(&Manga{ID: mangaA, Title: "Test"})
	for _, c := range []*Chapter{
		{ID: chapter1, MangaID: mangaA, Number: "1", Lang: "en", State: stateDone},
		{ID: "other", MangaID: mangaA, Number: "1", Volume: "2", Lang: "en", Source: "mangafire", Published: 100, State: stateDone},
	} {
		if _, err := db.insertChapter(c); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`DROP INDEX chapters_reader`,
		`ALTER TABLE chapters DROP COLUMN reader_id`,
		`DROP TABLE reader_chapters`,
		`DELETE FROM settings WHERE key='reader_chapters_version'`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for pass := 0; pass < 2; pass++ {
		db, err = openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		list, err := db.readerChapters(mangaA)
		if err != nil || len(list) != 1 || list[0].ReaderID != stableID("chapter:"+chapter1) {
			t.Fatalf("migration/restart must retain prior connector identity: %+v %v", list, err)
		}
		alt, _ := db.chapter("other")
		if alt.ReaderID != list[0].ReaderID {
			t.Fatal("migration must associate source copies despite missing volume")
		}
		if err := db.setPrimaryChapter("other"); err != nil {
			t.Fatal(err)
		}
		if pass == 1 && list[0].ID != "other" {
			t.Fatal("selected primary must persist over restart")
		}
		db.Close()
	}
}

func TestReaderIdentityMatchingAndConcurrency(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.db.insertChapter(&Chapter{ID: fmt.Sprintf("copy-%d", i), MangaID: mangaA, Number: "50", Lang: "en", Source: "mangafire"})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	var count int
	s.db.QueryRow(`SELECT count(DISTINCT reader_id) FROM chapters WHERE number='50'`).Scan(&count)
	if count != 1 {
		t.Fatalf("concurrent copies allocated %d identities", count)
	}
	for _, c := range []*Chapter{
		{ID: "vol1", Number: "1", Volume: "1", Lang: "en"},
		{ID: "vol2", Number: "1", Volume: "2", Lang: "en"},
		{ID: "unknown-volume", Number: "1", Lang: "en"},
		{ID: "spanish", Number: "50", Lang: "es"},
		{ID: "extra1", Lang: "en"},
		{ID: "extra2", Lang: "en"},
	} {
		c.MangaID = mangaA
		if _, err := s.db.insertChapter(c); err != nil {
			t.Fatal(err)
		}
	}
	s.db.QueryRow(`SELECT count(DISTINCT reader_id) FROM chapters WHERE id IN ('vol1','vol2','unknown-volume','spanish','extra1','extra2','copy-0')`).Scan(&count)
	if count != 7 {
		t.Fatal("ambiguous volumes, languages and unnumbered extras must stay distinct")
	}
}

func TestReaderIdentitySurvivesCopyDeletionAndInterruptedBinding(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	c := &Chapter{ID: "uploaded-copy", MangaID: mangaA, Number: "12", Lang: "en", Source: "upload", State: stateDone}
	if _, err := s.db.insertChapter(c); err != nil {
		t.Fatal(err)
	}
	c, _ = s.db.chapter(c.ID)
	id := c.ReaderID
	if err := s.dl.deleteChapterFiles(c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.chapter(c.ID); err == nil {
		t.Fatal("upload's source record should be removed")
	}
	c.ID = "reimported-copy"
	if _, err := s.db.insertChapter(c); err != nil {
		t.Fatal(err)
	}
	c, _ = s.db.chapter(c.ID)
	if c.ReaderID != id {
		t.Fatal("reimport after removing all copies must retain the reader identity")
	}
	// A crash may leave the identity allocated before the source row is bound.
	db := s.db
	var identities int
	db.QueryRow(`SELECT count(*) FROM reader_chapters`).Scan(&identities)
	db.Exec(`UPDATE chapters SET reader_id=NULL WHERE id=?`, chapter1)
	if err := db.bindReaderChapter(chapter1, false); err != nil {
		t.Fatal(err)
	}
	var after int
	db.QueryRow(`SELECT count(*) FROM reader_chapters`).Scan(&after)
	if after != identities {
		t.Fatal("resuming an interrupted binding allocated another identity")
	}
}
