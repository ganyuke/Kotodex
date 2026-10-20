package main

import (
	"bytes"
	"context"
	"image/color"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const takenDown = "48ccd96c-23de-4ba8-a6dc-fed34697d2ab"

func TestMangaDexListsUnavailableChapters(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"11111111-1111-1111-1111-111111111111","attributes":{"chapter":"1","title":"Here","translatedLanguage":"en","pages":20}},
		{"id":"22222222-2222-2222-2222-222222222222","attributes":{"chapter":"2","title":"Gone","translatedLanguage":"en","pages":22,"isUnavailable":true}},
		{"id":"33333333-3333-3333-3333-333333333333","attributes":{"chapter":"3","translatedLanguage":"en","pages":0,"externalUrl":"https://pub.example/ch"}}
	],"total":3}`)
	mdx := newMangaDex("test")
	mdx.api.gap = 0
	var queries []url.Values
	mdx.http = &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		queries = append(queries, r.URL.Query())
		return sourceResponse(body), nil
	})}
	for _, full := range []bool{true, false} {
		chapters, err := (mangaDexSource{mdx}).Chapters(context.Background(), mangaA, full)
		if err != nil || len(chapters) != 2 {
			t.Fatalf("listed: %+v %v", chapters, err)
		}
		if c := chapters[0]; c.Number != "1" || c.Pages != 20 || c.Unavailable {
			t.Fatalf("hosted: %+v", c)
		}
		if c := chapters[1]; c.Number != "2" || c.Title != "Gone" || c.Pages != 22 || !c.Unavailable || c.Source != "mangadex" {
			t.Fatalf("unavailable: %+v", c)
		}
	}
	// "includeEmptyPages=1" means only chapters without pages, which would
	// leave out everything else.
	for _, q := range queries {
		if q.Get("includeUnavailable") != "1" || q.Get("includeEmptyPages") != "0" {
			t.Fatalf("feed query: %v", q)
		}
	}
}

func adminRequest(t *testing.T, s *Server, uid int64, method, target string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, body)
		req.Header.Set("Content-Type", contentType)
	}
	token, _ := s.db.createSession(uid, "web", "test")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}

func seedTakenDown(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.db.insertChapter(&Chapter{ID: takenDown, MangaID: mangaA, Number: "4", Volume: "2", Title: "Arrival", Lang: "en", Group: "Group", Pages: 22, Unavailable: true}); err != nil {
		t.Fatal(err)
	}
}

func TestUnavailableChapterPages(t *testing.T) {
	s, _ := testServer(t)
	member := seed(t, s)
	admin, err := s.db.createUser("admin", "pw", true)
	if err != nil {
		t.Fatal(err)
	}
	seedTakenDown(t, s)

	// Hidden until asked for, and not counted as a chapter to download.
	w := adminRequest(t, s, admin, "GET", "/manga/"+mangaA, nil, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "c-"+takenDown) || !strings.Contains(w.Body.String(), "Show 1 unavailable chapter") || !strings.Contains(w.Body.String(), "1 of 2 chapters") {
		t.Fatalf("hidden by default: %d %s", w.Code, w.Body)
	}
	w = adminRequest(t, s, admin, "GET", "/manga/"+mangaA+"?unavailable=1", nil, "")
	if !strings.Contains(w.Body.String(), `class="state0 unavailable"`) || !strings.Contains(w.Body.String(), `href="/chapter/`+takenDown+`?next=%2fmanga%2f`+mangaA+`%3funavailable%3d1">Add pages`) {
		t.Fatalf("shown on request: %s", w.Body)
	}
	if w := adminRequest(t, s, member, "GET", "/manga/"+mangaA+"?unavailable=1", nil, ""); strings.Contains(w.Body.String(), "Add pages") || strings.Contains(w.Body.String(), ">Edit<") {
		t.Fatal("members see admin actions")
	}

	// Nothing downloads it.
	if n, err := s.db.queueMissingChapters([]*Chapter{{ID: takenDown, MangaID: mangaA}}, nil); err != nil || n != 0 {
		t.Fatalf("queued for download: %d %v", n, err)
	}
	adminRequest(t, s, member, "POST", "/chapter/"+takenDown+"/download", nil, "")
	if c, _ := s.db.chapter(takenDown); c.State != stateSkipped {
		t.Fatalf("downloaded on request: %d", c.State)
	}

	w = adminRequest(t, s, admin, "GET", "/chapter/"+takenDown+"?next=/manga/"+mangaA+"%3Funavailable%3D1", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `action="/chapter/`+takenDown+`/pages"`) || !strings.Contains(w.Body.String(), `value="/manga/`+mangaA+`?unavailable=1"`) {
		t.Fatalf("chapter page: %d %s", w.Code, w.Body)
	}
	if w := adminRequest(t, s, member, "GET", "/chapter/"+takenDown, nil, ""); w.Code != 403 {
		t.Fatalf("member chapter page: %d", w.Code)
	}

	red := sourceTestPNG(t, color.RGBA{255, 0, 0, 255})
	blue := sourceTestPNG(t, color.RGBA{0, 0, 255, 255})
	post := func(uid int64, script bool, files ...[2]string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		form.WriteField("next", "/manga/"+mangaA+"?unavailable=1")
		for _, f := range files {
			part, _ := form.CreateFormFile("pages", f[0])
			part.Write([]byte(f[1]))
		}
		form.Close()
		req := httptest.NewRequest("POST", "/chapter/"+takenDown+"/pages", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		if script {
			req.Header.Set("Kotodex-Upload", "1")
		}
		token, _ := s.db.createSession(uid, "web", "test")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w
	}
	if w := post(member, false, [2]string{"1.png", string(red)}); w.Code != 403 {
		t.Fatalf("member upload: %d", w.Code)
	}
	if w := post(admin, false, [2]string{"1.png", string(red)}, [2]string{"notes.txt", "x"}); w.Code != 400 || !strings.Contains(w.Body.String(), "Choose JPEG, PNG, WebP, or GIF pages") {
		t.Fatalf("text file: %d %s", w.Code, w.Body)
	}
	if w := post(admin, false, [2]string{"1.png", "not an image"}); w.Code != 400 || !strings.Contains(w.Body.String(), "not a supported image") {
		t.Fatalf("fake image: %d %s", w.Code, w.Body)
	}
	if w := post(admin, false, [2]string{"a.cbz", "x"}, [2]string{"1.png", string(red)}); w.Code != 400 {
		t.Fatalf("archive and pages: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(s.mangaDir, mangaA, takenDown)); !os.IsNotExist(err) {
		t.Fatal("refused upload left files")
	}
	w = post(admin, true, [2]string{"page10.png", string(blue)}, [2]string{".DS_Store", "junk"}, [2]string{"page2.png", string(red)})
	if w.Code != 204 || w.Header().Get("Kotodex-Location") != "/manga/"+mangaA+"?unavailable=1" || !strings.Contains(w.Header().Get("Set-Cookie"), "notice=") {
		t.Fatalf("script upload: %d %v", w.Code, w.Header())
	}
	c, err := s.db.chapter(takenDown)
	if err != nil || c.State != stateDone || c.Pages != 2 || c.Source != "mangadex" || c.SourceURL != "https://mangadex.org/chapter/"+takenDown ||
		c.Number != "4" || c.Volume != "2" || c.Title != "Arrival" || c.Group != "Group" || !c.Unavailable || !c.Primary {
		t.Fatalf("chapter kept its MangaDex details: %+v %v", c, err)
	}
	if got, err := os.ReadFile(filepath.Join(s.mangaDir, mangaA, takenDown, c.Files[0])); err != nil || !bytes.Equal(got, red) {
		t.Fatal("page2 goes before page10")
	}
	if w := post(admin, false, [2]string{"1.png", string(red)}); w.Code != 409 {
		t.Fatalf("second upload: %d", w.Code)
	}
	// A later check keeps the pages.
	if _, err := s.db.insertChapter(&Chapter{ID: takenDown, MangaID: mangaA, Number: "4", Volume: "2", Title: "Arrival", Lang: "en", Group: "Group", Pages: 22, Unavailable: true}); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.db.chapter(takenDown); c.State != stateDone || c.Pages != 2 {
		t.Fatalf("check undid the upload: %+v", c)
	}
	// Shown like any downloaded chapter.
	w = adminRequest(t, s, admin, "GET", "/manga/"+mangaA, nil, "")
	if !strings.Contains(w.Body.String(), `id="c-`+takenDown+`" class="state2"`) || strings.Contains(w.Body.String(), "unavailable chapter") {
		t.Fatalf("uploaded chapter row: %s", w.Body)
	}
	// Deleting the pages makes it unavailable again.
	adminRequest(t, s, admin, "POST", "/chapter/"+takenDown+"/delete", nil, "")
	if c, err := s.db.chapter(takenDown); err != nil || c.State != stateSkipped || !c.Unavailable {
		t.Fatalf("deleted: %+v %v", c, err)
	}

	// One CBZ works too.
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("pages", "chapter.cbz")
	part.Write(sourceTestArchive(t, map[string][]byte{"b.png": blue, "a.png": red}))
	form.Close()
	if w := adminRequest(t, s, admin, "POST", "/chapter/"+takenDown+"/pages", &body, form.FormDataContentType()); w.Code != 303 {
		t.Fatalf("archive: %d %s", w.Code, w.Body)
	}
	if c, _ := s.db.chapter(takenDown); c.State != stateDone || c.Pages != 2 {
		t.Fatalf("archive pages: %+v", c)
	}
}

func TestTakenDownLeavesQueue(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	s.db.queueChapter(chapter2, now())
	if _, err := s.db.insertChapter(&Chapter{ID: chapter2, MangaID: mangaA, Number: "9.5", Lang: "en", Published: 200, Unavailable: true}); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.db.chapter(chapter2); c.State != stateSkipped || !c.Unavailable {
		t.Fatalf("still queued: %+v", c)
	}
	// Put back on MangaDex, it can be downloaded again.
	s.db.insertChapter(&Chapter{ID: chapter2, MangaID: mangaA, Number: "9.5", Lang: "en", Published: 200})
	if n, _ := s.db.queueMissingChapters([]*Chapter{{ID: chapter2, MangaID: mangaA}}, nil); n != 1 {
		t.Fatal("restored chapter can't be queued")
	}
	// Downloaded chapters keep their pages when taken down.
	s.db.insertChapter(&Chapter{ID: chapter1, MangaID: mangaA, Number: "10", Lang: "en", Published: 100, Unavailable: true})
	if c, _ := s.db.chapter(chapter1); c.State != stateDone {
		t.Fatalf("downloaded chapter lost: %+v", c)
	}
}

func TestChapterEdits(t *testing.T) {
	s, _ := testServer(t)
	member := seed(t, s)
	admin, err := s.db.createUser("admin", "pw", true)
	if err != nil {
		t.Fatal(err)
	}
	seedTakenDown(t, s)
	before, _ := s.db.chapter(takenDown)
	edit := func(uid int64, path string, fields url.Values) *httptest.ResponseRecorder {
		t.Helper()
		return adminRequest(t, s, uid, "POST", path, bytes.NewBufferString(fields.Encode()), "application/x-www-form-urlencoded")
	}
	fields := url.Values{"number": {"Ch. 004.50"}, "volume": {"3"}, "title": {" Return "}, "lang": {"es-la"}, "group": {"Other"}, "next": {"/manga/" + mangaA + "?lang=all"}}
	if w := edit(member, "/chapter/"+takenDown+"/edit", fields); w.Code != 403 {
		t.Fatalf("member edit: %d", w.Code)
	}
	if w := edit(admin, "/chapter/"+takenDown+"/edit", url.Values{"number": {"four"}, "lang": {"en"}}); w.Code != 400 {
		t.Fatalf("bad number: %d", w.Code)
	}
	if w := edit(admin, "/chapter/"+takenDown+"/edit", fields); w.Code != 303 || w.Header().Get("Location") != "/manga/"+mangaA+"?lang=all" {
		t.Fatalf("edit: %d %v", w.Code, w.Header())
	}
	c, _ := s.db.chapter(takenDown)
	if c.Number != "4.5" || c.Volume != "3" || c.Title != "Return" || c.Lang != "es-la" || c.Group != "Other" || !c.Edited {
		t.Fatalf("edited: %+v", c)
	}
	// Alone in its reader chapter, it keeps the identity Kotatsu knows.
	var lang, number string
	s.db.QueryRow(`SELECT lang,number_key FROM reader_chapters WHERE id=?`, c.ReaderID).Scan(&lang, &number)
	if c.ReaderID != before.ReaderID || lang != "es-la" || number != numberKey("4.5") {
		t.Fatalf("reader chapter: %d %d %s %s", c.ReaderID, before.ReaderID, lang, number)
	}

	// Checks keep the edit and remember what MangaDex says now.
	s.db.insertChapter(&Chapter{ID: takenDown, MangaID: mangaA, Number: "5", Volume: "2", Title: "Arrival (fixed)", Lang: "en", Group: "Group", Pages: 22, Unavailable: true})
	c, _ = s.db.chapter(takenDown)
	if c.Number != "4.5" || c.Title != "Return" {
		t.Fatalf("check undid the edit: %+v", c)
	}
	w := adminRequest(t, s, admin, "GET", "/chapter/"+takenDown, nil, "")
	if !strings.Contains(w.Body.String(), "(MangaDex: Arrival (fixed))") || !strings.Contains(w.Body.String(), "Restore MangaDex details") {
		t.Fatalf("chapter page: %s", w.Body)
	}
	if w := edit(admin, "/chapter/"+takenDown+"/restore", url.Values{}); w.Code != 303 {
		t.Fatalf("restore: %d", w.Code)
	}
	c, _ = s.db.chapter(takenDown)
	if c.Number != "5" || c.Volume != "2" || c.Title != "Arrival (fixed)" || c.Lang != "en" || c.Edited {
		t.Fatalf("restored: %+v", c)
	}

	// Saving the source's own details is no edit.
	edit(admin, "/chapter/"+takenDown+"/edit", url.Values{"number": {"5"}, "volume": {"2"}, "title": {"Arrival (fixed)"}, "lang": {"en"}, "group": {"Group"}})
	if c, _ := s.db.chapter(takenDown); c.Edited {
		t.Fatal("unchanged details kept as an edit")
	}

	// Renumbered onto a chapter Kotatsu already has, it joins that one.
	edit(admin, "/chapter/"+takenDown+"/edit", url.Values{"number": {"10"}, "volume": {""}, "title": {"Arrival"}, "lang": {"en"}, "group": {"Group"}})
	c, _ = s.db.chapter(takenDown)
	one, _ := s.db.chapter(chapter1)
	if c.ReaderID != one.ReaderID || !one.Primary {
		t.Fatalf("joined: %d %d %v", c.ReaderID, one.ReaderID, one.Primary)
	}
	var left int
	s.db.QueryRow(`SELECT count(*) FROM reader_chapters WHERE id=?`, before.ReaderID).Scan(&left)
	if left != 0 {
		t.Fatal("empty reader chapter left behind")
	}
	// And leaves it again when renumbered away, without taking it along.
	edit(admin, "/chapter/"+takenDown+"/edit", url.Values{"number": {"11"}, "lang": {"en"}})
	c, _ = s.db.chapter(takenDown)
	one, _ = s.db.chapter(chapter1)
	var anchor string
	s.db.QueryRow(`SELECT anchor_id FROM reader_chapters WHERE id=?`, one.ReaderID).Scan(&anchor)
	if c.ReaderID == one.ReaderID || c.ReaderID == 0 || anchor != chapter1 {
		t.Fatalf("left: %d %d %s", c.ReaderID, one.ReaderID, anchor)
	}

	// Uploaded chapters have nothing to restore.
	s.db.Exec(`INSERT INTO chapters(id,manga_id,number,lang,source,state) VALUES('up','` + mangaA + `','1','en','upload',2)`)
	edit(admin, "/chapter/up/edit", url.Values{"number": {"2"}, "lang": {"en"}})
	if c, _ := s.db.chapter("up"); c.Number != "2" || c.Edited {
		t.Fatalf("upload edit: %+v", c)
	}
}
