package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// JSON API for the Kotatsu plugin. Every request except login carries the
// session token as "Authorization: Bearer <token>". File URLs in responses
// are relative to the server address and already signed.

type apiManga struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	AltTitles     []string `json:"altTitles"`
	Description   string   `json:"description"`
	Authors       []string `json:"authors"`
	Status        string   `json:"status"`
	Rating        string   `json:"contentRating"`
	Tags          []string `json:"tags"`
	Year          int      `json:"year"`
	CoverURL      string   `json:"coverUrl"`
	LargeCoverURL string   `json:"largeCoverUrl"`
	PublicURL     string   `json:"publicUrl"`
	Chapters      int      `json:"chapters"`
	Updated       int64    `json:"updated"`
}

type apiChapter struct {
	ID        string   `json:"id"`
	ReaderID  int64    `json:"readerId"`
	Source    string   `json:"source"`
	SourceURL string   `json:"sourceUrl,omitempty"`
	Title     string   `json:"title"`
	Number    *float64 `json:"number"` // null for oneshots and extras without a number
	Volume    int      `json:"volume"`
	Lang      string   `json:"lang"`
	Group     string   `json:"group"`
	Published int64    `json:"published"`
	Pages     int      `json:"pages"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) toAPIManga(u *User, m *Manga) apiManga {
	// Storage counts include alternate copies; the reader sees logical chapters.
	downloaded := m.Downloaded
	s.db.QueryRow(`SELECT count(DISTINCT reader_id) FROM chapters WHERE manga_id=? AND state=2`, m.ID).Scan(&downloaded)
	a := apiManga{
		ID: m.ID, Title: m.Title, AltTitles: nonNil(m.AltTitles), Description: m.Description,
		Authors: nonNil(m.Authors), Status: m.Status, Rating: m.Rating, Tags: nonNil(m.Tags), Year: m.Year,
		PublicURL: "https://mangadex.org/title/" + m.ID, Chapters: downloaded, Updated: m.Latest * 1000,
	}
	if m.Cover != "" {
		a.CoverURL = s.coverURL(u, m, thumbPath(m))
		a.LargeCoverURL = s.coverURL(u, m, coverPath(m))
	}
	return a
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, Device string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "Bad request")
		return
	}
	keys := loginKeys(r, req.Username)
	if !s.limiter.allow(keys) {
		apiError(w, http.StatusTooManyRequests, "Too many attempts. Try again later.")
		return
	}
	u, err := s.db.checkPassword(req.Username, req.Password)
	if err != nil {
		s.limiter.fail(keys)
		apiError(w, http.StatusUnauthorized, "Wrong username or password")
		return
	}
	device := req.Device
	if device == "" {
		device = r.UserAgent()
	}
	token, err := s.db.createSession(u.ID, "kotatsu", device)
	if err != nil {
		apiError(w, http.StatusInternalServerError, userMessage(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "username": u.Name})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	s.db.deleteSession(sessionToken(r))
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"username": currentUser(r).Name})
}

func (s *Server) apiList(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	list, err := s.db.library(LibraryQuery{
		UserID:   u.ID,
		Query:    strings.TrimSpace(q.Get("query")),
		Tags:     q["tag"],
		Sort:     q.Get("sort"),
		Offset:   offset,
		Limit:    limit,
		Followed: q.Get("followed") == "1",
		Readable: true,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, userMessage(err))
		return
	}
	out := make([]apiManga, 0, len(list))
	for _, m := range list {
		out = append(out, s.toAPIManga(u, m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiManga(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	m, err := s.db.manga(r.PathValue("id"), u.ID)
	if err != nil {
		apiError(w, http.StatusNotFound, "Not found")
		return
	}
	chapters, err := s.db.readerChapters(m.ID)
	if err == nil {
		err = s.db.fillMissingTitles(m.ID, chapters)
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, userMessage(err))
		return
	}
	out := struct {
		apiManga
		ChapterList []apiChapter `json:"chapterList"`
	}{apiManga: s.toAPIManga(u, m), ChapterList: make([]apiChapter, 0, len(chapters))}
	for _, c := range chapters {
		var num *float64
		if n, err := strconv.ParseFloat(c.Number, 64); err == nil {
			num = &n
		}
		vol, _ := strconv.ParseFloat(c.Volume, 64)
		source, sourceURL := c.Source, c.SourceURL
		if c.PagesUploaded { // only the details came from the source
			source, sourceURL = "upload", ""
		}
		out.ChapterList = append(out.ChapterList, apiChapter{
			ID: c.ID, ReaderID: c.ReaderID, Source: source, SourceURL: sourceURL, Title: c.Title, Number: num, Volume: int(vol), Lang: c.Lang,
			Group: scanlator(c), Published: c.Published * 1000, Pages: len(c.Files),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// scanlator is the line Kotatsu shows under a chapter, its only place to
// say that a chapter is official. "Official" goes first, so it stays visible
// when the line is cut off.
func scanlator(c *Chapter) string {
	switch {
	case c.Official && c.Group != "":
		return "Official (" + c.Group + ")"
	case c.Official:
		return "Official"
	}
	return c.Group
}

func (s *Server) apiPages(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := r.PathValue("id")
	var c *Chapter
	var err error
	if value, ok := strings.CutPrefix(id, "reader:"); ok {
		var readerID int64
		readerID, err = strconv.ParseInt(value, 10, 64)
		if err == nil {
			c, err = s.db.primaryChapter(readerID)
		}
	} else {
		// Old connector versions and previously cached chapter URLs.
		c, err = s.db.chapter(id)
	}
	if err != nil || c.State != stateDone {
		apiError(w, http.StatusNotFound, "Not downloaded")
		return
	}
	out := make([]string, 0, len(c.Files))
	for _, f := range c.Files {
		out = append(out, s.fileURL(u, c.MangaID+"/"+c.ID+"/"+f))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.db.allTags()
	if err != nil {
		apiError(w, http.StatusInternalServerError, userMessage(err))
		return
	}
	writeJSON(w, http.StatusOK, nonNil(tags))
}
