package main

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func TestMangaFireFeedAssignsVolumesBeforeBinding(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	md := &Chapter{ID: "md12", MangaID: mangaA, Number: "12", Volume: "2", Lang: "en"}
	if _, err := s.db.insertChapter(md); err != nil {
		t.Fatal(err)
	}
	md, _ = s.db.chapter(md.ID)
	ref := mangaFireOrigin + "/title/72xyj"
	if _, err := s.db.Exec(`INSERT INTO chapter_feeds(manga_id,url,source) VALUES(?,?,'mangafire')`, mangaA, ref); err != nil {
		t.Fatal(err)
	}
	s.dl.sources["mangafire"] = discoveredSource{chapters: []*Chapter{{SourceRef: "123", Number: "12.0", Lang: "en"}}}
	for pass := 0; pass < 2; pass++ {
		s.dl.checkFeeds(context.Background(), &Manga{ID: mangaA})
		mf, err := s.db.chapter(feedChapterID(mangaA, "mangafire:"+ref, "123"))
		if err != nil || mf.Volume != "2" || mf.ReaderID != md.ReaderID {
			t.Fatalf("source check must retain canonical volume and identity: %+v %v", mf, err)
		}
	}
}

func TestMangaFireMatchesMangaDexVolumes(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	for _, c := range []*Chapter{
		{ID: "md10", Number: "10", Volume: "2", Lang: "en"},
		{ID: "md12", Number: "12", Volume: "2", Lang: "en"},
		{ID: "md13", Number: "13", Volume: "3", Lang: "en"},
		{ID: "md14fr", Number: "14", Volume: "3", Lang: "fr"},
		{ID: "md15en", Number: "15", Volume: "3", Lang: "en"},
		{ID: "md15fr", Number: "15", Volume: "4", Lang: "fr"},
		{ID: "md16a", Number: "16", Volume: "3", Lang: "en"},
		{ID: "md16b", Number: "16", Volume: "4", Lang: "en"},
		{ID: "mdprecise1", Number: "20.0000000000000001", Volume: "4", Lang: "en"},
		{ID: "mdprecise2", Number: "20.0000000000000002", Volume: "5", Lang: "en"},
	} {
		c.MangaID = mangaA
		if _, err := s.db.insertChapter(c); err != nil {
			t.Fatal(err)
		}
	}
	index, err := s.db.chapterVolumes(mangaA)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ number, lang, want string }{
		{"10.0", "en", "2"},
		{"11", "en", "2"},  // missing chapter bounded by two chapters in volume 2
		{"12.5", "en", ""}, // volume boundary is not known
		{"14", "en", "3"},  // another language supplies the unambiguous volume
		{"15", "en", "3"},
		{"15", "fr", "4"},
		{"15", "de", ""}, // conflicting translations cannot supply a volume
		{"16", "en", ""}, // conflicting copies in the same language
		{"100", "en", ""},
		{"", "en", ""},
		{"20.0000000000000001", "en", "4"},
		{"20.0000000000000002", "en", "5"},
	} {
		if got := index.volume(&Chapter{Number: test.number, Lang: test.lang}); got != test.want {
			t.Errorf("chapter %s (%s): volume %q, want %q", test.number, test.lang, got, test.want)
		}
	}
	alt := &Chapter{ID: "mf11", MangaID: mangaA, Source: "mangafire", Number: "11", Lang: "en", State: stateDone}
	if _, err := s.db.insertChapter(alt); err != nil {
		t.Fatal(err)
	}
	before, _ := s.db.chapter(alt.ID)
	if _, err := s.db.readerChapters(mangaA); err != nil {
		t.Fatal(err)
	}
	after, _ := s.db.chapter(alt.ID)
	if after.Volume != "2" || after.ReaderID != before.ReaderID || after.State != stateDone {
		t.Fatalf("volume alignment must keep identity and files: %+v", after)
	}
	list, err := s.db.chapters(mangaA)
	if err != nil {
		t.Fatal(err)
	}
	var at int
	for i, c := range list {
		if c.ID == alt.ID {
			at = i
		}
	}
	if at == 0 || at == len(list)-1 || list[at-1].Number != "10" || list[at+1].Number != "12" {
		t.Fatal("MangaFire chapter 11 must appear within MangaDex's volume 2 sequence")
	}
}

func TestUnassignedMangaFireChaptersStayInSequence(t *testing.T) {
	for _, test := range []struct {
		name     string
		chapters []*Chapter
		want     []string
	}{
		{"continuous", []*Chapter{
			{ID: "a", Number: "12", Volume: "2", Lang: "en"},
			{ID: "b", Number: "13", Volume: "3", Lang: "en"},
			{ID: "mf", Source: "mangafire", Number: "12.5", Lang: "en"},
		}, []string{"a", "mf", "b"}},
		{"volumes restart chapter numbers", []*Chapter{
			{ID: "a", Number: "12", Volume: "1", Lang: "en"},
			{ID: "b", Number: "1", Volume: "2", Lang: "en"},
			{ID: "mf", Source: "mangafire", Number: "3", Lang: "en"},
		}, []string{"a", "b", "mf"}},
		{"independent languages", []*Chapter{
			{ID: "en1", Number: "12", Volume: "2", Lang: "en"},
			{ID: "fr1", Number: "12", Volume: "1", Lang: "fr"},
			{ID: "en2", Number: "13", Volume: "3", Lang: "en"},
			{ID: "fr2", Number: "1", Volume: "2", Lang: "fr"},
			{ID: "mf", Source: "mangafire", Number: "12.5", Lang: "en"},
		}, []string{"en1", "fr1", "mf", "fr2", "en2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			interleaveUnassignedChapters(test.chapters)
			var got []string
			for _, c := range test.chapters {
				got = append(got, c.ID)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("sequence %v, want %v", got, test.want)
			}
		})
	}
}

func TestReaderSequenceUsesUndownloadedVolumeEvidence(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	for _, c := range []*Chapter{
		{ID: "first-volume", Number: "12", Volume: "1"},
		{ID: "second-volume", Number: "9", Volume: "2", State: stateDone},
		{ID: "unknown", Source: "mangafire", Number: "3", State: stateDone},
	} {
		c.MangaID, c.Lang = mangaA, "en"
		if _, err := s.db.insertChapter(c); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.db.readerChapters(mangaA)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{"second-volume", "unknown", chapter1}) {
		// Unknown-volume rows keep their original number order after the
		// known volumes when the catalog shows that numbering restarts.
		t.Fatalf("downloaded subset must not erase volume numbering evidence: %v", ids)
	}
}

func TestExistingChapterAttributionAndVolumesRepairOnStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.upsertManga(&Manga{ID: mangaA, Title: "Test"})
	for _, c := range []*Chapter{
		{ID: chapter1, MangaID: mangaA, Number: "90", Volume: "7", Lang: "en"},
		{ID: "mf90", MangaID: mangaA, Source: "mangafire", Number: "90", Lang: "en", State: stateDone},
	} {
		if _, err := db.insertChapter(c); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := db.chapter("mf90")
	db.Exec(`UPDATE chapters SET source_ref='',source_url='' WHERE id=?`, chapter1)
	db.Exec(`UPDATE chapters SET files='["001.jpg"]',downloaded=123 WHERE id='mf90'`)
	db.Close()
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	md, _ := db.chapter(chapter1)
	if md.SourceRef != chapter1 || md.SourceURL != "https://mangadex.org/chapter/"+chapter1 {
		t.Fatal("legacy MangaDex attribution must be linked")
	}
	mf, _ := db.chapter("mf90")
	if mf.Volume != "7" || mf.ReaderID != before.ReaderID || mf.Downloaded != 123 || len(mf.Files) != 1 {
		t.Fatalf("existing MangaFire copy must gain volume without changing identity or files: %+v", mf)
	}
}
