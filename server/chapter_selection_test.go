package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestChapterSourceFilterAndDownloadSelection(t *testing.T) {
	for _, tc := range []struct {
		name, query, preferred, winner string
		count                          int
	}{
		{"default", "", "", "mangadex", 1},
		{"combined default", "sources=1&source=mangadex&source=mangafire", "", "mangadex", 1},
		{"combined prefer MangaFire", "sources=1&source=mangadex&source=mangafire", "mangafire", "mangafire", 1},
		{"MangaFire only", "sources=1&source=mangafire", "mangadex", "mangafire", 1},
		{"nothing selected", "sources=1", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testServer(t)
			uid := seed(t, s)
			s.db.Exec(`UPDATE chapters SET state=2`)
			for _, c := range []*Chapter{
				{ID: "new-md", MangaID: mangaA, Source: "mangadex", Number: "12", Lang: "en", Title: "Dex release", Published: 1000},
				{ID: "new-mf", MangaID: mangaA, Source: "mangafire", Number: "Ch. 012.00", Lang: "en", Title: "Fire release", Published: 100},
				{ID: "new-fr", MangaID: mangaA, Source: "mangafire", Number: "12", Lang: "fr", Title: "French release"},
			} {
				if _, err := s.db.insertChapter(c); err != nil {
					t.Fatal(err)
				}
			}
			values, _ := url.ParseQuery(tc.query)
			if tc.preferred != "" {
				values.Set("prefer", tc.preferred)
			}
			token, _ := s.db.createSession(uid, "web", "test")
			req := httptest.NewRequest("GET", "/manga/"+mangaA+"?"+values.Encode(), nil)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("page: %d %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			selected := selectedChapterSources(values)
			if strings.Contains(body, `id="c-new-md"`) != slices.Contains(selected, "mangadex") {
				t.Fatal("MangaDex row visibility disagrees with filter")
			}
			if strings.Contains(body, `id="c-new-mf"`) != slices.Contains(selected, "mangafire") {
				t.Fatal("MangaFire row visibility disagrees with filter")
			}
			if strings.Contains(body, `id="c-new-fr"`) {
				t.Fatal("language filter lost when filtering source")
			}
			if !strings.Contains(body, `name="sources" value="1"`) {
				t.Fatal("empty source selection cannot be submitted")
			}
			if strings.Contains(body, `class="list compact fixed chapters"`) && !strings.Contains(body, `hx-swap:inherited="innerHTML show:none"`) {
				t.Fatal("chapter actions must stay at the row instead of scrolling to the top")
			}
			if strings.Contains(body, `action="/chapter/new-mf/download"`) && !strings.Contains(body, `name="next" value="/manga/`+mangaA) {
				t.Fatal("chapter actions must return to this manga page")
			}
			values.Set("lang", "en")
			req = httptest.NewRequest("POST", "/manga/"+mangaA+"/download-all", strings.NewReader(values.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
			w = httptest.NewRecorder()
			s.routes().ServeHTTP(w, req)
			if w.Code != 303 {
				t.Fatalf("bulk download: %d %s", w.Code, w.Body.String())
			}
			q, err := s.db.queue()
			if err != nil || len(q) != tc.count {
				t.Fatalf("queued %d, expected %d (%v)", len(q), tc.count, err)
			}
			if tc.count > 0 && q[0].Source != tc.winner {
				t.Fatalf("preferred release: %s, expected %s", q[0].Source, tc.winner)
			}
		})
	}
}

func TestChapterDeduplicationIdentityAndConcurrency(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	var chapters []*Chapter
	for _, c := range []*Chapter{
		{ID: "md", MangaID: mangaA, Source: "mangadex", Volume: "Vol. 002.00", Number: "Ch. 0012.500", Lang: "en"},
		{ID: "mf", MangaID: mangaA, Source: "mangafire", Volume: "2", Number: "12.5", Lang: "en"},
		{ID: "french", MangaID: mangaA, Source: "mangafire", Volume: "2", Number: "12.5", Lang: "fr"},
		{ID: "other-volume", MangaID: mangaA, Source: "mangafire", Volume: "3", Number: "12.5", Lang: "en"},
		{ID: "numberless-1", MangaID: mangaA, Source: "mangadex", Lang: "en"},
		{ID: "numberless-2", MangaID: mangaA, Source: "mangadex", Lang: "en"},
		{ID: "large-1", MangaID: mangaA, Source: "mangadex", Number: "9007199254740992.1", Lang: "en"},
		{ID: "large-2", MangaID: mangaA, Source: "mangadex", Number: "9007199254740992.2", Lang: "en"},
	} {
		if _, err := s.db.insertChapter(c); err != nil {
			t.Fatal(err)
		}
		chapters = append(chapters, c)
	}
	if got := chapterLabel(chapters[0]); got != "Vol. 2 Ch. 12.5" {
		t.Fatalf("connector label: %s", got)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; _, err := s.db.queueMissingChapters(chapters, nil); results <- err }()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	queue, err := s.db.queue()
	if err != nil || len(queue) != 7 {
		t.Fatalf("deduplication lost distinct chapters or queued duplicates: %d %v", len(queue), err)
	}
	mf, _ := s.db.chapter("mf")
	if mf.State != stateSkipped {
		t.Fatal("duplicate MangaFire release queued")
	}
	unknown := &Chapter{ID: "unknown-volume", MangaID: mangaA, Source: "mangafire", Number: "12.50", Lang: "en"}
	s.db.insertChapter(unknown)
	if n, err := s.db.queueMissingChapters([]*Chapter{unknown}, nil); err != nil || n != 0 {
		t.Fatalf("unknown volume duplicated existing chapter: %d %v", n, err)
	}
}

type discoveredSource struct {
	failedSource
	chapters []*Chapter
}

func (s discoveredSource) Chapters(context.Context, string, bool) ([]*Chapter, error) {
	var out []*Chapter
	for _, c := range s.chapters {
		copy := *c
		out = append(out, &copy)
	}
	return out, nil
}

func TestScheduledChecksSelectAcrossSources(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "both sources"
		if fail {
			name = "MangaDex unavailable"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := testServer(t)
			seed(t, s)
			s.db.Exec(`UPDATE manga SET checked=? WHERE id=?`, now()-1, mangaA)
			s.db.Exec(`INSERT INTO chapter_feeds(manga_id,url,source,checked) VALUES(?,?,'mangafire',?)`, mangaA, mangaFireOrigin+"/title/72xyj", now()-1)
			s.dl.sources["mangadex"] = discoveredSource{chapters: []*Chapter{{ID: "new-md", Source: "mangadex", MangaID: mangaA, Number: "12", Lang: "en", Published: now()}}}
			if fail {
				s.dl.sources["mangadex"] = failedSource{}
			}
			s.dl.sources["mangafire"] = discoveredSource{chapters: []*Chapter{{SourceRef: "123", Number: "12.0", Lang: "en", Published: now()}}}
			s.dl.checkManga(context.Background(), &Manga{ID: mangaA, Title: "Akanabe", Refreshed: now()})
			q, err := s.db.queue()
			if err != nil || len(q) != 1 {
				t.Fatalf("scheduled queue: %+v %v", q, err)
			}
			want := "mangadex"
			if fail {
				want = "mangafire"
			}
			if q[0].Source != want {
				t.Fatalf("scheduled priority: %s, expected %s", q[0].Source, want)
			}
		})
	}
}

func TestChapterNumberNormalization(t *testing.T) {
	for raw, want := range map[string]string{"Ch. 0012.500": "12.5", "Chapter 001": "1", "Vol.002.0": "2", " 00.0500 ": "0.05", "000": "0", "Extra": "Extra", "": "", "Ch. Extra": "Ch. Extra"} {
		if got := normalizeChapterNumber(raw); got != want {
			t.Errorf("%q -> %q, expected %q", raw, got, want)
		}
	}
}

func TestChapterRefreshPreservesDownloadedPageCount(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	c := &Chapter{ID: "mf-downloaded", MangaID: mangaA, Source: "mangafire", SourceRef: "123", Number: "12", Lang: "en", Pages: 23}
	if _, err := s.db.insertChapter(c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE chapters SET state=2 WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	c.Pages = 0
	if _, err := s.db.insertChapter(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.db.chapter(c.ID)
	if err != nil || got.Pages != 23 {
		t.Fatalf("source refresh discarded local page count: %+v %v", got, err)
	}
	if numberKey("5/2") == numberKey("2.5") {
		t.Fatal("a nonnumeric label collided with a decimal identity")
	}
}

func TestReaderSourceMenuHidesTechnicalAdapters(t *testing.T) {
	s, _ := testServer(t)
	reader := seed(t, s)
	admin, err := s.db.createUser("admin", "pw", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []int64{reader, admin} {
		token, _ := s.db.createSession(uid, "web", "test")
		req := httptest.NewRequest("GET", "/manga/"+mangaA, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, `id="chapter-source-filter"`) {
			t.Fatalf("source menu: %d %s", w.Code, body)
		}
		if strings.Contains(body, `type="checkbox" name="source" value="json"`) || strings.Contains(body, `<option value="json"`) {
			t.Fatal("technical adapter exposed as a provider choice")
		}
		if strings.Contains(body, "Advanced adapters") != (uid == admin) {
			t.Fatal("advanced adapter configuration has incorrect visibility")
		}
		if uid == reader && strings.Contains(body, "JSON") {
			t.Fatal("reader UI exposes connector format")
		}
		if !strings.Contains(body, `<button class="source-filter-apply">Apply</button>`) {
			t.Fatal("source selection lacks explicit apply")
		}
	}
	token, _ := s.db.createSession(reader, "web", "test")
	req := httptest.NewRequest("POST", "/manga/"+mangaA+"/feeds", strings.NewReader(url.Values{"source": {"json"}, "url": {"https://adapter.example/feed"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("reader configured advanced adapter: %d", w.Code)
	}
}

func TestCompleteSourcePriorityControlsEveryFallback(t *testing.T) {
	for _, tc := range []struct {
		name              string
		priority, sources []string
		want              map[string]string
	}{
		{"second and third", []string{"json", "mangafire", "mangadex"}, []string{"json", "mangafire", "mangadex"}, map[string]string{"12": "mangafire", "13": "json", "14": "mangadex"}},
		{"change second only", []string{"json", "mangadex", "mangafire"}, []string{"json", "mangafire", "mangadex"}, map[string]string{"12": "mangadex", "13": "json", "14": "mangadex"}},
		{"hidden first", []string{"json", "mangafire", "mangadex"}, []string{"mangafire", "mangadex"}, map[string]string{"12": "mangafire", "13": "mangadex", "14": "mangadex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testServer(t)
			uid := seed(t, s)
			s.db.Exec(`UPDATE chapters SET state=2`)
			for _, pair := range [][2]string{{"mangadex", "12"}, {"mangafire", "12"}, {"mangadex", "13"}, {"json", "13"}, {"mangadex", "14"}} {
				if _, err := s.db.insertChapter(&Chapter{ID: pair[0] + pair[1], MangaID: mangaA, Source: pair[0], Number: pair[1], Lang: "en"}); err != nil {
					t.Fatal(err)
				}
			}
			token, _ := s.db.createSession(uid, "web", "test")
			values := url.Values{"sources": {"1"}, "source": tc.sources, "priority": tc.priority, "lang": {"en"}}
			req := httptest.NewRequest("POST", "/manga/"+mangaA+"/download-all", strings.NewReader(values.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, req)
			if w.Code != 303 {
				t.Fatalf("download: %d %s", w.Code, w.Body.String())
			}
			queue, err := s.db.queue()
			if err != nil || len(queue) != len(tc.want) {
				t.Fatalf("queue: %+v %v", queue, err)
			}
			for _, c := range queue {
				if tc.want[c.Number] != c.Source {
					t.Fatalf("chapter %s: got %s, want %s", c.Number, c.Source, tc.want[c.Number])
				}
			}
			// Refreshing or changing language must keep all priorities, including
			// unchecked sources, instead of rebuilding the old fixed fallback.
			req = httptest.NewRequest("GET", "/manga/"+mangaA+"?"+values.Encode(), nil)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
			w = httptest.NewRecorder()
			s.routes().ServeHTTP(w, req)
			body := w.Body.String()
			previous := -1
			for _, source := range tc.priority {
				index := strings.Index(body, `data-source-row="`+source+`"`)
				if index <= previous {
					t.Fatalf("priority order was lost: %v", tc.priority)
				}
				previous = index
			}
		})
	}
}

func TestSourcePriorityParsing(t *testing.T) {
	got := chapterSourcePriority(url.Values{"priority": {"mangafire", "mangafire", "", "json", "mangadex"}, "prefer": {"mangadex"}})
	if !slices.Equal(got, []string{"mangafire", "json", "mangadex"}) {
		t.Fatalf("full priority: %v", got)
	}
}
