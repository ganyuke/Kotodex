package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

func urlEscape(s string) string { return url.QueryEscape(s) }

// shownError is an error written for the person using Kotodex, like a
// missing field or a name already in use.
type shownError string

func (e shownError) Error() string { return string(e) }

// userMessage turns an error into something to show on a page. Database and
// file errors go to the log, so their text never reaches the browser.
func userMessage(err error) string {
	var se shownError
	var me *mdxError
	var ue *url.Error
	switch {
	case errors.As(err, &se), errors.As(err, &me), errors.Is(err, errWrongKey):
		return err.Error()
	case errors.As(err, &ue):
		// Only MangaDex requests go out over HTTP.
		log.Printf("MangaDex: %v", err)
		return "Can't reach MangaDex. Try again later."
	}
	log.Printf("error: %v", err)
	return "Something went wrong. Try again."
}

// Login throttling: 10 failed attempts per address, and per account, every
// 15 minutes.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func (l *loginLimiter) recent(ip string) []time.Time {
	cut := time.Now().Add(-15 * time.Minute)
	var keep []time.Time
	for _, t := range l.fails[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = keep
	}
	return keep
}

func (l *loginLimiter) allow(keys []string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if len(l.recent(k)) >= 10 {
			return false
		}
	}
	return true
}

func (l *loginLimiter) fail(keys []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Guesses at many made-up names would otherwise pile up here.
	if len(l.fails) > 10000 {
		for k := range l.fails {
			l.recent(k)
		}
	}
	for _, k := range keys {
		l.fails[k] = append(l.recent(k), time.Now())
	}
}

// clientIP trusts X-Forwarded-For only from a reverse proxy on a private
// address, which covers Docker networks and a proxy on the same host. It
// takes the last entry, the one that proxy added. Earlier entries come from
// the client and can be made up.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			parts := strings.Split(fwd, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	return host
}

// loginKeys are what failed sign-ins are counted against: the address, and
// the account, so spreading guesses over many addresses doesn't help.
func loginKeys(r *http.Request, username string) []string {
	return []string{"ip:" + clientIP(r), "user:" + truncate(strings.ToLower(strings.TrimSpace(username)), 100)}
}

func serverAddress(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

var langNames = map[string]string{
	"en": "English", "ja": "Japanese", "ja-ro": "Japanese (romanized)", "ko": "Korean", "zh": "Chinese (simplified)",
	"zh-hk": "Chinese (traditional)", "es": "Spanish", "es-la": "Spanish (Latin America)", "fr": "French",
	"de": "German", "it": "Italian", "pt": "Portuguese", "pt-br": "Portuguese (Brazil)", "ru": "Russian",
	"uk": "Ukrainian", "pl": "Polish", "tr": "Turkish", "vi": "Vietnamese", "id": "Indonesian", "th": "Thai",
	"ar": "Arabic", "fa": "Persian", "he": "Hebrew", "hu": "Hungarian", "cs": "Czech", "ro": "Romanian",
	"nl": "Dutch", "sv": "Swedish", "fi": "Finnish", "no": "Norwegian", "da": "Danish", "el": "Greek",
	"bg": "Bulgarian", "ca": "Catalan", "ms": "Malay", "tl": "Filipino", "hi": "Hindi", "bn": "Bengali",
	"mn": "Mongolian", "my": "Burmese", "lt": "Lithuanian", "sr": "Serbian", "hr": "Croatian", "ta": "Tamil",
	"ko-ro": "Korean (romanized)", "zh-ro": "Chinese (romanized)", "sk": "Slovak", "et": "Estonian",
	"kk": "Kazakh", "uz": "Uzbek", "ka": "Georgian", "az": "Azerbaijani", "be": "Belarusian", "ne": "Nepali",
	"ur": "Urdu", "la": "Latin", "eo": "Esperanto", "jv": "Javanese", "sl": "Slovenian", "lv": "Latvian",
}

type langOption struct{ Code, Name string }

var langCodeRe = regexp.MustCompile(`^[a-z]{2,3}(-[a-z]{2,3})?$`)

// langCodes keeps the MangaDex language codes from a form, once each. Lists
// are stored comma-separated, so anything else would break them.
func langCodes(in []string) []string {
	var out []string
	for _, l := range in {
		if langCodeRe.MatchString(l) && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// allLangs lists the named MangaDex languages, sorted by name.
func allLangs() []langOption {
	out := make([]langOption, 0, len(langNames))
	for code, name := range langNames {
		out = append(out, langOption{code, name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) loadTemplates() {
	funcs := template.FuncMap{
		"chapterLabel": chapterLabel,
		"lang": func(code string) string {
			if n, ok := langNames[code]; ok {
				return n
			}
			return code
		},
		"date": func(ts int64) string {
			if ts == 0 {
				return ""
			}
			return time.Unix(ts, 0).Format("Jan 2, 2006")
		},
		"ago": func(ts int64) string {
			if ts == 0 {
				return "never"
			}
			return ago(time.Since(time.Unix(ts, 0)))
		},
		"size": humanSize,
		"until": func(ts int64) string {
			if ts == 0 {
				return ""
			}
			d := time.Until(time.Unix(ts, 0))
			if d < time.Minute {
				return "in under a minute"
			}
			return "in " + strings.TrimSuffix(ago(d), " ago")
		},
		"percent": func(done, total int) int {
			if total == 0 {
				return 0
			}
			return done * 100 / total
		},
		"add": func(a, b int) int { return a + b },
		"title": func(s string) string {
			if s == "" {
				return ""
			}
			return strings.ToUpper(s[:1]) + s[1:]
		},
		"join": strings.Join,
	}
	s.pages = map[string]*template.Template{}
	names, _ := fs.Glob(templateFS, "templates/*.html")
	for _, n := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		s.pages[name] = template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", n))
	}
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	default:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n/1024)
	}
}

type pageData struct {
	Title  string
	Nav    string
	User   *User
	Search string
	Error  string
	Notice string
	Data   any
}

// errorPage shows msg as a page of its own, with the usual header and a way
// back. Plain-text errors would show unstyled, since the CSP blocks the
// browser's own styling of them.
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if liveFragment(r) {
		return // the stream ends and the page reloads into this
	}
	p := pageData{Title: msg, User: currentUser(r)}
	if err := s.pages["error"].Execute(w, p); err != nil {
		log.Printf("render error: %v", err)
	}
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusNotFound, "Page not found")
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p pageData) {
	p.User = currentUser(r)
	if p.Notice == "" {
		p.Notice = takeNotice(w, r)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if liveFragment(r) {
		if err := s.pages[name].ExecuteTemplate(w, "live", p); err != nil {
			log.Printf("render %s live: %v", name, err)
		}
		return
	}
	if err := s.pages[name].Execute(w, p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// localPath accepts only paths on this site. Browsers read "/\x" like
// "//x", another site, so backslashes are refused too. They also drop tabs
// and newlines from URLs ("/\t/x" becomes "//x"), which url.Parse rejects.
func localPath(p string) bool {
	if _, err := url.Parse(p); err != nil {
		return false
	}
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.Contains(p, "\\")
}

// back redirects after a form post. The notice goes in a one-time cookie
// rather than the URL, so it shows once and can't be forged with a link.
func back(w http.ResponseWriter, r *http.Request, fallback, notice string) {
	next := r.FormValue("next")
	if !localPath(next) {
		next = fallback
	}
	if notice != "" {
		// Only the page redirected to gets the cookie and shows it.
		path := "/"
		if u, err := url.Parse(next); err == nil && u.Path != "" {
			path = u.EscapedPath()
		}
		value := url.Values{"path": {path}, "text": {notice}}.Encode()
		setCookie(w, r, &http.Cookie{Name: noticeCookie, Value: value, Path: path, MaxAge: 60})
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

const noticeCookie = "notice"

// takeNotice reads the notice back left for this page and clears it.
func takeNotice(w http.ResponseWriter, r *http.Request) string {
	if r.Method != http.MethodGet || liveFragment(r) {
		return ""
	}
	c, err := r.Cookie(noticeCookie)
	if err != nil {
		return ""
	}
	v, err := url.ParseQuery(c.Value)
	if err != nil || v.Get("path") != r.URL.EscapedPath() {
		return ""
	}
	setCookie(w, r, &http.Cookie{Name: noticeCookie, Path: v.Get("path"), MaxAge: -1})
	return v.Get("text")
}

// Sign-in

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.sessionUser(sessionToken(r)); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p := pageData{Title: "Sign in"}
	if r.Method == http.MethodPost {
		keys := loginKeys(r, r.FormValue("username"))
		if !s.limiter.allow(keys) {
			p.Error = "Too many attempts. Try again later."
		} else if u, err := s.db.checkPassword(r.FormValue("username"), r.FormValue("password")); err != nil {
			s.limiter.fail(keys)
			p.Error = "Wrong username or password."
		} else {
			token, err := s.db.createSession(u.ID, "browser", r.UserAgent())
			if err != nil {
				s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
				return
			}
			setSessionCookie(w, r, token)
			back(w, r, "/", "")
			return
		}
	}
	p.Data = r.FormValue("next")
	s.render(w, r, "login", p)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.db.deleteSession(sessionToken(r))
	setCookie(w, r, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Library

func (s *Server) libraryPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	show := r.URL.Query().Get("show")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	list, err := s.db.library(LibraryQuery{UserID: u.ID, Query: q, Sort: r.URL.Query().Get("sort"), Followed: show != "all"})
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	type row struct {
		*Manga
		Thumb string
	}
	rows := make([]row, 0, len(list))
	for _, m := range list {
		thumb := ""
		if m.Cover != "" {
			thumb = s.coverURL(u, m, thumbPath(m))
		}
		rows = append(rows, row{m, thumb})
	}
	s.render(w, r, "library", pageData{Title: "Library", Nav: "library", Data: map[string]any{
		"Rows": rows, "Show": show, "Query": q, "Sort": r.URL.Query().Get("sort"),
	}})
}

func (s *Server) searchPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	p := pageData{Title: "Search MangaDex", Nav: "search", Search: q}
	type row struct {
		*Manga
		Following bool
	}
	var rows []row
	if q != "" {
		// A pasted MangaDex link or ID goes straight to that title.
		if id := mangaIDFrom(q); id != "" {
			if err := s.addManga(r, id); err != nil {
				p.Error = userMessage(err)
			} else {
				http.Redirect(w, r, "/manga/"+id, http.StatusSeeOther)
				return
			}
		}
		list, err := s.mdx.search(r.Context(), q, s.db.settings().ContentRatings)
		if err != nil {
			p.Error = userMessage(err)
		}
		for _, m := range list {
			var following bool
			s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM follows WHERE user_id=? AND manga_id=?)`, u.ID, m.ID).Scan(&following)
			if m.Cover != "" {
				m.Cover = fmt.Sprintf("%s/covers/%s/%s.256.jpg", mdxUploads, m.ID, m.Cover)
			}
			rows = append(rows, row{m, following})
		}
	}
	p.Data = map[string]any{"Rows": rows, "Query": q}
	s.render(w, r, "search", p)
}

func mangaIDFrom(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "/title/"); i >= 0 {
		s = s[i+len("/title/"):]
		s, _, _ = strings.Cut(s, "/")
	}
	if len(s) != 36 {
		return ""
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return ""
	}
	return id.String()
}

// addManga makes sure a MangaDex title is in the database.
func (s *Server) addManga(r *http.Request, id string) error {
	var exists bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM manga WHERE id=?)`, id).Scan(&exists)
	if exists {
		return nil
	}
	m, err := s.mdx.manga(r.Context(), id)
	if err != nil {
		return err
	}
	if err := s.db.upsertManga(m); err != nil {
		return err
	}
	return s.dl.ensureCover(r.Context(), m)
}

func (s *Server) followAction(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := mangaIDFrom(r.FormValue("id"))
	if id == "" {
		s.errorPage(w, r, http.StatusBadRequest, "Bad manga ID")
		return
	}
	if err := s.addManga(r, id); err != nil {
		s.errorPage(w, r, http.StatusBadGateway, userMessage(err))
		return
	}
	if err := s.db.setFollow(u.ID, id, true); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	s.dl.Check(id)
	back(w, r, "/manga/"+id, "Following.")
}

func (s *Server) unfollowAction(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := mangaIDFrom(r.FormValue("id"))
	if id == "" {
		s.errorPage(w, r, http.StatusBadRequest, "Bad manga ID")
		return
	}
	s.db.setFollow(u.ID, id, false)
	back(w, r, "/manga/"+id, "Unfollowed.")
}

// followReset drops the user's follow choice so it matches MangaDex again.
func (s *Server) followReset(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := r.PathValue("id")
	s.db.resetFollow(u.ID, id)
	s.dl.Check(id)
	back(w, r, "/manga/"+id, "Matching MangaDex.")
}

// Manga

func (s *Server) mangaPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	m, err := s.db.manga(r.PathValue("id"), u.ID)
	if err != nil {
		s.notFound(w, r)
		return
	}
	all, _ := s.db.chapters(m.ID)
	var size int64
	filter := r.URL.Query().Get("lang")
	var chapters []*Chapter
	for _, c := range all {
		size += c.Size
		if filter == "" || c.Lang == filter {
			chapters = append(chapters, c)
		}
	}
	langs := s.db.mangaLangs(m.ID)
	// Offer the follower's languages even if MangaDex has none of them yet.
	followLangs := map[string]bool{}
	langsCustom := false
	if m.Following {
		var list []string
		list, langsCustom = s.db.followLangs(u.ID, m.ID)
		for _, l := range list {
			followLangs[l] = true
		}
	}
	var linked bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM mdx_accounts WHERE user_id=?)`, u.ID).Scan(&linked)
	langChoices := []LangCount{}
	seen := map[string]bool{}
	for _, l := range langs {
		seen[l.Code] = true
		langChoices = append(langChoices, l)
	}
	for _, l := range slices.Sorted(maps.Keys(followLangs)) {
		if !seen[l] {
			langChoices = append(langChoices, LangCount{Code: l})
		}
	}
	// Newest first on the page.
	for i, j := 0, len(chapters)-1; i < j; i, j = i+1, j-1 {
		chapters[i], chapters[j] = chapters[j], chapters[i]
	}
	cover := ""
	if m.Cover != "" {
		cover = s.coverURL(u, m, thumbPath(m))
	}
	st := s.dl.Status()
	var current *Progress
	if st.Current != nil && st.Current.MangaID == m.ID {
		current = st.Current
	}
	s.render(w, r, "manga", pageData{Title: m.Title, Nav: "library", Data: map[string]any{
		"Manga": m, "Chapters": chapters, "Cover": cover, "Size": size, "Current": current,
		"Langs": langs, "LangChoices": langChoices, "FollowLangs": followLangs, "Filter": filter,
		"LangsCustom": langsCustom, "Linked": linked, "OnMangaDex": s.db.onMangaDex(u.ID, m.ID),
		"FollowCustom": linked && s.db.followChoice(u.ID, m.ID),
	}})
}

func (s *Server) mangaCheck(w http.ResponseWriter, r *http.Request) {
	s.dl.Check(r.PathValue("id"))
	back(w, r, "/manga/"+r.PathValue("id"), "Checking for new chapters.")
}

func (s *Server) mangaDownloadAll(w http.ResponseWriter, r *http.Request) {
	// One upload per chapter number and language, the earliest one, same as
	// for new chapters. Other groups' uploads can still be picked by hand.
	id, lang := r.PathValue("id"), r.FormValue("lang")
	chapters, err := s.db.queryChapters(`SELECT `+chapterCols+` FROM chapters c WHERE c.manga_id=? AND c.state IN (0,3) AND (?='' OR c.lang=?) ORDER BY c.published`, id, lang, lang)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	n := 0
	for _, c := range chapters {
		if s.db.hasChapterNumber(c.MangaID, c.Lang, c.Volume, c.Number) {
			continue
		}
		s.db.setChapterState(c.ID, stateQueued)
		n++
	}
	s.dl.Wake()
	back(w, r, "/manga/"+id, plural(n, "chapter")+" added to downloads.")
}

// mangaLanguages sets which languages this user wants new chapters in for
// one manga. It only changes what downloads from now on.
func (s *Server) mangaLanguages(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := r.PathValue("id")
	r.ParseForm()
	langs := langCodes(r.Form["lang"])
	if len(langs) == 0 {
		back(w, r, "/manga/"+id, "Pick at least one language, or unfollow.")
		return
	}
	s.db.setFollowLangs(u.ID, id, langs)
	back(w, r, "/manga/"+id, "Saved.")
}

func (s *Server) mangaLanguagesReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.db.Exec(`UPDATE follows SET langs='' WHERE user_id=? AND manga_id=?`, currentUser(r).ID, id)
	back(w, r, "/manga/"+id, "Using your default languages.")
}

func (s *Server) mangaDeleteFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	chapters, _ := s.db.chapters(id)
	n := 0
	for _, c := range chapters {
		if c.State == stateSkipped && c.Size == 0 {
			continue
		}
		if err := s.dl.deleteChapterFiles(c); err != nil {
			s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
			return
		}
		n++
	}
	back(w, r, "/manga/"+id, plural(n, "chapter")+" deleted.")
}

func (s *Server) chapterDownload(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.chapter(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if c.State != stateDone {
		s.db.setChapterState(c.ID, stateQueued)
		s.dl.Wake()
	}
	back(w, r, "/manga/"+c.MangaID, "")
}

func (s *Server) chapterCancel(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.chapter(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if c.State == stateQueued || c.State == stateFailed {
		s.db.setChapterState(c.ID, stateSkipped)
		s.dl.Changed()
	}
	back(w, r, "/manga/"+c.MangaID, "")
}

func (s *Server) chapterDelete(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.chapter(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.dl.deleteChapterFiles(c); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	back(w, r, "/manga/"+c.MangaID, "")
}

// Downloads

const historyPage = 50

func (s *Server) downloadsPage(w http.ResponseWriter, r *http.Request) {
	queue, err := s.db.queue()
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	var queued, failed int
	for _, c := range queue {
		if c.State == stateFailed {
			failed++
		} else {
			queued++
		}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	history, err := s.db.history(historyPage, (page-1)*historyPage)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	var total int64
	var count int
	s.db.QueryRow(`SELECT coalesce(sum(size),0), count(*) FROM chapters WHERE state=2`).Scan(&total, &count)
	st := s.dl.Status()
	s.render(w, r, "downloads", pageData{Title: "Downloads", Nav: "downloads", Data: map[string]any{
		"Queue": queue, "Queued": queued, "Failed": failed, "Status": st,
		"LastCheck": unixOrZero(st.LastCheck), "NextCheck": unixOrZero(st.NextCheck),
		"TotalSize": total, "TotalChapters": count, "History": history, "Page": page,
		"HasOlder": page*historyPage < count,
	}})
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s *Server) downloadsCheck(w http.ResponseWriter, r *http.Request) {
	s.dl.Check("")
	back(w, r, "/downloads", "Checking all followed manga for new chapters.")
}

func (s *Server) downloadsRetry(w http.ResponseWriter, r *http.Request) {
	res, _ := s.db.Exec(`UPDATE chapters SET state=1, tries=0, error='' WHERE state=3`)
	n, _ := res.RowsAffected()
	s.dl.Wake()
	back(w, r, "/downloads", plural(int(n), "chapter")+" added back to downloads.")
}

// Settings

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, "")
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, errMsg string) {
	u := currentUser(r)
	sessions, _ := s.db.sessions(u.ID, sessionToken(r))
	var mdx struct {
		Linked   bool
		Username string
		Synced   int64
		Error    string
		Follows  int
	}
	if s.db.QueryRow(`SELECT username,synced,error FROM mdx_accounts WHERE user_id=?`, u.ID).Scan(&mdx.Username, &mdx.Synced, &mdx.Error) == nil {
		mdx.Linked = true
		s.db.QueryRow(`SELECT count(*) FROM mdx_follows WHERE user_id=?`, u.ID).Scan(&mdx.Follows)
	}
	myLangs := map[string]bool{}
	for _, l := range s.db.userLangs(u.ID) {
		myLangs[l] = true
	}
	data := map[string]any{
		"Sessions": sessions, "MangaDex": mdx, "Address": serverAddress(r),
		"AllLangs": allLangs(), "MyLangs": myLangs,
	}
	if u.Admin {
		users, _ := s.db.users()
		st := s.db.settings()
		data["Users"] = users
		data["Interval"] = int(st.Interval.Minutes())
		data["DataSaver"] = st.DataSaver
		ratings := map[string]bool{}
		for _, r := range st.ContentRatings {
			ratings[r] = true
		}
		data["Ratings"] = ratings
	}
	s.render(w, r, "settings", pageData{Title: "Settings", Nav: "settings", Error: errMsg, Data: data})
}

func (s *Server) settingsPassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if _, err := s.db.checkPassword(u.Name, r.FormValue("current")); err != nil {
		s.renderSettings(w, r, "The current password is wrong.")
		return
	}
	if r.FormValue("new") != r.FormValue("confirm") {
		s.renderSettings(w, r, "The new passwords don't match.")
		return
	}
	if err := s.db.setPassword(u.ID, r.FormValue("new")); err != nil {
		s.renderSettings(w, r, userMessage(err))
		return
	}
	// Changing the password signs out every device, including this one.
	token, _ := s.db.createSession(u.ID, "browser", r.UserAgent())
	setSessionCookie(w, r, token)
	back(w, r, "/settings", "Password changed. Other devices are signed out.")
}

func (s *Server) settingsSignOut(w http.ResponseWriter, r *http.Request) {
	s.db.signOutDevice(currentUser(r).ID, r.FormValue("hash"))
	back(w, r, "/settings", "Signed out.")
}

func (s *Server) settingsMangaDex(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	user, pass := strings.TrimSpace(r.FormValue("username")), r.FormValue("password")
	clientID, secret := strings.TrimSpace(r.FormValue("client_id")), strings.TrimSpace(r.FormValue("client_secret"))
	tok, err := s.mdx.login(r.Context(), user, pass, clientID, secret)
	if err != nil {
		s.renderSettings(w, r, userMessage(err))
		return
	}
	_, err = s.db.Exec(`INSERT INTO mdx_accounts(user_id,username,client_id,client_secret,refresh) VALUES(?,?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET username=excluded.username, client_id=excluded.client_id,
		client_secret=excluded.client_secret, refresh=excluded.refresh, error=''`, u.ID, user, clientID, s.keys.seal(secret), s.keys.seal(tok.Refresh))
	if err != nil {
		s.renderSettings(w, r, userMessage(err))
		return
	}
	s.dl.Check("")
	back(w, r, "/settings", "MangaDex account linked. Importing follows.")
}

func (s *Server) settingsMangaDexUnlink(w http.ResponseWriter, r *http.Request) {
	if err := s.db.unlinkMangaDex(currentUser(r).ID, r.FormValue("keep") == "1"); err != nil {
		s.renderSettings(w, r, userMessage(err))
		return
	}
	back(w, r, "/settings", "MangaDex account unlinked.")
}

func (s *Server) settingsServer(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	interval, err := strconv.Atoi(r.FormValue("interval"))
	if err != nil || interval < 10 {
		s.renderSettings(w, r, "Check every 10 minutes or more.")
		return
	}
	ratings := r.Form["rating"]
	if len(ratings) == 0 {
		ratings = []string{"safe"}
	}
	dataSaver := "0"
	if r.FormValue("data_saver") == "1" {
		dataSaver = "1"
	}
	s.db.setSetting("interval", strconv.Itoa(interval))
	s.db.setSetting("data_saver", dataSaver)
	s.db.setSetting("content_ratings", strings.Join(ratings, ","))
	s.dl.Reschedule()
	back(w, r, "/settings", "Saved.")
}

func (s *Server) settingsLanguages(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	langs := langCodes(r.Form["lang"])
	if len(langs) == 0 {
		s.renderSettings(w, r, "Pick at least one language.")
		return
	}
	s.db.Exec(`UPDATE users SET langs=? WHERE id=?`, strings.Join(langs, ","), currentUser(r).ID)
	back(w, r, "/settings", "Saved.")
}

func (s *Server) settingsAddUser(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.createUser(r.FormValue("username"), r.FormValue("password"), r.FormValue("admin") == "1"); err != nil {
		s.renderSettings(w, r, userMessage(err))
		return
	}
	back(w, r, "/settings", "User added.")
}

func (s *Server) settingsResetPassword(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.db.setPassword(id, r.FormValue("password")); err != nil {
		s.renderSettings(w, r, userMessage(err))
		return
	}
	if id == currentUser(r).ID {
		token, _ := s.db.createSession(id, "browser", r.UserAgent())
		setSessionCookie(w, r, token)
	}
	back(w, r, "/settings", "Password changed.")
}

func (s *Server) settingsDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id == currentUser(r).ID {
		s.renderSettings(w, r, "You can't delete your own account.")
		return
	}
	deleted, err := s.db.deleteUser(id)
	if err != nil {
		s.renderSettings(w, r, userMessage(err))
		return
	}
	if !deleted {
		s.renderSettings(w, r, "Can't delete the last admin.")
		return
	}
	back(w, r, "/settings", "User deleted.")
}

// downloadingManga reports whether a chapter of the manga page's manga is
// downloading.
func (s *Server) downloadingManga(r *http.Request) bool {
	cur := s.dl.Status().Current
	return cur != nil && cur.MangaID == r.PathValue("id")
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /img/{sid}/{sig}/{path...}", s.handleImage)

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginPage)
	mux.HandleFunc("POST /logout", s.web(false, s.logout))
	mux.HandleFunc("GET /{$}", s.web(false, s.libraryPage))
	mux.HandleFunc("GET /search", s.web(false, s.searchPage))
	mux.HandleFunc("POST /follow", s.web(false, s.followAction))
	mux.HandleFunc("POST /unfollow", s.web(false, s.unfollowAction))
	mux.HandleFunc("GET /manga/{id}", s.web(false, s.live(s.mangaPage, s.downloadingManga)))
	mux.HandleFunc("POST /manga/{id}/check", s.web(false, s.mangaCheck))
	mux.HandleFunc("POST /manga/{id}/download-all", s.web(false, s.mangaDownloadAll))
	mux.HandleFunc("POST /manga/{id}/languages", s.web(false, s.mangaLanguages))
	mux.HandleFunc("POST /manga/{id}/languages/reset", s.web(false, s.mangaLanguagesReset))
	mux.HandleFunc("POST /manga/{id}/follow/reset", s.web(false, s.followReset))
	mux.HandleFunc("POST /manga/{id}/delete-files", s.web(true, s.mangaDeleteFiles))
	mux.HandleFunc("POST /chapter/{id}/download", s.web(false, s.chapterDownload))
	mux.HandleFunc("POST /chapter/{id}/cancel", s.web(false, s.chapterCancel))
	mux.HandleFunc("POST /chapter/{id}/delete", s.web(true, s.chapterDelete))
	mux.HandleFunc("GET /downloads", s.web(false, s.live(s.downloadsPage, func(*http.Request) bool { return true })))
	mux.HandleFunc("POST /downloads/check", s.web(false, s.downloadsCheck))
	mux.HandleFunc("POST /downloads/retry", s.web(false, s.downloadsRetry))
	mux.HandleFunc("GET /settings", s.web(false, s.settingsPage))
	mux.HandleFunc("POST /settings/password", s.web(false, s.settingsPassword))
	mux.HandleFunc("POST /settings/sign-out", s.web(false, s.settingsSignOut))
	mux.HandleFunc("POST /settings/mangadex", s.web(false, s.settingsMangaDex))
	mux.HandleFunc("POST /settings/mangadex/unlink", s.web(false, s.settingsMangaDexUnlink))
	mux.HandleFunc("POST /settings/languages", s.web(false, s.settingsLanguages))
	mux.HandleFunc("POST /settings/server", s.web(true, s.settingsServer))
	mux.HandleFunc("POST /settings/users", s.web(true, s.settingsAddUser))
	mux.HandleFunc("POST /settings/users/{id}/password", s.web(true, s.settingsResetPassword))
	mux.HandleFunc("POST /settings/users/{id}/delete", s.web(true, s.settingsDeleteUser))

	mux.HandleFunc("POST /api/v1/login", s.apiLogin)
	mux.HandleFunc("POST /api/v1/logout", s.api(s.apiLogout))
	mux.HandleFunc("GET /api/v1/me", s.api(s.apiMe))
	mux.HandleFunc("GET /api/v1/manga", s.api(s.apiList))
	mux.HandleFunc("GET /api/v1/manga/{id}", s.api(s.apiManga))
	mux.HandleFunc("GET /api/v1/chapters/{id}/pages", s.api(s.apiPages))
	mux.HandleFunc("GET /api/v1/tags", s.api(s.apiTags))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	// Anything else. The header shows when signed in.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if u, err := s.db.sessionUser(sessionToken(r)); err == nil {
			r = r.WithContext(context.WithValue(r.Context(), userKey, u))
		}
		s.notFound(w, r)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
	})
	return mux
}
