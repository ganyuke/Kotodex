package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sourceTestPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, c)
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sourceTestArchive(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, data := range entries {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCBZUploadAndRead(t *testing.T) {
	s, _ := testServer(t)
	member := seed(t, s)
	admin, err := s.db.createUser("admin", "pw", true)
	if err != nil {
		t.Fatal(err)
	}
	red := sourceTestPNG(t, color.RGBA{255, 0, 0, 255})
	blue := sourceTestPNG(t, color.RGBA{0, 0, 255, 255})
	archive := sourceTestArchive(t, map[string][]byte{"pages/page10.png": blue, "pages/page2.png": red, "ComicInfo.xml": []byte("metadata")})
	upload := func(uid int64, number string, alt bool) *httptest.ResponseRecorder {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		form.WriteField("number", number)
		form.WriteField("lang", "en")
		form.WriteField("source_url", "https://scanlator.example/chapter")
		if alt {
			form.WriteField("alternate", "1")
		}
		f, _ := form.CreateFormFile("archive", "chapter.cbz")
		f.Write(archive)
		form.Close()
		req := httptest.NewRequest("POST", "/manga/"+mangaA+"/upload", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		token, _ := s.db.createSession(uid, "web", "test")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w
	}
	if w := upload(member, "11", false); w.Code != 403 {
		t.Fatalf("member upload: %d", w.Code)
	}
	if w := upload(admin, "10.0", false); w.Code != 409 {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	if w := upload(admin, "11", false); w.Code != 303 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	chapters, err := s.db.readableChapters(mangaA)
	if err != nil || len(chapters) != 2 {
		t.Fatalf("merged chapters: %v %v", chapters, err)
	}
	c := chapters[1]
	if c.Source != "upload" || c.Number != "11" || c.Pages != 2 || c.SourceURL != "https://scanlator.example/chapter" {
		t.Fatalf("upload metadata: %+v", c)
	}
	if got, err := os.ReadFile(filepath.Join(s.mangaDir, mangaA, c.ID, c.Files[0])); err != nil || !bytes.Equal(got, red) {
		t.Fatal("page2 must precede page10 and keep original bytes")
	}
	if w := upload(admin, "11", false); w.Code != 409 {
		t.Fatalf("repeat upload: %d", w.Code)
	}
	if w := upload(admin, "10", true); w.Code != 303 {
		t.Fatalf("explicit alternate: %d", w.Code)
	}

	token, _ := s.db.createSession(member, "kotatsu", "reader")
	req := httptest.NewRequest("GET", "/api/v1/chapter/"+c.ID+"/pages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	// Use the same API handler the route registers, with its path value.
	req.SetPathValue("id", c.ID)
	w := httptest.NewRecorder()
	s.api(s.apiPages)(w, req)
	var pages []string
	if err := json.Unmarshal(w.Body.Bytes(), &pages); err != nil || len(pages) != 2 {
		t.Fatalf("reader pages: %s", w.Body)
	}
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("GET", pages[0], nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), red) {
		t.Fatalf("signed uploaded page: %d", w.Code)
	}
	if err := s.dl.deleteChapterFiles(c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.chapter(c.ID); err == nil {
		t.Fatal("deleted upload must not offer an impossible remote retry")
	}
	var files []string
	filepath.WalkDir(s.mangaDir, func(p string, d os.DirEntry, e error) error {
		if e == nil && strings.HasPrefix(d.Name(), ".upload-") {
			files = append(files, p)
		}
		return nil
	})
	if len(files) > 0 {
		t.Fatalf("staging directories remain: %v", files)
	}
}

func TestCBZRejectsUnsafeAndInvalidPages(t *testing.T) {
	png := sourceTestPNG(t, color.RGBA{255, 0, 0, 255})
	for _, tc := range []struct {
		name    string
		entries map[string][]byte
	}{
		{"traversal", map[string][]byte{"../page.png": png}},
		{"absolute", map[string][]byte{"/page.png": png}},
		{"backslash", map[string][]byte{"..\\page.png": png}},
		{"html", map[string][]byte{"page.jpg": []byte("<html>challenge</html>")}},
		{"empty", map[string][]byte{"ComicInfo.xml": []byte("metadata")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := sourceTestArchive(t, tc.entries)
			z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := extractChapter(z, t.TempDir()); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
	z := &zip.Reader{File: []*zip.File{{FileHeader: zip.FileHeader{Name: "page.png", UncompressedSize64: maxSourcePage + 1}}}}
	if _, _, err := extractChapter(z, t.TempDir()); err == nil {
		t.Fatal("oversized page accepted")
	}
}

type sourceTransport func(*http.Request) (*http.Response, error)

func (f sourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func sourceResponse(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}
}

type failedSource struct{}

func (failedSource) Chapters(context.Context, string, bool) ([]*Chapter, error) {
	return nil, errors.New("upstream unavailable")
}
func (failedSource) Pages(context.Context, *Chapter, bool) ([]SourcePage, error) {
	return nil, errors.New("upstream unavailable")
}
func (failedSource) Fetch(context.Context, string, io.Writer) (int64, error) {
	return 0, errors.New("upstream unavailable")
}

func TestFeedMergesAndDownloadsWithoutMangaDex(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s)
	png := sourceTestPNG(t, color.RGBA{0, 255, 0, 255})
	feed := []feedChapter{{ID: "a", Number: "010.00", Lang: "en", Pages: []string{"https://images.example/a"}}}
	client := &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("User-Agent") != "" {
			t.Error("source received user credentials or a custom user-agent")
		}
		if r.URL.Host == "images.example" {
			return sourceResponse(png), nil
		}
		data, _ := json.Marshal(map[string]any{"chapters": feed})
		return sourceResponse(data), nil
	})}
	s.dl.sources["json"] = &jsonSource{client: client}
	s.dl.sources["mangadex"] = failedSource{}
	_, err := s.db.Exec(`INSERT INTO chapter_feeds(manga_id,url) VALUES(?,?)`, mangaA, "https://adapter.example/feed")
	if err != nil {
		t.Fatal(err)
	}
	m := &Manga{ID: mangaA, Title: "Akanabe", Refreshed: now()}
	s.dl.checkManga(context.Background(), m)
	id := feedChapterID(mangaA, "json:https://adapter.example/feed", "a")
	c, err := s.db.chapter(id)
	if err != nil || c.State != stateSkipped {
		t.Fatalf("first check should list back catalog: %+v %v", c, err)
	}
	feed = append(feed, feedChapter{ID: "b", Number: "11", Lang: "en", Pages: []string{"https://images.example/b"}}, feedChapter{ID: "c", Number: "11", Lang: "fr", Pages: []string{"https://images.example/c"}})
	s.dl.checkManga(context.Background(), m)
	c, err = s.db.nextQueued()
	if err != nil || c.Number != "11" || c.Lang != "en" || c.Source != "json" {
		t.Fatalf("new wanted chapter: %+v %v", c, err)
	}
	if err := s.dl.downloadChapter(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	c, _ = s.db.chapter(c.ID)
	if c.State != stateDone || len(c.Files) != 1 || !strings.HasSuffix(c.Files[0], ".img") {
		t.Fatalf("downloaded chapter: %+v", c)
	}
	token, _ := s.db.createSession(uid, "kotatsu", "reader")
	u, _ := s.db.sessionUser(token)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("GET", s.fileURL(u, mangaA+"/"+c.ID+"/"+c.Files[0]), nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), png) {
		t.Fatalf("extensionless source page: %d", w.Code)
	}
	if !s.db.hasChapterNumber(mangaA, "en", "", "0011.0") || s.db.hasChapterNumber(mangaA, "fr", "", "11") {
		t.Fatal("incorrect numeric/language duplicate matching")
	}
	if _, err := s.db.nextQueued(); err == nil {
		t.Fatal("duplicate or unwanted language queued")
	}
	if id == feedChapterID(mangaA, "json:https://other.example/feed", "a") {
		t.Fatal("source identities collided")
	}
}

func TestFeedValidationAndPublicNetwork(t *testing.T) {
	for _, body := range []string{
		`{"chapters":[{"id":"a","lang":"en","pages":["file:///etc/passwd"]}]}`,
		`{"chapters":[{"id":"a","lang":"en","pages":["https://x.example/a"]},{"id":"a","lang":"en","pages":["https://x.example/b"]}]}`,
		`<html>challenge</html>`,
	} {
		src := &jsonSource{client: &http.Client{Transport: sourceTransport(func(*http.Request) (*http.Response, error) { return sourceResponse([]byte(body)), nil })}}
		if _, err := src.Chapters(context.Background(), "https://adapter.example/feed", true); err == nil {
			t.Fatal("invalid feed accepted")
		}
	}
	src := &jsonSource{client: coverClient()}
	if _, err := src.Chapters(context.Background(), "http://127.0.0.1/feed", true); err == nil {
		t.Fatal("connector reached a private address")
	}
	if _, err := src.Chapters(context.Background(), "https://user:secret@example.com/feed", true); err == nil {
		t.Fatal("credentials in URL accepted")
	}
	if _, err := copySourceImage(strings.NewReader("<html>challenge</html>"), io.Discard); err == nil {
		t.Fatal("HTML saved as page")
	}
}

func TestMangaFireUpstreamSigningAndPagination(t *testing.T) {
	q := url.Values{"sort": {"number"}, "order": {"desc"}, "page": {"1"}, "limit": {"200"}}
	canonical := mangaFireCanonical("/api/titles/72xyj/chapters", q)
	if canonical != "/titles/72xyj/chapters?limit=200&order=desc&page=1&sort=number" {
		t.Fatal(canonical)
	}
	// Checked against the pinned upstream routine and a successful public API response.
	if got := mangaFireVRF(canonical); got != "8sK3xtqdFdvqEMVoy7tpX-p4tWCPb69P1VQNYfBmQhgU030lqxEiWlqCcP6J6wSUCxQxf8lhf8Y_TilEO5A" {
		t.Fatalf("signature: %s", got)
	}
	if got := mangaFireCanonical("/api/titles", url.Values{"languages[]": {"en", "es-la"}, "keyword": {"a b"}}); got != "/titles?keyword=a b&languages[0]=en&languages[1]=es-la" {
		t.Fatal(got)
	}
	for _, stage := range mangaFireVRFStages {
		if stage.table == "" || stage.key == "" {
			t.Fatal("missing upstream constants")
		}
	}
	calls := 0
	src := newMangaFireSource()
	src.lim.gap = 0
	src.client = &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Query().Get("vrf") == "" || r.Header.Get("Referer") != mangaFireOrigin+"/" {
			t.Error("missing upstream request signing")
		}
		if r.Header.Get("User-Agent") != "" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected custom user-agent or cookies")
		}
		switch r.URL.Path {
		case "/api/titles/72xyj/chapters":
			if r.URL.Query().Get("page") == "1" {
				return sourceResponse([]byte(`{"items":[{"id":5037948,"number":90,"name":"Always, on the Veranda","language":"en","createdAt":1758933029}],"meta":{"lastPage":2}}`)), nil
			}
			return sourceResponse([]byte(`{"items":[{"id":6345098,"number":90.5,"name":"French chapter","language":"fr","createdAt":1767303629}],"meta":{"lastPage":2}}`)), nil
		case "/api/chapters/5037948":
			return sourceResponse([]byte(`{"data":{"pages":[{"url":"https://images.example/one.png"},{"url":"https://images.example/two"}]}}`)), nil
		}
		return nil, errors.New("unexpected endpoint")
	})}
	chapters, err := src.Chapters(context.Background(), mangaFireOrigin+"/title/72xyj-kyou-mo-veranda-dee", true)
	if err != nil || len(chapters) != 2 || calls != 2 || chapters[1].Number != "90.5" {
		t.Fatalf("chapters: %+v %v calls=%d", chapters, err, calls)
	}
	pages, err := src.Pages(context.Background(), chapters[0], false)
	if err != nil || len(pages) != 2 || pages[1].Extension != ".img" {
		t.Fatalf("pages: %v %v", pages, err)
	}
	for _, raw := range []string{"https://evil.example/title/72xyj", "https://mangafire.to.evil.example/title/72xyj", "https://mangafire.to/title/72xyj/chapter/1"} {
		if _, err := mangaFireTitle(raw); err == nil {
			t.Fatalf("invalid source accepted: %s", raw)
		}
	}
}

// Explicit opt-in: ordinary tests never request manga from the internet.
func TestMangaFireLive(t *testing.T) {
	if os.Getenv("KOTODEX_TEST_MANGAFIRE") != "1" {
		t.Skip("set KOTODEX_TEST_MANGAFIRE=1 to verify the public example")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	src := newMangaFireSource()
	chapters, err := src.Chapters(ctx, mangaFireOrigin+"/title/72xyj-kyou-mo-veranda-dee", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chapters {
		if c.Lang == "en" {
			pages, err := src.Pages(ctx, c, false)
			if err != nil || len(pages) == 0 {
				t.Fatalf("pages: %v", err)
			}
			n, err := src.Fetch(ctx, pages[0].URL, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d chapters listed; English chapter %s has %d pages; first page validated (%d bytes)", len(chapters), c.Number, len(pages), n)
			return
		}
	}
	t.Fatal("example has no English chapters")
}

func TestSourceAdministrationAndBulkSelection(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s)
	admin, _ := s.db.createUser("admin", "pw", true)
	post := func(user int64, endpoint string, form url.Values) *httptest.ResponseRecorder {
		token, _ := s.db.createSession(user, "web", "test")
		req := httptest.NewRequest("POST", endpoint, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w
	}
	endpoint := "/manga/" + mangaA + "/feeds"
	form := url.Values{"source": {"mangafire"}, "url": {mangaFireOrigin + "/title/72xyj-kyou-mo-veranda-dee"}}
	if w := post(uid, endpoint, form); w.Code != 303 {
		t.Fatalf("member could not link source: %d", w.Code)
	}
	if w := post(admin, endpoint, form); w.Code != 303 {
		t.Fatalf("admin link: %d %s", w.Code, w.Body.String())
	}
	feeds, err := s.db.chapterFeeds(mangaA)
	if err != nil || len(feeds) != 1 || feeds[0].Source != "mangafire" || feeds[0].URL != mangaFireOrigin+"/title/72xyj" {
		t.Fatalf("canonical source: %+v %v", feeds, err)
	}
	form.Set("url", mangaFireOrigin+"/title/72xyj-another-slug")
	post(admin, endpoint, form)
	feeds, _ = s.db.chapterFeeds(mangaA)
	if len(feeds) != 1 {
		t.Fatal("slug change created duplicate source")
	}
	for _, c := range []*Chapter{
		{ID: "38ccd96c-23de-4ba8-a6dc-fed34697d2ab", Source: "mangafire", SourceRef: "1", MangaID: mangaA, Number: "10", Lang: "en"},
		{ID: "48ccd96c-23de-4ba8-a6dc-fed34697d2ab", Source: "mangafire", SourceRef: "2", MangaID: mangaA, Number: "9.5", Lang: "en"},
		{ID: "58ccd96c-23de-4ba8-a6dc-fed34697d2ab", Source: "mangafire", SourceRef: "3", MangaID: mangaA, Number: "12", Lang: "en"},
	} {
		if _, err := s.db.insertChapter(c); err != nil {
			t.Fatal(err)
		}
	}
	if w := post(uid, "/manga/"+mangaA+"/download-all", url.Values{"source": {"mangafire"}, "lang": {"en"}}); w.Code != 303 {
		t.Fatalf("source selection: %d", w.Code)
	}
	queue, _ := s.db.queue()
	if len(queue) != 2 {
		t.Fatalf("expected missing MangaFire chapters only: %+v", queue)
	}
	for _, c := range queue {
		if c.Source != "mangafire" || c.Number == "10" {
			t.Fatalf("wrong source or duplicate queued: %+v", c)
		}
	}
	for _, user := range []int64{uid, admin} {
		token, _ := s.db.createSession(user, "web", "test")
		req := httptest.NewRequest("GET", "/manga/"+mangaA, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `aria-label="Source priority"`) {
			t.Fatalf("source page rendering: %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "Connect source") {
			t.Fatal("source linking unavailable to reader")
		}
		if strings.Contains(w.Body.String(), "Upload chapter files") != (user == admin) {
			t.Fatal("upload permission changed")
		}
	}
	form.Set("remove", "1")
	if w := post(uid, endpoint, form); w.Code != 403 {
		t.Fatal("reader disconnected shared source")
	}
	post(admin, endpoint, form)
	feeds, _ = s.db.chapterFeeds(mangaA)
	if len(feeds) != 0 {
		t.Fatal("source not disconnected")
	}
	if _, err := s.db.chapter("48ccd96c-23de-4ba8-a6dc-fed34697d2ab"); err != nil {
		t.Fatal("disconnect removed chapter record")
	}
}

func TestSourceMigrationPreservesExistingChapters(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertManga(&Manga{ID: mangaA, Title: "Existing manga"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.insertChapter(&Chapter{ID: chapter1, MangaID: mangaA, Lang: "en", Number: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE chapters SET state=2,files='["001.jpg"]',downloaded=123 WHERE id=?`, chapter1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE chapters SET number='Ch. 001.50',volume='Vol. 002.0' WHERE id=?`, chapter1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX chapters_identity`); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"source", "source_ref", "source_url", "remote_pages", "number_key", "volume_key"} {
		if _, err := db.Exec("ALTER TABLE chapters DROP COLUMN " + column); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := db.chapter(chapter1)
	if err != nil || c.ID != chapter1 || c.Source != "mangadex" || c.State != stateDone || c.Downloaded != 123 || len(c.Files) != 1 {
		t.Fatalf("migration changed existing chapter: %+v %v", c, err)
	}
	if c.Number != "1.5" || c.Volume != "2" || !db.hasChapterNumber(mangaA, "en", "2", "1.50") {
		t.Fatalf("migration did not normalize existing identity: %+v", c)
	}
}

func TestDisconnectedSourceCannotInsertDuringCheck(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	c := &Chapter{ID: "68ccd96c-23de-4ba8-a6dc-fed34697d2ab", MangaID: mangaA, Source: "json", FeedURL: "https://adapter.example/feed", Lang: "en", Number: "12", State: stateQueued}
	inserted, err := s.db.insertChapter(c)
	if err != nil || inserted {
		t.Fatalf("disconnected source inserted chapter: %v %v", inserted, err)
	}
	if _, err := s.db.chapter(c.ID); err == nil {
		t.Fatal("disconnected source's chapter was stored")
	}
}
