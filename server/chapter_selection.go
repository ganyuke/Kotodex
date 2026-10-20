package main

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Selection is explicit after the first filter submission, including when no
// boxes are checked. An unfiltered manga page always starts with MangaDex.
func selectedChapterSources(values url.Values) []string {
	if !values.Has("source") && !values.Has("sources") {
		return []string{"mangadex"}
	}
	var out []string
	for _, source := range values["source"] {
		if source != "" && !slices.Contains(out, source) {
			out = append(out, source)
		}
	}
	return out
}

// The query parameters of the chapter table's filters.
var chapterFilterKeys = []string{"sources", "source", "priority", "prefer", "lang", "unavailable", "official"}

func chapterFilters(q url.Values) url.Values {
	out := url.Values{}
	for _, key := range chapterFilterKeys {
		if q.Has(key) {
			out[key] = q[key]
		}
	}
	return out
}

// pageFilters are the filters a manga page shows. Filters in the address
// are saved as this person's filters for the manga. Without any, the saved
// ones are used, so the page looks the same when opened again.
func (db *DB) pageFilters(userID int64, mangaID string, q url.Values) (filters url.Values, saved bool) {
	filters = chapterFilters(q)
	if len(filters) > 0 {
		// Live updates repeat the page's address and change nothing.
		if !q.Has("live") {
			db.Exec(`INSERT INTO chapter_filters(user_id,manga_id,query) VALUES(?,?,?)
				ON CONFLICT(user_id,manga_id) DO UPDATE SET query=excluded.query`, userID, mangaID, filters.Encode())
		}
		return filters, true
	}
	var query string
	if db.QueryRow(`SELECT query FROM chapter_filters WHERE user_id=? AND manga_id=?`, userID, mangaID).Scan(&query) != nil {
		return filters, false
	}
	parsed, _ := url.ParseQuery(query)
	return chapterFilters(parsed), true
}

func visibleChapters(chapters []*Chapter, sources, languages []string) []*Chapter {
	var out []*Chapter
	for _, c := range chapters {
		if slices.Contains(sources, c.Source) && (languages == nil || slices.Contains(languages, c.Lang)) {
			out = append(out, c)
		}
	}
	return out
}

// The complete order is carried independently of which sources are checked.
// Older links with a single preferred source retain their meaning.
func chapterSourcePriority(values url.Values) []string {
	var out []string
	requested := values["priority"]
	if !values.Has("priority") && values.Get("prefer") != "" {
		requested = []string{values.Get("prefer")}
	}
	for _, source := range requested {
		if source != "" && !slices.Contains(out, source) {
			out = append(out, source)
		}
	}
	return out
}

func sourceRank(source string, priority []string) int {
	if i := slices.Index(priority, source); i >= 0 {
		return i
	}
	if i := slices.Index([]string{"mangadex", "mangafire", "json", "upload"}, source); i >= 0 {
		return len(priority) + i
	}
	return len(priority) + 4
}

func compareChapterReleases(a, b *Chapter, priority []string) int {
	if n := sourceRank(a.Source, priority) - sourceRank(b.Source, priority); n != 0 {
		return n
	}
	if n := strings.Compare(a.Source, b.Source); n != 0 {
		return n
	}
	// Keep the earliest release within one source, then a stable identity.
	if a.Published < b.Published {
		return -1
	}
	if a.Published > b.Published {
		return 1
	}
	return strings.Compare(a.ID, b.ID)
}

// Queue the chosen visible releases only. The database guard, rather than an
// earlier in-memory check, decides whether the shared library already has one.
func (db *DB) queueMissingChapters(chapters []*Chapter, priority []string) (int, error) {
	ordered := slices.Clone(chapters)
	slices.SortStableFunc(ordered, func(a, b *Chapter) int { return compareChapterReleases(a, b, priority) })
	n, at := 0, now()
	for _, c := range ordered {
		res, err := db.Exec(`UPDATE chapters SET state=1,tries=0,error='',queued=? WHERE id=? AND state IN (0,3) AND unavailable=0
			AND (?='' OR EXISTS(SELECT 1 FROM chapter_feeds WHERE manga_id=? AND url=? AND source=?))
			AND NOT EXISTS(SELECT 1 FROM chapters other WHERE other.manga_id=chapters.manga_id
				AND other.lang=chapters.lang AND other.state IN (1,2) AND chapters.number_key<>''
				AND other.number_key=chapters.number_key
				AND (other.volume_key=chapters.volume_key OR other.volume_key='' OR chapters.volume_key=''))`, at, c.ID, c.FeedURL, c.MangaID, c.FeedURL, c.Source)
		if err != nil {
			return n, err
		}
		count, err := res.RowsAffected()
		if err != nil {
			return n, err
		}
		n += int(count)
	}
	return n, nil
}

var chapterPrefix = regexp.MustCompile(`(?i)^(?:ch(?:apter)?|vol(?:ume)?)\.?\s*`)

// Strip presentation prefixes only when the remainder really is a number.
// Unknown labels stay intact instead of guessing an identity or a volume.
func normalizeChapterNumber(raw string) string {
	original := strings.TrimSpace(raw)
	value := chapterPrefix.ReplaceAllString(original, "")
	if !chapterDecimal.MatchString(value) {
		return original
	}
	whole, fraction, _ := strings.Cut(value, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction != "" {
		return whole + "." + fraction
	}
	return whole
}

func normalizeChapter(c *Chapter) {
	c.Volume = normalizeChapterNumber(c.Volume)
	c.Number = normalizeChapterNumber(c.Number)
}

// Backfill identity keys only during migration. Close rows before any writes:
// the database deliberately has a single connection.
func backfillChapterKeys(db *DB) error {
	rows, err := db.Query(`SELECT id,volume,number FROM chapters`)
	if err != nil {
		return err
	}
	var chapters []*Chapter
	for rows.Next() {
		c := &Chapter{}
		if err := rows.Scan(&c.ID, &c.Volume, &c.Number); err != nil {
			rows.Close()
			return err
		}
		normalizeChapter(c)
		chapters = append(chapters, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range chapters {
		if _, err := db.Exec(`UPDATE chapters SET volume=?,number=?,volume_key=?,number_key=? WHERE id=?`, c.Volume, c.Number, numberKey(c.Volume), numberKey(c.Number), c.ID); err != nil {
			return fmt.Errorf("migrate chapter identity: %w", err)
		}
	}
	return nil
}
