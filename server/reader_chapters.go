package main

import (
	"database/sql"
	"errors"
	"fmt"
)

// Source chapter rows own files and provenance. Reader chapters own identity
// and the choice of copy exposed to Kotatsu. Never regenerate this identity
// from source metadata when it is refreshed or a primary copy changes.
func initReaderChapters(db *DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS reader_chapters(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		manga_id TEXT NOT NULL REFERENCES manga(id) ON DELETE CASCADE,
		anchor_id TEXT NOT NULL UNIQUE,
		lang TEXT NOT NULL,
		number_key TEXT NOT NULL,
		volume_key TEXT NOT NULL,
		primary_id TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	var has int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('chapters') WHERE name='reader_id'`).Scan(&has); err != nil {
		return err
	}
	if has == 0 {
		if _, err := db.Exec(`ALTER TABLE chapters ADD COLUMN reader_id INTEGER REFERENCES reader_chapters(id)`); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS chapters_reader ON chapters(reader_id)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS reader_chapters_match ON reader_chapters(manga_id,lang,number_key,volume_key)`); err != nil {
		return err
	}
	var version string
	db.QueryRow(`SELECT value FROM settings WHERE key='reader_chapters_version'`).Scan(&version)
	// Preserve the numeric identity of the first downloaded copy on upgrade.
	// Hashing is used only to retain IDs already exposed by the old connector;
	// all subsequent identities are allocated and stored by SQLite.
	chapters, err := db.queryChapters(`SELECT ` + chapterCols + ` FROM chapters c WHERE c.reader_id IS NULL
		ORDER BY (c.state=2) DESC,c.downloaded,c.published,c.id`)
	if err != nil {
		return err
	}
	for _, c := range chapters {
		if err := db.bindReaderChapter(c.ID, version == ""); err != nil {
			return fmt.Errorf("migrate reader chapter: %w", err)
		}
	}
	_, err = db.Exec(`INSERT INTO settings(key,value) VALUES('reader_chapters_version','1') ON CONFLICT(key) DO UPDATE SET value='1'`)
	return err
}

func (db *DB) bindReaderChapter(releaseID string, legacy bool) error {
	db.readerMu.Lock()
	defer db.readerMu.Unlock()
	return db.bindReaderChapterLocked(releaseID, legacy)
}

func (db *DB) bindReaderChapterLocked(releaseID string, legacy bool) error {
	c, err := db.chapter(releaseID)
	if errors.Is(err, sql.ErrNoRows) { // a source was disconnected during discovery
		return nil
	}
	if err != nil || c.ReaderID != 0 {
		return err
	}
	number, volume := numberKey(c.Number), numberKey(c.Volume)
	var id int64
	// An anchor also lets interrupted allocation resume, and lets an uploaded
	// copy be deleted and reimported without recycling the reader identity.
	err = db.QueryRow(`SELECT id FROM reader_chapters WHERE anchor_id=? AND manga_id=?`, c.ID, c.MangaID).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id == 0 && number != "" {
		// Metadata helps associate a new source copy with an existing chapter;
		// it is not used to generate its ID. A missing volume is allowed. If
		// several volume-specific chapters fit, leave the ambiguous copy separate.
		var count int
		err = db.QueryRow(`SELECT count(*),coalesce(min(id),0) FROM reader_chapters
			WHERE manga_id=? AND lang=? AND number_key=? AND (volume_key=? OR volume_key='' OR ?='')`,
			c.MangaID, c.Lang, number, volume, volume).Scan(&count, &id)
		if err != nil {
			return err
		}
		if count != 1 {
			id = 0
		}
	}
	if id == 0 {
		if legacy {
			id = stableID("chapter:" + c.ID)
			_, err = db.Exec(`INSERT INTO reader_chapters(id,manga_id,anchor_id,lang,number_key,volume_key) VALUES(?,?,?,?,?,?)`,
				id, c.MangaID, c.ID, c.Lang, number, volume)
		} else {
			var result sql.Result
			result, err = db.Exec(`INSERT INTO reader_chapters(manga_id,anchor_id,lang,number_key,volume_key) VALUES(?,?,?,?,?)`,
				c.MangaID, c.ID, c.Lang, number, volume)
			if err == nil {
				id, err = result.LastInsertId()
			}
		}
		if err != nil {
			return err
		}
	}
	// Learn an available volume for matching future copies, without changing
	// this chapter's numeric identity or requiring every source to supply it.
	if volume != "" {
		if _, err := db.Exec(`UPDATE reader_chapters SET volume_key=? WHERE id=? AND volume_key=''`, volume, id); err != nil {
			return err
		}
	}
	_, err = db.Exec(`UPDATE chapters SET reader_id=? WHERE id=? AND reader_id IS NULL`, id, c.ID)
	return err
}

// rebindReaderChapter follows an edit of a copy's language, number, or
// volume. A copy alone in its reader chapter keeps that identity, so reading
// progress stays, unless exactly one other reader chapter now matches. A copy
// that shared its reader chapter leaves it and is bound like a new copy.
func (db *DB) rebindReaderChapter(releaseID string) error {
	db.readerMu.Lock()
	defer db.readerMu.Unlock()
	c, err := db.chapter(releaseID)
	if err != nil {
		return err
	}
	if c.ReaderID == 0 {
		return db.bindReaderChapterLocked(c.ID, false)
	}
	number, volume := numberKey(c.Number), numberKey(c.Volume)
	var lang, oldNumber, oldVolume, anchor string
	if err := db.QueryRow(`SELECT lang,number_key,volume_key,anchor_id FROM reader_chapters WHERE id=?`, c.ReaderID).Scan(&lang, &oldNumber, &oldVolume, &anchor); err != nil {
		return err
	}
	if lang == c.Lang && oldNumber == number && (oldVolume == volume || volume == "") {
		return nil
	}
	var others int
	if err := db.QueryRow(`SELECT count(*) FROM chapters WHERE reader_id=? AND id<>?`, c.ReaderID, c.ID).Scan(&others); err != nil {
		return err
	}
	if others > 0 {
		if anchor == c.ID {
			if _, err := db.Exec(`UPDATE reader_chapters SET anchor_id=(SELECT min(id) FROM chapters WHERE reader_id=? AND id<>?) WHERE id=?`, c.ReaderID, c.ID, c.ReaderID); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`UPDATE chapters SET reader_id=NULL WHERE id=?`, c.ID); err != nil {
			return err
		}
		return db.bindReaderChapterLocked(c.ID, false)
	}
	var count int
	var match int64
	if number != "" {
		if err := db.QueryRow(`SELECT count(*),coalesce(min(id),0) FROM reader_chapters
			WHERE manga_id=? AND lang=? AND number_key=? AND (volume_key=? OR volume_key='' OR ?='') AND id<>?`,
			c.MangaID, c.Lang, number, volume, volume, c.ReaderID).Scan(&count, &match); err != nil {
			return err
		}
	}
	if count == 1 {
		if _, err := db.Exec(`UPDATE chapters SET reader_id=? WHERE id=?`, match, c.ID); err != nil {
			return err
		}
		if volume != "" {
			if _, err := db.Exec(`UPDATE reader_chapters SET volume_key=? WHERE id=? AND volume_key=''`, volume, match); err != nil {
				return err
			}
		}
		_, err = db.Exec(`DELETE FROM reader_chapters WHERE id=? AND NOT EXISTS(SELECT 1 FROM chapters WHERE reader_id=?)`, c.ReaderID, c.ReaderID)
		return err
	}
	_, err = db.Exec(`UPDATE reader_chapters SET lang=?,number_key=?,volume_key=? WHERE id=?`, c.Lang, number, volume, c.ReaderID)
	return err
}

// Keep a selected primary while it is available. Only an absent/deleted copy
// permits automatic fallback; downloading another copy never steals selection.
func (db *DB) repairChapterPrimaries(mangaID string) error {
	_, err := db.Exec(`UPDATE reader_chapters SET primary_id=coalesce((
		SELECT c.id FROM chapters c WHERE c.reader_id=reader_chapters.id AND c.state=2
		ORDER BY c.downloaded,c.published,c.id LIMIT 1),'')
		WHERE manga_id=? AND NOT EXISTS(SELECT 1 FROM chapters c
			WHERE c.id=reader_chapters.primary_id AND c.reader_id=reader_chapters.id AND c.state=2)`, mangaID)
	return err
}

func (db *DB) readerChapters(mangaID string) ([]*Chapter, error) {
	if err := db.alignMangaFireVolumes(mangaID); err != nil {
		return nil, err
	}
	if err := db.repairChapterPrimaries(mangaID); err != nil {
		return nil, err
	}
	return db.queryReadingChapters(mangaID, `SELECT `+chapterCols+` FROM chapters c JOIN reader_chapters rc
		ON rc.id=c.reader_id AND rc.primary_id=c.id WHERE c.manga_id=? AND c.state=2 ORDER BY `+chapterOrder, mangaID)
}

func (db *DB) primaryChapter(id int64) (*Chapter, error) {
	var mangaID string
	if err := db.QueryRow(`SELECT manga_id FROM reader_chapters WHERE id=?`, id).Scan(&mangaID); err != nil {
		return nil, err
	}
	if err := db.repairChapterPrimaries(mangaID); err != nil {
		return nil, err
	}
	return scanChapter(db.QueryRow(`SELECT `+chapterCols+` FROM chapters c JOIN reader_chapters rc ON rc.primary_id=c.id
		AND rc.id=c.reader_id WHERE rc.id=? AND c.state=2`, id))
}

func (db *DB) setPrimaryChapter(releaseID string) error {
	res, err := db.Exec(`UPDATE reader_chapters SET primary_id=? WHERE id=(
		SELECT reader_id FROM chapters WHERE id=? AND state=2)`, releaseID, releaseID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return shownError("Download this copy before making it primary.")
	}
	return nil
}
