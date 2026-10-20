package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mdxAPI     = "https://api.mangadex.org"
	mdxAuth    = "https://auth.mangadex.org/realms/mangadex/protocol/openid-connect/token"
	mdxUploads = "https://uploads.mangadex.org"
	mdxReport  = "https://api.mangadex.network/report"
)

// MangaDex asks for at most 5 requests per second per IP on the API, and 40
// per minute on /at-home/server. Image servers have no limit, but reports for
// non-official image servers are required.
type limiter struct {
	mu   sync.Mutex
	next time.Time
	gap  time.Duration
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	t := time.Now()
	if l.next.Before(t) {
		l.next = t
	}
	delay := l.next.Sub(t)
	l.next = l.next.Add(l.gap)
	l.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type MangaDex struct {
	http    *http.Client
	agent   string
	api     limiter
	homeLim limiter
}

func newMangaDex(agent string) *MangaDex {
	return &MangaDex{
		http:    &http.Client{Timeout: 60 * time.Second},
		agent:   agent,
		api:     limiter{gap: 250 * time.Millisecond},
		homeLim: limiter{gap: 1600 * time.Millisecond},
	}
}

type mdxError struct {
	Status int
	Detail string
}

func (e *mdxError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("MangaDex: %s (HTTP %d)", e.Detail, e.Status)
	}
	return fmt.Sprintf("MangaDex: HTTP %d", e.Status)
}

func (m *MangaDex) get(ctx context.Context, lim *limiter, path string, q url.Values, token string, out any) error {
	for attempt := 0; ; attempt++ {
		if err := lim.wait(ctx); err != nil {
			return err
		}
		u := mdxAPI + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("User-Agent", m.agent)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := m.http.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			wait := 10 * time.Second
			if s, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Retry-After"), 10, 64); err == nil {
				wait = time.Until(time.Unix(s, 0)) + time.Second
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Errors []struct{ Detail string } `json:"errors"`
			}
			json.Unmarshal(body, &e)
			me := &mdxError{Status: resp.StatusCode}
			if len(e.Errors) > 0 {
				me.Detail = e.Errors[0].Detail
			}
			return me
		}
		return json.Unmarshal(body, out)
	}
}

// API shapes

type mdxRel struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Attributes json.RawMessage `json:"attributes"`
}

type mdxManga struct {
	ID         string `json:"id"`
	Attributes struct {
		Title       map[string]string   `json:"title"`
		AltTitles   []map[string]string `json:"altTitles"`
		Description map[string]string   `json:"description"`
		Status      string              `json:"status"`
		Year        int                 `json:"year"`
		Rating      string              `json:"contentRating"`
		OrigLang    string              `json:"originalLanguage"`
		Tags        []struct {
			Attributes struct {
				Name map[string]string `json:"name"`
			} `json:"attributes"`
		} `json:"tags"`
	} `json:"attributes"`
	Relationships []mdxRel `json:"relationships"`
}

type mdxChapter struct {
	ID         string `json:"id"`
	Attributes struct {
		Volume        *string `json:"volume"`
		Chapter       *string `json:"chapter"`
		Title         *string `json:"title"`
		Lang          string  `json:"translatedLanguage"`
		ExternalURL   *string `json:"externalUrl"`
		IsUnavailable bool    `json:"isUnavailable"`
		ReadableAt    string  `json:"readableAt"`
		PublishAt     string  `json:"publishAt"`
		Pages         int     `json:"pages"`
	} `json:"attributes"`
	Relationships []mdxRel `json:"relationships"`
}

func pickLang(m map[string]string, langs ...string) string {
	for _, l := range langs {
		if v := m[l]; v != "" {
			return v
		}
	}
	// Map order is random, so take the first language alphabetically.
	// Otherwise the title could change on every refresh.
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if v != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	slices.Sort(keys)
	return m[keys[0]]
}

// toManga converts a MangaDex manga. Titles prefer English, then romanized.
func (mm *mdxManga) toManga() *Manga {
	a := mm.Attributes
	m := &Manga{
		ID:          mm.ID,
		Title:       pickLang(a.Title, "en", a.OrigLang+"-ro", "ja-ro", "ko-ro", "zh-ro", a.OrigLang),
		Description: pickLang(a.Description, "en"),
		Status:      a.Status,
		Year:        a.Year,
		Rating:      a.Rating,
	}
	seen := map[string]bool{m.Title: true}
	for _, k := range slices.Sorted(maps.Keys(a.Title)) {
		if v := a.Title[k]; !seen[v] {
			seen[v] = true
			m.AltTitles = append(m.AltTitles, v)
		}
	}
	for _, alt := range a.AltTitles {
		for _, k := range slices.Sorted(maps.Keys(alt)) {
			if v := alt[k]; !seen[v] {
				seen[v] = true
				m.AltTitles = append(m.AltTitles, v)
			}
		}
	}
	for _, t := range a.Tags {
		if n := pickLang(t.Attributes.Name, "en"); n != "" {
			m.Tags = append(m.Tags, n)
		}
	}
	people := map[string]bool{}
	for _, r := range mm.Relationships {
		var attrs struct {
			Name     string `json:"name"`
			FileName string `json:"fileName"`
		}
		json.Unmarshal(r.Attributes, &attrs)
		switch r.Type {
		case "author", "artist":
			if attrs.Name != "" && !people[attrs.Name] {
				people[attrs.Name] = true
				m.Authors = append(m.Authors, attrs.Name)
			}
		case "cover_art":
			m.Cover = attrs.FileName
		}
	}
	return m
}

func (mc *mdxChapter) toChapter(mangaID string) *Chapter {
	a := mc.Attributes
	c := &Chapter{ID: mc.ID, MangaID: mangaID, Lang: a.Lang, Pages: a.Pages}
	if a.Volume != nil {
		c.Volume = *a.Volume
	}
	if a.Chapter != nil {
		c.Number = *a.Chapter
	}
	if a.Title != nil {
		c.Title = *a.Title
	}
	ts := a.ReadableAt
	if ts == "" {
		ts = a.PublishAt
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		c.Published = t.Unix()
	}
	var groups []string
	for _, r := range mc.Relationships {
		if r.Type == "scanlation_group" {
			var attrs struct{ Name string }
			json.Unmarshal(r.Attributes, &attrs)
			if attrs.Name != "" {
				groups = append(groups, attrs.Name)
			}
		}
	}
	c.Group = strings.Join(groups, ", ")
	return c
}

// readable reports whether the chapter is hosted on MangaDex. Official
// publisher chapters only link elsewhere and have no pages to download.
func (mc *mdxChapter) readable() bool {
	a := mc.Attributes
	return a.ExternalURL == nil && !a.IsUnavailable && a.Pages > 0
}

func ratingParams(q url.Values, ratings []string) {
	for _, r := range ratings {
		q.Add("contentRating[]", r)
	}
}

func (m *MangaDex) search(ctx context.Context, title string, ratings []string) ([]*Manga, error) {
	q := url.Values{}
	q.Set("title", title)
	q.Set("limit", "32")
	q.Add("includes[]", "cover_art")
	q.Add("includes[]", "author")
	q.Set("order[relevance]", "desc")
	ratingParams(q, ratings)
	var res struct{ Data []mdxManga }
	if err := m.get(ctx, &m.api, "/manga", q, "", &res); err != nil {
		return nil, err
	}
	out := make([]*Manga, 0, len(res.Data))
	for i := range res.Data {
		out = append(out, res.Data[i].toManga())
	}
	return out, nil
}

func (m *MangaDex) manga(ctx context.Context, id string) (*Manga, error) {
	q := url.Values{}
	q.Add("includes[]", "cover_art")
	q.Add("includes[]", "author")
	q.Add("includes[]", "artist")
	var res struct{ Data mdxManga }
	if err := m.get(ctx, &m.api, "/manga/"+url.PathEscape(id), q, "", &res); err != nil {
		return nil, err
	}
	return res.Data.toManga(), nil
}

// feed returns a manga's chapters in the given languages, or in every
// language when langs is empty. With all=false
// only the newest page is fetched, which is enough to spot new uploads.
func (m *MangaDex) feed(ctx context.Context, mangaID string, langs []string, all bool) ([]mdxChapter, error) {
	var out []mdxChapter
	limit := 500
	if !all {
		limit = 100
	}
	for offset := 0; ; offset += limit {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(limit))
		q.Set("offset", strconv.Itoa(offset))
		q.Add("includes[]", "scanlation_group")
		q.Set("order[readableAt]", "desc")
		// Leave out official publisher chapters, which only link elsewhere, so
		// they don't crowd real uploads out of the quick check's newest page.
		// Chapters taken down (DMCA) are left out unless asked for. They keep
		// their metadata and page count, so pages can be added by hand.
		q.Set("includeExternalUrl", "0")
		q.Set("includeEmptyPages", "0")
		q.Set("includeUnavailable", "1")
		for _, l := range langs {
			q.Add("translatedLanguage[]", l)
		}
		ratingParams(q, []string{"safe", "suggestive", "erotica", "pornographic"})
		var res struct {
			Data  []mdxChapter
			Total int
		}
		if err := m.get(ctx, &m.api, "/manga/"+url.PathEscape(mangaID)+"/feed", q, "", &res); err != nil {
			return nil, err
		}
		out = append(out, res.Data...)
		// MangaDex refuses offset+limit past 10000.
		if !all || len(res.Data) < limit || offset+2*limit > 10000 {
			return out, nil
		}
	}
}

type atHome struct {
	BaseURL string `json:"baseUrl"`
	Chapter struct {
		Hash      string   `json:"hash"`
		Data      []string `json:"data"`
		DataSaver []string `json:"dataSaver"`
	} `json:"chapter"`
}

func (m *MangaDex) atHome(ctx context.Context, chapterID string) (*atHome, error) {
	var res atHome
	if err := m.get(ctx, &m.homeLim, "/at-home/server/"+url.PathEscape(chapterID), nil, "", &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// fetch downloads a file into w and reports how it went to MangaDex@Home.
func (m *MangaDex) fetch(ctx context.Context, u string, w io.Writer) (int64, error) {
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", m.agent)
	resp, err := m.http.Do(req)
	var n int64
	cached := false
	if err == nil {
		cached = strings.HasPrefix(resp.Header.Get("X-Cache"), "HIT")
		if resp.StatusCode == http.StatusOK {
			n, err = io.Copy(w, resp.Body)
		} else {
			err = fmt.Errorf("image server returned HTTP %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	if !strings.Contains(u, "mangadex.org") {
		go m.report(u, err == nil, n, time.Since(start), cached)
	}
	return n, err
}

func (m *MangaDex) report(u string, ok bool, bytes int64, d time.Duration, cached bool) {
	body, _ := json.Marshal(map[string]any{
		"url": u, "success": ok, "bytes": bytes, "duration": d.Milliseconds(), "cached": cached,
	})
	req, _ := http.NewRequest(http.MethodPost, mdxReport, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", m.agent)
	if resp, err := m.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

// Account access
//
// MangaDex accounts sign in through a personal API client (created under
// Settings > API Clients on mangadex.org). Only the refresh token is stored.

type mdxTokens struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Error   string `json:"error_description"`
}

func (m *MangaDex) token(ctx context.Context, form url.Values) (*mdxTokens, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, mdxAuth, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", m.agent)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var t mdxTokens
	json.NewDecoder(resp.Body).Decode(&t)
	if resp.StatusCode != http.StatusOK || t.Access == "" {
		if t.Error == "" {
			t.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, shownError("MangaDex sign-in failed: " + t.Error)
	}
	return &t, nil
}

func (m *MangaDex) login(ctx context.Context, user, pass, clientID, secret string) (*mdxTokens, error) {
	return m.token(ctx, url.Values{
		"grant_type": {"password"}, "username": {user}, "password": {pass},
		"client_id": {clientID}, "client_secret": {secret},
	})
}

func (m *MangaDex) refresh(ctx context.Context, refresh, clientID, secret string) (*mdxTokens, error) {
	return m.token(ctx, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {clientID}, "client_secret": {secret},
	})
}

// follows lists every manga the account follows.
func (m *MangaDex) follows(ctx context.Context, access string) ([]*Manga, error) {
	var out []*Manga
	for offset := 0; ; offset += 100 {
		q := url.Values{}
		q.Set("limit", "100")
		q.Set("offset", strconv.Itoa(offset))
		q.Add("includes[]", "cover_art")
		q.Add("includes[]", "author")
		q.Add("includes[]", "artist")
		var res struct {
			Data  []mdxManga
			Total int
		}
		if err := m.get(ctx, &m.api, "/user/follows/manga", q, access, &res); err != nil {
			return nil, err
		}
		for i := range res.Data {
			out = append(out, res.Data[i].toManga())
		}
		if len(res.Data) < 100 || offset+100 >= res.Total {
			return out, nil
		}
	}
}
