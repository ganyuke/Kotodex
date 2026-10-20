package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// The details of a chapter an administrator can change.
type chapterDetails struct {
	Volume, Number, Title, Lang, Group string
}

func detailsOf(c *Chapter) chapterDetails {
	return chapterDetails{c.Volume, c.Number, c.Title, c.Lang, c.Group}
}

// sourceDetails is what the source last said about an edited chapter, or
// nil when the chapter has no edit.
func (db *DB) sourceDetails(id string) (*chapterDetails, error) {
	var d chapterDetails
	err := db.QueryRow(`SELECT volume,number,title,lang,grp FROM chapter_edits WHERE chapter_id=?`, id).Scan(&d.Volume, &d.Number, &d.Title, &d.Lang, &d.Group)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &d, err
}

// editChapter sets a chapter's details. Uploaded chapters have no source to
// keep them from, so only chapters from a source get an edit row.
func (db *DB) editChapter(c *Chapter, d chapterDetails) error {
	if c.Source != "upload" {
		// Saved first, so a check running meanwhile updates the edit row
		// instead of the edited details.
		if _, err := db.Exec(`INSERT INTO chapter_edits(chapter_id,volume,number,title,lang,grp)
			SELECT id,volume,number,title,lang,grp FROM chapters WHERE id=? ON CONFLICT DO NOTHING`, c.ID); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`UPDATE chapters SET volume=?,number=?,title=?,lang=?,grp=?,volume_key=?,number_key=? WHERE id=?`,
		d.Volume, d.Number, d.Title, d.Lang, d.Group, numberKey(d.Volume), numberKey(d.Number), c.ID); err != nil {
		return err
	}
	// Details that match the source again are no edit.
	if _, err := db.Exec(`DELETE FROM chapter_edits WHERE chapter_id=? AND volume=? AND number=? AND title=? AND lang=? AND grp=?`,
		c.ID, d.Volume, d.Number, d.Title, d.Lang, d.Group); err != nil {
		return err
	}
	return db.rebindReaderChapter(c.ID)
}

// restoreChapter puts back the source's details of an edited chapter.
func (db *DB) restoreChapter(c *Chapter) error {
	d, err := db.sourceDetails(c.ID)
	if err != nil || d == nil {
		return err
	}
	if _, err := db.Exec(`UPDATE chapters SET volume=?,number=?,title=?,lang=?,grp=?,volume_key=?,number_key=? WHERE id=?`,
		d.Volume, d.Number, d.Title, d.Lang, d.Group, numberKey(d.Volume), numberKey(d.Number), c.ID); err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM chapter_edits WHERE chapter_id=?`, c.ID); err != nil {
		return err
	}
	return db.rebindReaderChapter(c.ID)
}

// chapterNext is where the chapter page goes back to: the manga page, with
// the filters it was opened from.
func chapterNext(r *http.Request, c *Chapter) string {
	if next := r.FormValue("next"); localPath(next) && strings.HasPrefix(next, "/manga/"+c.MangaID) {
		return next
	}
	return "/manga/" + c.MangaID
}

func (s *Server) chapterPage(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.chapter(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, err := s.db.manga(c.MangaID, currentUser(r).ID)
	if err != nil {
		s.notFound(w, r)
		return
	}
	original, err := s.db.sourceDetails(c.ID)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	langs := allLangs()
	if _, ok := langNames[c.Lang]; !ok {
		langs = append([]langOption{{c.Lang, c.Lang}}, langs...)
	}
	s.render(w, r, "chapter", pageData{Title: chapterLabel(c) + " - " + m.Title, Nav: "library", Data: map[string]any{
		"Chapter": c, "Manga": m, "Original": original, "Langs": langs, "Next": chapterNext(r, c),
		"AddPages": c.Unavailable && c.State != stateDone,
	}})
}

func (s *Server) chapterEdit(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.chapter(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Invalid form.")
		return
	}
	d := chapterDetails{
		Volume: normalizeChapterNumber(r.FormValue("volume")), Number: normalizeChapterNumber(r.FormValue("number")),
		Title: strings.TrimSpace(r.FormValue("title")), Lang: strings.TrimSpace(r.FormValue("lang")), Group: strings.TrimSpace(r.FormValue("group")),
	}
	if !validChapterMetadata(d.Volume, d.Number, d.Lang) || len(d.Title) > 500 || len(d.Group) > 500 {
		s.errorPage(w, r, http.StatusBadRequest, "Use numeric chapter and volume numbers and a language code such as en.")
		return
	}
	if d != detailsOf(c) {
		if err := s.db.editChapter(c, d); err != nil {
			s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
			return
		}
		s.db.repairChapterPrimaries(c.MangaID)
		s.dl.Changed()
	}
	back(w, r, chapterNext(r, c), "Saved.")
}

func (s *Server) chapterRestore(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.chapter(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.db.restoreChapter(c); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	s.db.repairChapterPrimaries(c.MangaID)
	s.dl.Changed()
	back(w, r, chapterNext(r, c), "Restored "+sourceLabel(c.Source)+" details.")
}
