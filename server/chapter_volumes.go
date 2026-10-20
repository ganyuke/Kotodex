package main

import (
	"cmp"
	"math/big"
	"slices"
)

type volumePoint struct {
	number *big.Rat
	volume string // empty when MangaDex disagrees about this chapter's volume
}

type volumeSeries map[string]volumePoint

type chapterVolumes struct {
	languages map[string]volumeSeries
	shared    volumeSeries
}

func (series volumeSeries) add(number *big.Rat, volume string) {
	key := number.RatString()
	if previous, exists := series[key]; exists && previous.volume != volume {
		volume = ""
	}
	series[key] = volumePoint{number, volume}
}

func (series volumeSeries) between(number *big.Rat) string {
	var lower, upper *volumePoint
	for _, point := range series {
		switch point.number.Cmp(number) {
		case -1:
			if lower == nil || point.number.Cmp(lower.number) > 0 {
				copy := point
				lower = &copy
			}
		case 1:
			if upper == nil || point.number.Cmp(upper.number) < 0 {
				copy := point
				upper = &copy
			}
		}
	}
	// Fill a missing chapter only when both known neighbours agree. Never
	// extrapolate across a volume boundary or beyond the known chapter range.
	if lower != nil && upper != nil && lower.volume != "" && lower.volume == upper.volume {
		return lower.volume
	}
	return ""
}

func (index *chapterVolumes) volume(c *Chapter) string {
	if !chapterDecimal.MatchString(c.Number) {
		return ""
	}
	number, ok := new(big.Rat).SetString(c.Number)
	if !ok {
		return ""
	}
	local := index.languages[c.Lang]
	if point, exists := local[number.RatString()]; exists {
		return point.volume
	}
	if point, exists := index.shared[number.RatString()]; exists {
		return point.volume
	}
	if volume := local.between(number); volume != "" {
		return volume
	}
	return index.shared.between(number)
}

func (db *DB) chapterVolumes(mangaID string) (*chapterVolumes, error) {
	rows, err := db.Query(`SELECT number,volume,lang FROM chapters WHERE manga_id=? AND source='mangadex' AND number<>'' AND volume<>''`, mangaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	index := &chapterVolumes{languages: map[string]volumeSeries{}, shared: volumeSeries{}}
	for rows.Next() {
		var number, volume, language string
		if err := rows.Scan(&number, &volume, &language); err != nil {
			return nil, err
		}
		number, volume = normalizeChapterNumber(number), normalizeChapterNumber(volume)
		if !chapterDecimal.MatchString(number) || !chapterDecimal.MatchString(volume) {
			continue
		}
		n, ok := new(big.Rat).SetString(number)
		if !ok {
			continue
		}
		if index.languages[language] == nil {
			index.languages[language] = volumeSeries{}
		}
		index.languages[language].add(n, volume)
		index.shared.add(n, volume)
	}
	return index, rows.Err()
}

// MangaFire's chapter feed has no volume membership. Align it with the
// canonical MangaDex chapter metadata instead of treating volume bundles as
// individual chapters or assigning volumes by counting chapters.
func (db *DB) alignMangaFireVolumes(mangaID string) error {
	index, err := db.chapterVolumes(mangaID)
	if err != nil {
		return err
	}
	chapters, err := db.queryChapters(`SELECT `+chapterCols+` FROM chapters c WHERE manga_id=? AND source='mangafire'`, mangaID)
	if err != nil {
		return err
	}
	for _, c := range chapters {
		volume := index.volume(c)
		if volume == "" || volume == c.Volume {
			continue
		}
		if _, err := db.Exec(`UPDATE chapters SET volume=?,volume_key=? WHERE id=? AND source='mangafire'`, volume, numberKey(volume), c.ID); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE reader_chapters SET volume_key=? WHERE id=? AND volume_key=''`, numberKey(volume), c.ReaderID); err != nil {
			return err
		}
	}
	return nil
}

// Also repair previously imported chapters without requiring an online check.
func (db *DB) backfillMangaFireVolumes() error {
	rows, err := db.Query(`SELECT DISTINCT manga_id FROM chapters WHERE source='mangafire'`)
	if err != nil {
		return err
	}
	var manga []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		manga = append(manga, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range manga {
		if err := db.alignMangaFireVolumes(id); err != nil {
			return err
		}
	}
	return nil
}

// In a series with continuous chapter numbering, a chapter whose volume is
// unresolved still belongs between its numbered neighbours. Keep volume-first
// order for series that restart numbering inside each volume.
func interleaveUnassignedChapters(chapters []*Chapter) {
	type numberedChapter struct {
		chapter *Chapter
		number  *big.Rat
	}
	type volumeRange struct{ volume, first, last *big.Rat }
	positions := map[string][]int{}
	needed := map[string]bool{}
	for i, c := range chapters {
		if !chapterDecimal.MatchString(c.Number) {
			continue
		}
		positions[c.Lang] = append(positions[c.Lang], i)
		if c.Source == "mangafire" && c.Volume == "" {
			needed[c.Lang] = true
		}
	}
	for language := range needed {
		var numbered []numberedChapter
		ranges := map[string]*volumeRange{}
		valid := true
		for _, i := range positions[language] {
			c := chapters[i]
			n, ok := new(big.Rat).SetString(c.Number)
			if !ok {
				valid = false
				break
			}
			numbered = append(numbered, numberedChapter{c, n})
			if c.Volume == "" {
				continue
			}
			v, ok := new(big.Rat).SetString(c.Volume)
			if !ok {
				valid = false
				break
			}
			key := v.RatString()
			if r := ranges[key]; r == nil {
				ranges[key] = &volumeRange{v, n, n}
			} else {
				if n.Cmp(r.first) < 0 {
					r.first = n
				}
				if n.Cmp(r.last) > 0 {
					r.last = n
				}
			}
		}
		var ordered []*volumeRange
		for _, r := range ranges {
			ordered = append(ordered, r)
		}
		slices.SortFunc(ordered, func(a, b *volumeRange) int { return a.volume.Cmp(b.volume) })
		for i := 1; i < len(ordered); i++ {
			if ordered[i].first.Cmp(ordered[i-1].last) <= 0 {
				valid = false
			}
		}
		if !valid {
			continue
		}
		slices.SortStableFunc(numbered, func(a, b numberedChapter) int { return a.number.Cmp(b.number) })
		for i, position := range positions[language] {
			chapters[position] = numbered[i].chapter
		}
	}
}

func (db *DB) queryReadingChapters(mangaID, query string, args ...any) ([]*Chapter, error) {
	chapters, err := db.queryChapters(query, args...)
	if err != nil || !slices.ContainsFunc(chapters, func(c *Chapter) bool { return c.Source == "mangafire" && c.Volume == "" }) {
		return chapters, err
	}
	// Decide whether numbering restarts using the whole catalog, including
	// chapters that have not been downloaded, rather than the visible subset.
	all, err := db.queryChapters(`SELECT `+chapterCols+` FROM chapters c WHERE c.manga_id=? ORDER BY `+chapterOrder, mangaID)
	if err != nil {
		return nil, err
	}
	interleaveUnassignedChapters(all)
	positions := make(map[string]int, len(all))
	for i, c := range all {
		positions[c.ID] = i
	}
	slices.SortStableFunc(chapters, func(a, b *Chapter) int { return cmp.Compare(positions[a.ID], positions[b.ID]) })
	return chapters, nil
}
