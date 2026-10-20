package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Names as MangaFire sends them.
func TestMangaFireName(t *testing.T) {
	for _, tc := range []struct {
		name            string
		official        bool
		title, uploader string
	}{
		{"(nao)", true, "", "nao"},
		{"Chapter 98 - (nao)", true, "", "nao"},
		{"Chapter 130.5 - Volume 13 (ShelfLife)", true, "Volume 13", "ShelfLife"},
		{"Epilogue (Rillant)", true, "Epilogue", "Rillant"},
		{"CHAPTER 314 CORE (PART 1)", true, "CORE (PART 1)", ""},
		{"Wizard and Blue Rose (Pt. 1)", true, "Wizard and Blue Rose (Pt. 1)", ""},
		{"Unji Zuma (3)", true, "Unji Zuma (3)", ""},
		{"Batalla siniestra en Shinjuku (14)", false, "Batalla siniestra en Shinjuku (14)", ""},
		{"L'audience à Irishima (Fin)", false, "L'audience à Irishima (Fin)", ""},
		{"Yellow Diamond (original version of translation)", false, "Yellow Diamond (original version of translation)", ""},
		{"Chapter 12: Yellow Diamond", false, "Yellow Diamond", ""},
		{"Chapters of Life", false, "Chapters of Life", ""},
	} {
		title, uploader := mangaFireName(tc.name, tc.official)
		if title != tc.title || uploader != tc.uploader {
			t.Errorf("%q: got %q %q, want %q %q", tc.name, title, uploader, tc.title, tc.uploader)
		}
	}
}

func TestOfficialChapters(t *testing.T) {
	s, ts := testServer(t)
	seed(t, s)
	admin, _ := s.db.createUser("admin", "pw", true)
	s.db.Exec(`INSERT INTO chapter_feeds(manga_id,url,source) VALUES(?,?,?)`, mangaA, "https://mangafire.to/title/x", "mangafire")
	fan := &Chapter{ID: "fan", MangaID: mangaA, Number: "11", Title: "Prayer", Lang: "en", Group: "Fans", Published: 300, Source: "mangafire", SourceRef: "1", FeedURL: "https://mangafire.to/title/x"}
	official := &Chapter{ID: "official", MangaID: mangaA, Number: "11", Lang: "en", Group: "nao", Official: true, Published: 400, Source: "mangafire", SourceRef: "2", FeedURL: "https://mangafire.to/title/x"}
	for _, c := range []*Chapter{fan, official} {
		if _, err := s.db.insertChapter(c); err != nil {
			t.Fatal(err)
		}
	}

	// Listed first of the two, with the other copy's title, and marked.
	w := adminRequest(t, s, admin, "GET", "/manga/"+mangaA+"?sources=1&source=mangafire", nil, "")
	body := w.Body.String()
	if a, b := strings.Index(body, `id="c-official"`), strings.Index(body, `id="c-fan"`); a < 0 || b < 0 || a > b {
		t.Fatalf("official copy not listed first: %d %d", a, b)
	}
	row := body[strings.Index(body, `id="c-official"`):strings.Index(body, `id="c-fan"`)]
	if !strings.Contains(row, `<td title="Prayer">Prayer</td>`) || !strings.Contains(row, `<span class="pill">Official</span>`) || !strings.Contains(row, `<td title="nao">nao</td>`) {
		t.Fatalf("official row: %s", row)
	}
	if strings.Contains(body[strings.Index(body, `id="c-fan"`):], `<span class="pill">Official</span></td>`) {
		t.Fatal("fan copy marked official")
	}

	// Kotatsu shows only the scanlator line.
	s.db.Exec(`UPDATE chapters SET state=2,files='["001.jpg"]' WHERE id='official'`)
	s.db.repairChapterPrimaries(mangaA)
	res, _ := http.Post(ts.URL+"/api/v1/login", "application/json", strings.NewReader(`{"username":"mio","password":"pw"}`))
	var login struct{ Token string }
	json.NewDecoder(res.Body).Decode(&login)
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/manga/"+mangaA, nil)
	req.Header.Set("Authorization", "Bearer "+login.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ChapterList []apiChapter `json:"chapterList"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	var got *apiChapter
	for i := range out.ChapterList {
		if out.ChapterList[i].ID == "official" {
			got = &out.ChapterList[i]
		}
	}
	if got == nil || got.Group != "Official (nao)" || got.Title != "Prayer" {
		t.Fatalf("kotatsu chapter: %+v", got)
	}
	if scanlator(&Chapter{Official: true}) != "Official" || scanlator(&Chapter{Group: "Fans"}) != "Fans" {
		t.Fatal("scanlator line")
	}

	// The edit page shows the chapter's own title, not the borrowed one.
	w = adminRequest(t, s, admin, "GET", "/chapter/official", nil, "")
	if !strings.Contains(w.Body.String(), `name="title" value=""`) {
		t.Fatal("edit page borrowed a title")
	}
}

// Before the official column, official MangaFire chapters had the group
// "Official".
func TestOfficialGroupMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.upsertManga(&Manga{ID: mangaA, Title: "A"})
	db.Exec(`INSERT INTO chapters(id,manga_id,number,lang,grp,source) VALUES('a',?,'1','en','Official','mangafire'),('b',?,'1','en','Official','mangadex')`, mangaA, mangaA)
	db.Exec(`ALTER TABLE chapters DROP COLUMN official`)
	db.Close()
	db, err = openDB(filepath.Join(dir, "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, _ := db.chapter("a")
	b, _ := db.chapter("b")
	if !a.Official || a.Group != "" || b.Official || b.Group != "Official" {
		t.Fatalf("migrated: %+v %+v", a, b)
	}
}

func TestPlaceWhere(t *testing.T) {
	for _, tc := range []struct {
		percent  float64
		chapters int
		want     string
	}{
		{0.04, 43, "Chapter 2 of 43"},
		{0, 43, "Chapter 1 of 43"},
		{0.98, 43, "Chapter 43 of 43"},
		{0.04, 0, "4% read"},
	} {
		p := &Place{Percent: tc.percent, Chapters: tc.chapters}
		if got := p.Where(); got != tc.want {
			t.Errorf("%v of %d: %q, want %q", tc.percent, tc.chapters, got, tc.want)
		}
	}
}

// Values computed outside Go, with Kotlin's 64-bit overflow.
func TestKotatsuUID(t *testing.T) {
	if got := kotatsuUID("MANGADEX", mangaA); got != 6946276950196467221 {
		t.Fatalf("manga: %d", got)
	}
	if got := kotatsuUID("MANGADEX", chapter1); got != -3557813573409852259 {
		t.Fatalf("chapter: %d", got)
	}
}

func TestReadOnKotatsuMangaDex(t *testing.T) {
	s, ts := testServer(t)
	seed(t, s)
	token := syncToken(t, ts)
	row := func(mangaID int64) string {
		manga := strings.ReplaceAll(syncManga(mangaID, mangaA), kotodexSource, "MANGADEX")
		return fmt.Sprintf(`{"manga_id":%d,"created_at":1000,"updated_at":%d,"chapter_id":%d,"page":4,"scroll":0.0,"percent":0.25,"deleted_at":0,"chapters":12,"manga":%s}`,
			mangaID, time.Now().UnixMilli(), kotatsuUID("MANGADEX", chapter1), manga)
	}
	// The second manga's ID was made some other way, so its chapter is unknown.
	if code, b := syncPost(t, ts, token, "/resource/history", `{"history":[`+row(kotatsuUID("MANGADEX", mangaA))+`,`+row(42)+`],"timestamp":1}`); code >= 300 {
		t.Fatalf("sync: %d %s", code, b)
	}
	uid := int64(1)
	w := adminRequest(t, s, uid, "GET", "/people/mio/reading", nil, "")
	body := w.Body.String()
	if !strings.Contains(body, "Ch. 10, page 5") || !strings.Contains(body, `href="/manga/`+mangaA+`"`) || !strings.Contains(body, "MangaDex") {
		t.Fatalf("read on MangaDex: %s", body)
	}
	if strings.Count(body, "Ch. 10, page 5") != 1 || !strings.Contains(body, "Chapter 4 of 12") {
		t.Fatalf("unmatched manga: %s", body)
	}
}

func TestOfficialOnlyFilter(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s)
	s.db.Exec(`INSERT INTO chapter_feeds(manga_id,url,source) VALUES(?,?,?)`, mangaA, "https://mangafire.to/title/x", "mangafire")
	for _, c := range []*Chapter{
		{ID: "fan", MangaID: mangaA, Number: "11", Title: "Fan copy", Lang: "en", Source: "mangafire", SourceRef: "1", FeedURL: "https://mangafire.to/title/x"},
		{ID: "official", MangaID: mangaA, Number: "11", Title: "Official copy", Lang: "en", Official: true, Source: "mangafire", SourceRef: "2", FeedURL: "https://mangafire.to/title/x"},
	} {
		if _, err := s.db.insertChapter(c); err != nil {
			t.Fatal(err)
		}
	}
	page := func(query string) string {
		return adminRequest(t, s, uid, "GET", "/manga/"+mangaA+query, nil, "").Body.String()
	}
	if body := page("?sources=1&source=mangadex"); strings.Contains(body, "Only official chapters") {
		t.Fatal("filter offered without official chapters")
	}
	body := page("?sources=1&source=mangafire&source=mangadex&official=1")
	if !strings.Contains(body, "Official copy") || strings.Contains(body, "Fan copy") || strings.Contains(body, `id="c-`+chapter1+`"`) {
		t.Fatal("official filter shows other chapters")
	}
	if !strings.Contains(body, "Download all official English chapters") {
		t.Fatal("download button doesn't say official")
	}
	if !strings.Contains(body, `name="official" value="1" checked`) || strings.Count(body, `<input type="hidden" name="official" value="1">`) != 3 {
		t.Fatal("other filter forms drop the official filter")
	}
	// Remembered, like the other filters.
	if body := page(""); !strings.Contains(body, "Official copy") || strings.Contains(body, "Fan copy") {
		t.Fatal("official filter not remembered")
	}
	// Download all takes what the table shows.
	form := url.Values{"sources": {"1"}, "source": {"mangafire"}, "lang": {"en"}, "official": {"1"}}
	adminRequest(t, s, uid, "POST", "/manga/"+mangaA+"/download-all", bytes.NewBufferString(form.Encode()), "application/x-www-form-urlencoded")
	fan, _ := s.db.chapter("fan")
	official, _ := s.db.chapter("official")
	if official.State != stateQueued || fan.State == stateQueued {
		t.Fatalf("download all: official %d, fan %d", official.State, fan.State)
	}
}
