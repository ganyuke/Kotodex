package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// ChapterSource translates a remote source into Kotodex chapters. Manga IDs
// belong to Kotodex's catalog; remote identities must never replace them.
// Connectors own their HTTP behavior, including rate limits and reporting.
// Chapters returns newest first; Pages returns reading order.
type ChapterSource interface {
	Chapters(context.Context, string, bool) ([]*Chapter, error)
	Pages(context.Context, *Chapter, bool) ([]SourcePage, error)
	Fetch(context.Context, string, io.Writer) (int64, error)
}

type SourcePage struct{ URL, Extension string }

type mangaDexSource struct{ mdx *MangaDex }

func (s mangaDexSource) Chapters(ctx context.Context, ref string, full bool) ([]*Chapter, error) {
	feed, err := s.mdx.feed(ctx, ref, nil, full)
	if err != nil {
		return nil, err
	}
	var out []*Chapter
	for _, item := range feed {
		// External chapters only link to a publisher. Unavailable chapters
		// stay in the list so their pages can be added by hand.
		if item.Attributes.ExternalURL != nil {
			continue
		}
		c := item.toChapter(ref)
		c.Unavailable = !item.readable()
		normalizeChapter(c)
		c.Source, c.SourceRef, c.SourceURL = "mangadex", c.ID, "https://mangadex.org/chapter/"+c.ID
		out = append(out, c)
	}
	return out, nil
}

func (s mangaDexSource) Pages(ctx context.Context, c *Chapter, saver bool) ([]SourcePage, error) {
	ref := c.SourceRef
	if ref == "" {
		ref = c.ID
	} // databases created before connectors
	ah, err := s.mdx.atHome(ctx, ref)
	if err != nil {
		return nil, err
	}
	files, kind := ah.Chapter.Data, "data"
	if saver && len(ah.Chapter.DataSaver) > 0 {
		files, kind = ah.Chapter.DataSaver, "data-saver"
	}
	var out []SourcePage
	for _, f := range files {
		out = append(out, SourcePage{fmt.Sprintf("%s/%s/%s/%s", ah.BaseURL, kind, ah.Chapter.Hash, f), path.Ext(f)})
	}
	return out, nil
}

func (s mangaDexSource) Fetch(ctx context.Context, u string, w io.Writer) (int64, error) {
	return s.mdx.fetch(ctx, u, w)
}

// JSON feeds let a separate source adapter publish chapters without being
// coupled to Kotodex, its credentials, or its database. No scripts execute here.
type jsonSource struct{ client *http.Client }

type feedChapter struct {
	ID        string   `json:"id"`
	Volume    string   `json:"volume"`
	Number    string   `json:"number"`
	Title     string   `json:"title"`
	Lang      string   `json:"lang"`
	Group     string   `json:"group"`
	Published int64    `json:"published"`
	URL       string   `json:"url"`
	Pages     []string `json:"pages"`
}

const maxSourcePage = 32 << 20

func webAddress(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

func (s *jsonSource) get(ctx context.Context, raw string) (*http.Response, error) {
	if !webAddress(raw) {
		return nil, errors.New("source requires an HTTP or HTTPS URL without credentials or fragments")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("source returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (s *jsonSource) Chapters(ctx context.Context, ref string, _ bool) ([]*Chapter, error) {
	resp, err := s.get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 8<<20 {
		return nil, errors.New("chapter feed exceeds 8 MiB")
	}
	var feed struct {
		Chapters []feedChapter `json:"chapters"`
	}
	if err := json.Unmarshal(body, &feed); err != nil {
		return nil, errors.New("source did not return a chapter feed")
	}
	if len(feed.Chapters) == 0 || len(feed.Chapters) > 10000 {
		return nil, errors.New("feed must contain between 1 and 10000 chapters")
	}
	seen := map[string]bool{}
	out := make([]*Chapter, 0, len(feed.Chapters))
	for _, item := range feed.Chapters {
		item.Volume, item.Number = normalizeChapterNumber(item.Volume), normalizeChapterNumber(item.Number)
		if item.ID == "" || len(item.ID) > 500 || seen[item.ID] || !validChapterMetadata(item.Volume, item.Number, item.Lang) || len(item.Pages) == 0 || len(item.Pages) > 1000 || len(item.Title) > 500 || len(item.Group) > 500 || item.Published < 0 {
			return nil, errors.New("feed contains an invalid or duplicate chapter")
		}
		if item.URL != "" && !webAddress(item.URL) {
			return nil, errors.New("invalid chapter attribution URL")
		}
		seen[item.ID] = true
		for _, p := range item.Pages {
			if !webAddress(p) {
				return nil, errors.New("invalid page URL")
			}
		}
		out = append(out, &Chapter{Source: "json", SourceRef: item.ID, SourceURL: item.URL,
			Volume: item.Volume, Number: item.Number, Title: item.Title, Lang: item.Lang, Group: item.Group,
			Published: item.Published, Pages: len(item.Pages), RemotePages: item.Pages})
	}
	return out, nil
}

func (s *jsonSource) Pages(_ context.Context, c *Chapter, _ bool) ([]SourcePage, error) {
	var out []SourcePage
	for _, raw := range c.RemotePages {
		u, err := url.Parse(raw)
		if err != nil || !webAddress(raw) {
			return nil, errors.New("invalid page URL")
		}
		ext := strings.ToLower(path.Ext(u.Path))
		if !imageExtension(ext) {
			ext = ".img"
		}
		out = append(out, SourcePage{raw, ext})
	}
	return out, nil
}

func (s *jsonSource) Fetch(ctx context.Context, raw string, w io.Writer) (int64, error) {
	resp, err := s.get(ctx, raw)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return copySourceImage(resp.Body, w)
}

func copySourceImage(r io.Reader, w io.Writer) (int64, error) {
	// An HTML challenge is never saved as a page.
	data, err := io.ReadAll(io.LimitReader(r, maxSourcePage+1))
	if err != nil {
		return 0, err
	}
	if len(data) > maxSourcePage {
		return 0, errors.New("page exceeds 32 MiB")
	}
	if _, ok := coverTypes[http.DetectContentType(data)]; !ok {
		return 0, errors.New("page is not a supported image")
	}
	n, err := w.Write(data)
	return int64(n), err
}

// Stable, namespaced chapter IDs prevent unrelated feeds from colliding.
func feedChapterID(mangaID, feedURL, remoteID string) string {
	data, _ := json.Marshal([]string{mangaID, feedURL, remoteID})
	return uuid.NewSHA1(uuid.NameSpaceURL, data).String()
}

type chapterFeed struct {
	URL     string
	Source  string
	Checked int64
	Error   string
}

func (db *DB) chapterFeeds(mangaID string) ([]chapterFeed, error) {
	rows, err := db.Query(`SELECT url,source,checked,error FROM chapter_feeds WHERE manga_id=? ORDER BY url`, mangaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chapterFeed
	for rows.Next() {
		var f chapterFeed
		if err := rows.Scan(&f.URL, &f.Source, &f.Checked, &f.Error); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (d *Downloader) checkFeeds(ctx context.Context, m *Manga) (candidates []*Chapter) {
	feeds, err := d.db.chapterFeeds(m.ID)
	if err != nil {
		return
	}
	for _, feed := range feeds {
		if ctx.Err() != nil {
			return
		}
		source := d.sources[feed.Source]
		if source == nil {
			continue
		}
		chapters, err := source.Chapters(ctx, feed.URL, true)
		if err == nil && feed.Source == "mangafire" {
			var volumes *chapterVolumes
			volumes, err = d.db.chapterVolumes(m.ID)
			if err == nil {
				for _, c := range chapters {
					if volume := volumes.volume(c); volume != "" {
						c.Volume = volume
					}
				}
			}
		}
		if err == nil {
			wanted := d.db.wantedLangs(m.ID)
			for i := len(chapters) - 1; i >= 0; i-- {
				c := chapters[i]
				c.Source, c.FeedURL = feed.Source, feed.URL
				c.MangaID, c.ID = m.ID, feedChapterID(m.ID, feed.Source+":"+feed.URL, c.SourceRef)
				c.State = stateSkipped
				eligible := feed.Checked != 0 && wanted[c.Lang] && (c.Published == 0 || c.Published >= feed.Checked-24*3600)
				var inserted bool
				if inserted, err = d.db.insertChapter(c); err != nil {
					break
				}
				if inserted && eligible {
					candidates = append(candidates, c)
				}
			}
		}
		if err != nil {
			// Detailed upstream errors stay in server logs, like other errors.
			d.db.Exec(`UPDATE chapter_feeds SET error=? WHERE manga_id=? AND url=?`, userMessage(err), m.ID, feed.URL)
		} else {
			d.db.Exec(`UPDATE chapter_feeds SET checked=?,error='' WHERE manga_id=? AND url=?`, now(), m.ID, feed.URL)
		}
	}
	return candidates
}

func sourceLabel(source string) string {
	switch source {
	case "mangadex":
		return "MangaDex"
	case "mangafire":
		return "MangaFire"
	case "json":
		return "Imported chapters"
	case "upload":
		return "Upload"
	default:
		return source
	}
}

// A connector that accepts explicit title/feed mappings advertises its URL
// validation here. Registering it also adds it to the administrator's picker.
type linkedChapterSource interface {
	ChapterSource
	NormalizeReference(string) (string, error)
}

type sourceOption struct{ Code, Name string }

func (d *Downloader) linkedSources() []sourceOption {
	var out []sourceOption
	for code, source := range d.sources {
		if code == "json" {
			continue
		}
		if _, ok := source.(linkedChapterSource); ok {
			out = append(out, sourceOption{code, sourceLabel(code)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *jsonSource) NormalizeReference(raw string) (string, error) {
	if !webAddress(raw) || len(raw) > 2000 {
		return "", errors.New("use an HTTP or HTTPS feed URL without credentials or fragments")
	}
	return raw, nil
}
