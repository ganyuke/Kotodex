package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

const maxUpload = 256 << 20
const maxExpandedUpload = 512 << 20

var chapterDecimal = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

func validChapterMetadata(volume, number, lang string) bool {
	return len(volume) <= 40 && len(number) <= 40 && (volume == "" || chapterDecimal.MatchString(volume)) &&
		(number == "" || chapterDecimal.MatchString(number)) && langCodeRe.MatchString(lang)
}

func numberKey(s string) string {
	if s == "" {
		return ""
	}
	if chapterDecimal.MatchString(s) {
		if n, ok := new(big.Rat).SetString(s); ok {
			return "number:" + n.RatString()
		}
	}
	return "label:" + s
}

func imageExtension(ext string) bool {
	return slices.Contains([]string{".jpg", ".jpeg", ".png", ".webp", ".gif"}, strings.ToLower(ext))
}

// Natural order handles page2 before page10, even without zero padding.
func naturalCompare(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		if a[0] >= '0' && a[0] <= '9' && b[0] >= '0' && b[0] <= '9' {
			i, j := 0, 0
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			x, y := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
			if len(x) < len(y) {
				return -1
			}
			if len(x) > len(y) {
				return 1
			}
			if n := strings.Compare(x, y); n != 0 {
				return n
			}
			a, b = a[i:], b[j:]
		} else {
			if a[0] < b[0] {
				return -1
			}
			if a[0] > b[0] {
				return 1
			}
			a, b = a[1:], b[1:]
		}
	}
	return strings.Compare(a, b)
}

// pageFile is one page an administrator uploaded, from an archive or as a
// file of its own. Only its name is used, for sorting.
type pageFile struct {
	name string
	size int64
	open func() (io.ReadCloser, error)
}

// extractChapter never writes archive paths. Only validated image bytes are
// copied to generated filenames, with independent per-page and total limits.
func extractChapter(z *zip.Reader, dir string) ([]string, int64, error) {
	if len(z.File) > 2000 {
		return nil, 0, shownError("Archive has too many entries.")
	}
	var pages []pageFile
	seen := map[string]bool{}
	for _, f := range z.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if strings.Contains(f.Name, "\\") || strings.HasPrefix(f.Name, "/") || slices.Contains(strings.Split(f.Name, "/"), "..") || f.Mode()&os.ModeSymlink != 0 {
			return nil, 0, shownError("Archive contains an unsafe path.")
		}
		if strings.HasPrefix(f.Name, "__MACOSX/") || strings.HasPrefix(path.Base(f.Name), ".") || !imageExtension(path.Ext(f.Name)) {
			continue
		}
		if seen[f.Name] {
			return nil, 0, shownError("Archive contains duplicate page names.")
		}
		seen[f.Name] = true
		pages = append(pages, pageFile{f.Name, int64(min(f.UncompressedSize64, maxSourcePage+1)), f.Open})
	}
	return savePages(pages, dir, "Archive contains a page that is not a supported image.")
}

// savePages writes pages in filename order as 001.jpg, 002.png, and so on.
func savePages(pages []pageFile, dir, notImage string) ([]string, int64, error) {
	if len(pages) == 0 || len(pages) > 1000 {
		return nil, 0, shownError("Upload must contain between 1 and 1000 image pages.")
	}
	for _, p := range pages {
		if p.size > maxSourcePage {
			return nil, 0, shownError("A page exceeds 32 MiB.")
		}
	}
	slices.SortStableFunc(pages, func(a, b pageFile) int {
		if n := naturalCompare(a.name, b.name); n != 0 {
			return n
		}
		return strings.Compare(a.name, b.name)
	})
	var names []string
	var size int64
	for i, p := range pages {
		r, err := p.open()
		if err != nil {
			return nil, 0, shownError("Could not read a page.")
		}
		data, err := io.ReadAll(io.LimitReader(r, maxSourcePage+1))
		r.Close()
		if err != nil {
			return nil, 0, shownError("Could not read a page.")
		}
		size += int64(len(data))
		if len(data) > maxSourcePage || size > maxExpandedUpload {
			return nil, 0, shownError("Expanded upload exceeds its size limit.")
		}
		ext, ok := coverTypes[http.DetectContentType(data)]
		if !ok {
			return nil, 0, shownError(notImage)
		}
		name := fmt.Sprintf("%03d.%s", i+1, ext)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return nil, 0, err
		}
		names = append(names, name)
	}
	return names, size, nil
}

func (s *Server) mangaUpload(w http.ResponseWriter, r *http.Request) {
	m, err := s.db.manga(r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		s.notFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	err = r.ParseMultipartForm(8 << 20)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Upload a CBZ or ZIP under 256 MiB.")
		return
	}
	c := &Chapter{ID: uuid.NewString(), MangaID: m.ID, Source: "upload", SourceURL: strings.TrimSpace(r.FormValue("source_url")),
		Volume: strings.TrimSpace(r.FormValue("volume")), Number: strings.TrimSpace(r.FormValue("number")),
		Lang: strings.TrimSpace(r.FormValue("lang")), Title: strings.TrimSpace(r.FormValue("title")), Group: strings.TrimSpace(r.FormValue("group"))}
	normalizeChapter(c)
	if !validChapterMetadata(c.Volume, c.Number, c.Lang) || len(c.Title) > 500 || len(c.Group) > 500 || (c.SourceURL != "" && (!webAddress(c.SourceURL) || len(c.SourceURL) > 2000)) {
		s.errorPage(w, r, http.StatusBadRequest, "Use numeric chapter and volume numbers, a language code such as en, and an optional HTTP source URL.")
		return
	}
	if r.FormValue("alternate") != "1" && s.db.hasChapterNumber(m.ID, c.Lang, c.Volume, c.Number) {
		s.errorPage(w, r, http.StatusConflict, "This chapter is already downloaded or queued. Cancel its download first, or explicitly upload an alternate edition.")
		return
	}
	f, header, err := r.FormFile("archive")
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Choose a CBZ or ZIP file.")
		return
	}
	defer f.Close()
	switch strings.ToLower(path.Ext(header.Filename)) {
	case ".cbz", ".zip":
	default:
		s.errorPage(w, r, http.StatusBadRequest, "Choose a CBZ or ZIP file.")
		return
	}
	if header.Size > maxUpload {
		s.errorPage(w, r, http.StatusBadRequest, "Upload a CBZ or ZIP under 256 MiB.")
		return
	}
	z, err := zip.NewReader(f, header.Size)
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Could not read the CBZ or ZIP file.")
		return
	}
	if err := os.MkdirAll(filepath.Join(s.mangaDir, m.ID), 0o755); err != nil {
		s.errorPage(w, r, 500, userMessage(err))
		return
	}
	stage, err := os.MkdirTemp(filepath.Join(s.mangaDir, m.ID), ".upload-")
	if err != nil {
		s.errorPage(w, r, 500, userMessage(err))
		return
	}
	defer os.RemoveAll(stage)
	files, size, err := extractChapter(z, stage)
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, userMessage(err))
		return
	}
	dest := filepath.Join(s.mangaDir, m.ID, c.ID)
	if err := os.Rename(stage, dest); err != nil {
		s.errorPage(w, r, 500, userMessage(err))
		return
	}
	list, _ := json.Marshal(files)
	res, err := s.db.Exec(`INSERT INTO chapters(id,manga_id,volume,number,title,lang,grp,published,pages,state,files,size,downloaded,source,source_url,volume_key,number_key)
		SELECT ?,?,?,?,?,?,?,?,?,2,?,?,?,'upload',?,?,? WHERE ? OR ?='' OR NOT EXISTS(
			SELECT 1 FROM chapters WHERE manga_id=? AND lang=? AND state IN (1,2) AND number_key=?
			AND (volume_key=? OR volume_key='' OR ?=''))`,
		c.ID, m.ID, c.Volume, c.Number, c.Title, c.Lang, c.Group, now(), len(files), string(list), size, now(), c.SourceURL, numberKey(c.Volume), numberKey(c.Number),
		r.FormValue("alternate") == "1", numberKey(c.Number), m.ID, c.Lang, numberKey(c.Number), numberKey(c.Volume), numberKey(c.Volume))
	if err != nil {
		os.RemoveAll(dest)
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		os.RemoveAll(dest)
		s.errorPage(w, r, http.StatusConflict, "This chapter was downloaded or queued while the upload was being processed.")
		return
	}
	if err := s.db.bindReaderChapter(c.ID, false); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	if err := s.db.repairChapterPrimaries(m.ID); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	s.db.Exec(`UPDATE manga SET latest=? WHERE id=?`, now(), m.ID)
	s.dl.Changed()
	back(w, r, "/manga/"+m.ID, "Chapter uploaded.")
}

// chapterPages adds pages to a chapter the source lists but has no pages
// for, such as one taken down from MangaDex. The chapter keeps its details
// and its link to the source. The pages are images, or one CBZ or ZIP.
func (s *Server) chapterPages(w http.ResponseWriter, r *http.Request) {
	c, err := s.db.chapter(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if !c.Unavailable || c.State == stateDone {
		s.errorPage(w, r, http.StatusConflict, "This chapter already has pages.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxExpandedUpload+1<<20)
	err = r.ParseMultipartForm(8 << 20)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Upload pages under 512 MiB in total.")
		return
	}
	var pages []pageFile
	var archive *multipart.FileHeader
	for _, f := range r.MultipartForm.File["pages"] {
		// Folders bring along files the system made.
		name := path.Base(f.Filename)
		if strings.HasPrefix(name, ".") || strings.EqualFold(name, "thumbs.db") || strings.EqualFold(name, "desktop.ini") {
			continue
		}
		switch ext := strings.ToLower(path.Ext(name)); {
		case ext == ".cbz" || ext == ".zip":
			archive = f
		case imageExtension(ext):
			pages = append(pages, pageFile{name, f.Size, func() (io.ReadCloser, error) { return f.Open() }})
		default:
			s.errorPage(w, r, http.StatusBadRequest, "Choose JPEG, PNG, WebP, or GIF pages, or one CBZ or ZIP.")
			return
		}
	}
	if archive != nil && (len(pages) > 0 || len(r.MultipartForm.File["pages"]) > 1) || archive == nil && len(pages) == 0 {
		s.errorPage(w, r, http.StatusBadRequest, "Choose JPEG, PNG, WebP, or GIF pages, or one CBZ or ZIP.")
		return
	}
	if err := os.MkdirAll(filepath.Join(s.mangaDir, c.MangaID), 0o755); err != nil {
		s.errorPage(w, r, 500, userMessage(err))
		return
	}
	stage, err := os.MkdirTemp(filepath.Join(s.mangaDir, c.MangaID), ".upload-")
	if err != nil {
		s.errorPage(w, r, 500, userMessage(err))
		return
	}
	defer os.RemoveAll(stage)
	var files []string
	var size int64
	if archive != nil {
		if archive.Size > maxUpload {
			s.errorPage(w, r, http.StatusBadRequest, "Upload a CBZ or ZIP under 256 MiB.")
			return
		}
		f, err := archive.Open()
		if err != nil {
			s.errorPage(w, r, http.StatusBadRequest, "Could not read the CBZ or ZIP file.")
			return
		}
		defer f.Close()
		z, err := zip.NewReader(f, archive.Size)
		if err != nil {
			s.errorPage(w, r, http.StatusBadRequest, "Could not read the CBZ or ZIP file.")
			return
		}
		files, size, err = extractChapter(z, stage)
		if err != nil {
			s.errorPage(w, r, http.StatusBadRequest, userMessage(err))
			return
		}
	} else if files, size, err = savePages(pages, stage, "A page is not a supported image."); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, userMessage(err))
		return
	}
	// Two uploads to one chapter would share its folder.
	s.pagesMu.Lock()
	defer s.pagesMu.Unlock()
	if c, err = s.db.chapter(c.ID); err != nil || !c.Unavailable || c.State == stateDone {
		s.errorPage(w, r, http.StatusConflict, "This chapter already has pages.")
		return
	}
	dest := filepath.Join(s.mangaDir, c.MangaID, c.ID)
	// Pages left from a download before the chapter was taken down.
	if err := os.RemoveAll(dest); err != nil {
		s.errorPage(w, r, 500, userMessage(err))
		return
	}
	if err := os.Rename(stage, dest); err != nil {
		s.errorPage(w, r, 500, userMessage(err))
		return
	}
	list, _ := json.Marshal(files)
	res, err := s.db.Exec(`UPDATE chapters SET pages=?,state=2,files=?,size=?,downloaded=?,error='',tries=0,pages_uploaded=1
		WHERE id=? AND unavailable=1 AND state<>2`, len(files), string(list), size, now(), c.ID)
	n := int64(0)
	if err == nil {
		n, err = res.RowsAffected()
	}
	if err != nil || n == 0 {
		os.RemoveAll(dest)
		if err != nil {
			s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
			return
		}
		s.errorPage(w, r, http.StatusConflict, "This chapter already has pages.")
		return
	}
	if err := s.db.bindReaderChapter(c.ID, false); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	if err := s.db.repairChapterPrimaries(c.MangaID); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, userMessage(err))
		return
	}
	s.db.Exec(`UPDATE manga SET latest=? WHERE id=?`, now(), c.MangaID)
	s.dl.Changed()
	back(w, r, "/manga/"+c.MangaID, plural(len(files), "page")+" added.")
}

func (s *Server) mangaFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.db.manga(id, currentUser(r).ID); err != nil {
		s.notFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, 400, "Invalid form.")
		return
	}
	source := r.FormValue("source")
	if source == "" {
		source = "json"
	}
	if source == "json" && !currentUser(r).Admin {
		s.errorPage(w, r, http.StatusForbidden, "Only administrators can configure external adapters.")
		return
	}
	u := strings.TrimSpace(r.FormValue("url"))
	connector, ok := s.dl.sources[source].(linkedChapterSource)
	if !ok {
		s.errorPage(w, r, http.StatusBadRequest, "Unknown chapter source.")
		return
	}
	u, normalizeErr := connector.NormalizeReference(u)
	if normalizeErr != nil || len(u) > 2000 {
		s.errorPage(w, r, http.StatusBadRequest, "Use a valid "+sourceLabel(source)+" URL without credentials or fragments.")
		return
	}
	var err error
	if r.FormValue("remove") == "1" {
		if !currentUser(r).Admin {
			s.errorPage(w, r, http.StatusForbidden, "Only administrators can disconnect a shared source.")
			return
		}
		_, err = s.db.Exec(`DELETE FROM chapter_feeds WHERE manga_id=? AND url=?`, id, u)
	} else {
		_, err = s.db.Exec(`INSERT INTO chapter_feeds(manga_id,url,source) VALUES(?,?,?) ON CONFLICT DO NOTHING`, id, u, source)
	}
	if err != nil {
		s.errorPage(w, r, 500, userMessage(err))
		return
	}
	s.dl.Changed()
	if r.FormValue("remove") != "1" {
		s.dl.Check(id)
	}
	back(w, r, "/manga/"+id, "Chapter sources updated.")
}
