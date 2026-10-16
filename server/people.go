package main

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Profiles, the full pages behind their "Show all" links, and the activity
// feed. The profile shows the same amount for everyone, however much they
// read, and the full pages go 25 rows at a time.

const (
	profileReading = 6
	profileShelf   = 8
	profileLog     = 5
	profileFollows = 8
	rowsPerPage    = 25
	feedDays       = 14
	feedItems      = 100
	mostReadCount  = 5
)

// Pager is the Previous and Next links under a long table.
type Pager struct {
	From, To, Total int
	Prev, Next      string
}

// pager works out the page asked for in ?page= and the links around it,
// keeping the rest of the query.
func pager(r *http.Request, total int) (Pager, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * rowsPerPage
	p := Pager{From: offset + 1, To: min(offset+rowsPerPage, total), Total: total}
	link := func(n int) string {
		q := r.URL.Query()
		if n == 1 {
			q.Del("page")
		} else {
			q.Set("page", strconv.Itoa(n))
		}
		if len(q) == 0 {
			return r.URL.Path
		}
		return r.URL.Path + "?" + q.Encode()
	}
	if page > 1 {
		p.Prev = link(page - 1)
	}
	if offset+rowsPerPage < total {
		p.Next = link(page + 1)
	}
	return p, offset
}

func (s *Server) personFrom(w http.ResponseWriter, r *http.Request) (*Person, bool) {
	p, err := s.db.person(r.PathValue("name"))
	if err != nil {
		s.notFound(w, r)
		return nil, false
	}
	return p, true
}

func peoplePath(p *Person) string { return "/people/" + url.PathEscape(p.Name) }

type followRow struct {
	*Manga
	Thumb string
}

func (s *Server) followRows(u *User, owner int64, limit int) ([]followRow, int) {
	list, _ := s.db.library(LibraryQuery{UserID: owner, Followed: true, Sort: "title", Limit: limit})
	var total int
	s.db.QueryRow(`SELECT count(*) FROM follows WHERE user_id=?`, owner).Scan(&total)
	rows := make([]followRow, 0, len(list))
	for _, m := range list {
		thumb := ""
		if m.Cover != "" {
			thumb = s.coverURL(u, m, thumbPath(m))
		}
		rows = append(rows, followRow{m, thumb})
	}
	return rows, total
}

func (s *Server) profilePage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	p, ok := s.personFrom(w, r)
	if !ok {
		return
	}
	data := map[string]any{"Person": p, "Me": u.ID == p.ID, "Shows": p.Shows(u), "Path": peoplePath(p)}
	if !p.Shows(u) {
		follows, total := s.followRows(u, p.ID, 0)
		data["Follows"], data["FollowCount"] = follows, total
		s.render(w, r, "profile", pageData{Title: p.Name, Nav: "activity", Data: data})
		return
	}
	now := time.Now()
	ci := newChapterIndex(s.db)
	yearAgo := now.AddDate(0, 0, -371).UnixMilli()
	year, _ := s.db.readSessions(u.ID, p.ID, yearAgo, "")
	weekAgo := now.AddDate(0, 0, -7).UnixMilli()
	var week []*ReadSession
	for _, rs := range year {
		if rs.End >= weekAgo {
			week = append(week, rs)
		}
	}
	data["WeekChapters"], data["WeekTime"] = chaptersRead(week), minutes(readingTime(week))
	data["Heatmap"] = heatmap(year, now)
	log := year[:min(profileLog, len(year))]
	s.fillSessions(u, ci, log)
	data["Log"], data["LogMore"] = log, len(year) > profileLog || s.db.readBefore(u.ID, p.ID, yearAgo)
	unfinished := false
	reading, readingCount, _ := s.db.progress(PlaceQuery{Viewer: u.ID, Owner: p.ID, Finished: &unfinished, Limit: profileReading})
	s.fillProgress(u, ci, reading)
	finished := true
	_, finishedCount, _ := s.db.progress(PlaceQuery{Viewer: u.ID, Owner: p.ID, Finished: &finished, CountOnly: true})
	data["Reading"], data["ReadingCount"], data["FinishedCount"] = reading, readingCount, finishedCount
	lists, _ := s.db.lists(u.ID, p.ID)
	for _, l := range lists {
		l.Entries, _, _ = s.db.listEntries(ListQuery{Viewer: u.ID, Owner: p.ID, List: l.ID, Order: l.Order, Limit: profileShelf})
		s.fillEntries(u, ci, l.Entries)
	}
	data["Lists"] = lists
	data["Follows"], data["FollowCount"] = s.followRows(u, p.ID, profileFollows)
	s.render(w, r, "profile", pageData{Title: p.Name, Nav: "activity", Data: data})
}

// readingPage is everything someone has started or finished.
func (s *Server) readingPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	p, ok := s.personFrom(w, r)
	if !ok {
		return
	}
	if !p.Shows(u) {
		http.Redirect(w, r, peoplePath(p), http.StatusSeeOther)
		return
	}
	q := r.URL.Query()
	tab := q.Get("tab")
	finished := tab == "finished"
	query := PlaceQuery{Viewer: u.ID, Owner: p.ID, Finished: &finished, Query: strings.TrimSpace(q.Get("q")), Sort: q.Get("sort")}
	notFinished := false
	yes := true
	_, inProgress, _ := s.db.progress(PlaceQuery{Viewer: u.ID, Owner: p.ID, Finished: &notFinished, CountOnly: true})
	_, done, _ := s.db.progress(PlaceQuery{Viewer: u.ID, Owner: p.ID, Finished: &yes, CountOnly: true})
	query.CountOnly = true
	_, total, _ := s.db.progress(query)
	pg, offset := pager(r, total)
	query.CountOnly, query.Offset, query.Limit = false, offset, rowsPerPage
	rows, _, err := s.db.progress(query)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	s.fillProgress(u, newChapterIndex(s.db), rows)
	s.render(w, r, "reading", pageData{Title: p.Name + " / Reading", Nav: "activity", Data: map[string]any{
		"Person": p, "Path": peoplePath(p), "Tab": tab, "Rows": rows, "Pager": pg, "Query": query.Query, "Sort": query.Sort,
		"InProgress": inProgress, "Finished": done,
	}})
}

// listPage is one of someone's favorites categories, with tabs for the rest.
func (s *Server) listPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	p, ok := s.personFrom(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	lists, _ := s.db.lists(u.ID, p.ID)
	var list *List
	for _, l := range lists {
		if l.ID == id {
			list = l
		}
	}
	if list == nil || !p.Shows(u) {
		s.notFound(w, r)
		return
	}
	q := r.URL.Query()
	query := ListQuery{Viewer: u.ID, Owner: p.ID, List: list.ID, Order: list.Order, Sort: q.Get("sort"), Query: strings.TrimSpace(q.Get("q"))}
	_, total, _ := s.db.listEntries(ListQuery{Viewer: u.ID, Owner: p.ID, List: list.ID, Query: query.Query, Limit: 1})
	pg, offset := pager(r, total)
	query.Offset, query.Limit = offset, rowsPerPage
	rows, _, err := s.db.listEntries(query)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	s.fillEntries(u, newChapterIndex(s.db), rows)
	s.render(w, r, "list", pageData{Title: p.Name + " / " + list.Title, Nav: "activity", Data: map[string]any{
		"Person": p, "Path": peoplePath(p), "Lists": lists, "List": list, "Rows": rows, "Pager": pg,
		"Query": query.Query, "Sort": query.Sort,
	}})
}

// logPage is someone's reading sessions, newest first.
func (s *Server) logPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	p, ok := s.personFrom(w, r)
	if !ok {
		return
	}
	if !p.Shows(u) {
		http.Redirect(w, r, peoplePath(p), http.StatusSeeOther)
		return
	}
	all, err := s.db.readSessions(u.ID, p.ID, 0, "")
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	pg, offset := pager(r, len(all))
	rows := all[min(offset, len(all)):min(offset+rowsPerPage, len(all))]
	s.fillSessions(u, newChapterIndex(s.db), rows)
	s.render(w, r, "log", pageData{Title: p.Name + " / Reading log", Nav: "activity", Data: map[string]any{
		"Person": p, "Path": peoplePath(p), "Rows": rows, "Pager": pg,
	}})
}

func (s *Server) followingPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	p, ok := s.personFrom(w, r)
	if !ok {
		return
	}
	rows, total := s.followRows(u, p.ID, 0)
	s.render(w, r, "following", pageData{Title: p.Name + " / Following", Nav: "activity", Data: map[string]any{
		"Person": p, "Path": peoplePath(p), "Rows": rows, "Count": total,
	}})
}

// activityPage is what everyone on the server read, listed, and followed
// lately, plus the manga read most this week.
func (s *Server) activityPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	mine := r.URL.Query().Get("show") == "mine"
	var owner int64
	if mine {
		owner = u.ID
	}
	now := time.Now()
	since := now.AddDate(0, 0, -feedDays).UnixMilli()
	ci := newChapterIndex(s.db)
	sessions, err := s.db.readSessions(u.ID, owner, since, "")
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	var items []*FeedItem
	for _, rs := range sessions {
		items = append(items, &FeedItem{SyncedManga: rs.SyncedManga, UserName: rs.UserName, At: rs.End, Time: rs.Duration(), Kind: "read", What: rs.What(), session: rs})
	}
	events, _ := s.db.listEvents(u.ID, owner, since)
	items = append(items, listFeed(events)...)
	follows, _ := s.db.recentFollows(owner, since)
	items = append(items, follows...)
	days := groupDays(items)
	// Only the newest items, and only then are chapter names looked up.
	n := 0
	for i := range days {
		if n+len(days[i].Items) > feedItems {
			if n == feedItems {
				days = days[:i]
			} else {
				days[i].Items = days[i].Items[:feedItems-n]
				days = days[:i+1]
			}
			break
		}
		n += len(days[i].Items)
	}
	for _, d := range days {
		for _, it := range d.Items {
			if it.session != nil {
				s.fillSessions(u, ci, []*ReadSession{it.session})
				it.SyncedManga = it.session.SyncedManga
				it.What = it.session.What()
			} else if it.Kind == "follow" {
				if l := ci.local(it.URL); l != nil {
					it.Local, it.Title = l, l.Title
					if l.Cover != "" {
						it.Cover = s.coverURL(u, l, thumbPath(l))
					}
				}
			} else {
				s.fill(u, ci, &it.SyncedManga)
			}
		}
	}
	weekAgo := now.AddDate(0, 0, -7).UnixMilli()
	var week []*ReadSession
	for _, rs := range sessions {
		if rs.End >= weekAgo {
			week = append(week, rs)
		}
	}
	if mine {
		// The ranking is the whole server's either way.
		week, _ = s.db.readSessions(u.ID, 0, weekAgo, "")
	}
	top := mostRead(week, mostReadCount)
	for _, m := range top {
		s.fill(u, ci, &m.SyncedManga)
	}
	s.render(w, r, "activity", pageData{Title: "Activity", Nav: "activity", Data: map[string]any{
		"Days": days, "Mine": mine, "MostRead": top,
	}})
}

func (s *Server) settingsSharing(w http.ResponseWriter, r *http.Request) {
	if err := s.db.setShare(currentUser(r).ID, r.FormValue("share")); err != nil {
		s.renderSettings(w, r, userMessage(err))
		return
	}
	s.dl.Changed()
	back(w, r, "/settings", "Saved.")
}
