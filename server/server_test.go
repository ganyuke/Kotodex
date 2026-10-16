package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mangaDir := filepath.Join(dir, "manga")
	mdx := newMangaDex("Kotodex-test")
	keys, err := loadKeys(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		db: db, mdx: mdx, dl: newDownloader(db, mdx, keys, mangaDir), keys: keys,
		mangaDir: mangaDir, limiter: &loginLimiter{fails: map[string][]time.Time{}}, covers: newCovers(db, mangaDir),
	}
	s.loadTemplates()
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

const (
	mangaA   = "6f2b317e-17ff-491a-8f61-e6e591ae2df2"
	chapter1 = "18ccd96c-23de-4ba8-a6dc-fed34697d2ab"
	chapter2 = "28ccd96c-23de-4ba8-a6dc-fed34697d2ab"
)

const testSyncPassword = "sync-pw"

func seed(t *testing.T, s *Server) int64 {
	t.Helper()
	uid, err := s.db.createUser("mio", "pw", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.upsertManga(&Manga{ID: mangaA, Title: "Akanabe", Tags: []string{"Comedy"}, Cover: "x.jpg"}); err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE users SET sync_pass=? WHERE id=?`, hashToken(testSyncPassword), uid)
	s.db.setFollow(uid, mangaA, true)
	s.db.insertChapter(&Chapter{ID: chapter1, MangaID: mangaA, Number: "10", Lang: "en", Published: 100})
	s.db.insertChapter(&Chapter{ID: chapter2, MangaID: mangaA, Number: "9.5", Lang: "en", Published: 200})
	os.MkdirAll(filepath.Join(s.mangaDir, mangaA, chapter1), 0o755)
	os.WriteFile(filepath.Join(s.mangaDir, mangaA, chapter1, "001.jpg"), []byte("page"), 0o644)
	s.db.Exec(`UPDATE chapters SET state=2, files='["001.jpg"]' WHERE id=?`, chapter1)
	return uid
}

func TestAPIFlow(t *testing.T) {
	s, ts := testServer(t)
	seed(t, s)

	res, err := http.Post(ts.URL+"/api/v1/login", "application/json", strings.NewReader(`{"username":"mio","password":"nope"}`))
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %v %v", res.StatusCode, err)
	}
	res, _ = http.Post(ts.URL+"/api/v1/login", "application/json", strings.NewReader(`{"username":"Mio","password":"pw"}`))
	var login struct{ Token string }
	json.NewDecoder(res.Body).Decode(&login)
	if login.Token == "" {
		t.Fatal("no token")
	}

	get := func(path string, out any) int {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+login.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if out != nil {
			json.NewDecoder(res.Body).Decode(out)
		}
		return res.StatusCode
	}

	var list []apiManga
	get("/api/v1/manga?tag=Comedy", &list)
	if len(list) != 1 || list[0].Chapters != 1 {
		t.Fatalf("list: %+v", list)
	}
	get("/api/v1/manga?tag=Horror", &list)
	if len(list) != 0 {
		t.Fatalf("tag filter: %+v", list)
	}

	var detail struct{ ChapterList []apiChapter }
	get("/api/v1/manga/"+mangaA, &detail)
	if len(detail.ChapterList) != 1 || detail.ChapterList[0].ID != chapter1 {
		t.Fatalf("only downloaded chapters should be listed: %+v", detail.ChapterList)
	}

	var pages []string
	get("/api/v1/chapters/"+chapter1+"/pages", &pages)
	if len(pages) != 1 {
		t.Fatalf("pages: %v", pages)
	}
	res, _ = http.Get(ts.URL + pages[0])
	if res.StatusCode != http.StatusOK {
		t.Fatalf("signed page: %d", res.StatusCode)
	}
	if code := get("/api/v1/chapters/"+chapter2+"/pages", nil); code != http.StatusNotFound {
		t.Fatalf("pages of a chapter that isn't downloaded: %d", code)
	}

	// Changing the password revokes the session and every signed link.
	var uid int64
	s.db.QueryRow(`SELECT id FROM users WHERE name='mio'`).Scan(&uid)
	s.db.setPassword(uid, "new")
	if code := get("/api/v1/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("session after password change: %d", code)
	}
	res, _ = http.Get(ts.URL + pages[0])
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("old signed link: %d", res.StatusCode)
	}
}

func TestSignOutRevokesLinks(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	phone, _ := s.db.createSession(uid, "kotatsu", "phone")
	tablet, _ := s.db.createSession(uid, "kotatsu", "tablet")
	link := func(token string) string {
		u, err := s.db.sessionUser(token)
		if err != nil {
			t.Fatal(err)
		}
		return ts.URL + s.fileURL(u, mangaA+"/"+chapter1+"/001.jpg")
	}
	phoneLink, tabletLink := link(phone), link(tablet)
	if phoneLink == tabletLink {
		t.Fatal("each session should get its own links")
	}
	s.db.deleteSession(phone)
	if res, _ := http.Get(phoneLink); res.StatusCode != http.StatusForbidden {
		t.Fatalf("signed-out device's link: %d", res.StatusCode)
	}
	if res, _ := http.Get(tabletLink); res.StatusCode != http.StatusOK {
		t.Fatalf("other device's link: %d", res.StatusCode)
	}
	// A valid session ID with a signature for a different file is refused.
	forged := strings.Replace(tabletLink, "001.jpg", "002.jpg", 1)
	if res, _ := http.Get(forged); res.StatusCode != http.StatusForbidden {
		t.Fatalf("forged link: %d", res.StatusCode)
	}
}

func TestChapterOrderAndDuplicates(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	list, _ := s.db.chapters(mangaA)
	if list[0].Number != "9.5" || list[1].Number != "10" {
		t.Fatalf("order: %s, %s", list[0].Number, list[1].Number)
	}
	if !s.db.hasChapterNumber(mangaA, "en", "", "10") {
		t.Fatal("chapter 10 is downloaded")
	}
	if s.db.hasChapterNumber(mangaA, "en", "", "9.5") {
		t.Fatal("chapter 9.5 is only known, not downloaded or queued")
	}
	if s.db.hasChapterNumber(mangaA, "es", "", "10") {
		t.Fatal("other languages don't count")
	}
}

func TestWebRequiresSignIn(t *testing.T) {
	_, ts := testServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, _ := client.Get(ts.URL + "/settings")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Fatalf("got %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestCrossSitePostBlocked(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	req, _ := http.NewRequest("POST", ts.URL+"/unfollow", strings.NewReader("id="+mangaA))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d", res.StatusCode)
	}
}

func TestImagePathValidation(t *testing.T) {
	good := []string{mangaA + "/cover.jpg", mangaA + "/thumb.jpg", mangaA + "/" + chapter1 + "/001.png"}
	bad := []string{"../kotodex.db", mangaA + "/../x", mangaA + "/" + chapter1 + "/../../kotodex.db", mangaA + "/.cover"}
	for _, p := range good {
		if !filePathRe.MatchString(p) {
			t.Errorf("should accept %s", p)
		}
	}
	for _, p := range bad {
		if filePathRe.MatchString(p) {
			t.Errorf("should reject %s", p)
		}
	}
}

func TestMangaIDFrom(t *testing.T) {
	cases := map[string]string{
		"https://mangadex.org/title/" + mangaA + "/akanabe-sensei": mangaA,
		strings.ToUpper(mangaA):                mangaA,
		"akanabe":                              "",
		"../../../../../../../../../x-y-z-w-v": "",
	}
	for in, want := range cases {
		if got := mangaIDFrom(in); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}

func TestIdleSessionsExpire(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "kotatsu", "old phone")
	u, _ := s.db.sessionUser(token)
	link := ts.URL + s.fileURL(u, mangaA+"/"+chapter1+"/001.jpg")
	if res, _ := http.Get(link); res.StatusCode != http.StatusOK {
		t.Fatalf("fresh link: %d", res.StatusCode)
	}

	s.db.Exec(`UPDATE sessions SET used=? WHERE hash=?`, time.Now().Add(-91*24*time.Hour).Unix(), hashToken(token))
	if res, _ := http.Get(link); res.StatusCode != http.StatusForbidden {
		t.Fatalf("link from an idle session: %d", res.StatusCode)
	}
	if _, err := s.db.sessionUser(token); err == nil {
		t.Fatal("idle session should be refused")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM sessions WHERE hash=?`, hashToken(token)).Scan(&n)
	if n != 0 {
		t.Fatal("idle session should be deleted")
	}

	// Used 89 days ago is still fine.
	recent, _ := s.db.createSession(uid, "kotatsu", "tablet")
	s.db.Exec(`UPDATE sessions SET used=? WHERE hash=?`, time.Now().Add(-89*24*time.Hour).Unix(), hashToken(recent))
	s.db.deleteIdleSessions()
	if _, err := s.db.sessionUser(recent); err != nil {
		t.Fatalf("recent session: %v", err)
	}
}

func TestForwardedForCantBeSpoofed(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.17.0.1:5000" // reverse proxy on a Docker network
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("should use the entry the proxy added, got %s", got)
	}
	r.RemoteAddr = "198.51.100.7:5000" // straight from the internet
	if got := clientIP(r); got != "198.51.100.7" {
		t.Fatalf("should ignore the header from a public address, got %s", got)
	}
}

func TestLoginLimitPerAccount(t *testing.T) {
	s, ts := testServer(t)
	seed(t, s)
	try := func(ip, pass string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/login", strings.NewReader(`{"username":"mio","password":"`+pass+`"}`))
		req.Header.Set("X-Forwarded-For", ip) // the test client connects from loopback
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	for i := 0; i < 10; i++ {
		try(fmt.Sprintf("10.0.0.%d", i), "wrong")
	}
	if code := try("10.0.1.1", "pw"); code != http.StatusTooManyRequests {
		t.Fatalf("guesses from many addresses should still lock the account for a while, got %d", code)
	}
}

func TestLocalPath(t *testing.T) {
	for _, p := range []string{"/manga/x", "/downloads?page=2"} {
		if !localPath(p) {
			t.Errorf("should allow %s", p)
		}
	}
	for _, p := range []string{"", "https://evil.example", "//evil.example", "/\\evil.example", "/\\/evil.example", "/\t/evil.example", "/\n/evil.example", "/\r/evil.example"} {
		if localPath(p) {
			t.Errorf("should refuse %s", p)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	s, _ := testServer(t)
	ts := httptest.NewServer(securityHeaders(s.routes()))
	defer ts.Close()
	res, _ := http.Get(ts.URL + "/login")
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP: %q", csp)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing headers")
	}
}

func TestKeyFile(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), keyFileName)
	keys, err := loadKeys(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(keyPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	sealed := keys.seal("client-secret")
	if strings.Contains(sealed, "client-secret") {
		t.Fatal("not encrypted")
	}
	again, _ := loadKeys(keyPath)
	if got, err := again.open(sealed); err != nil || got != "client-secret" {
		t.Fatalf("same key after restart: %q %v", got, err)
	}

	// A database copied without the key can't be read.
	os.Remove(keyPath)
	other, _ := loadKeys(keyPath)
	if _, err := other.open(sealed); err == nil {
		t.Fatal("a different key should not decrypt")
	}

	t.Setenv("KOTODEX_SECRET", "short")
	if _, err := loadKeys(keyPath); err == nil {
		t.Fatal("short KOTODEX_SECRET should be refused")
	}
}

func TestFollowLanguages(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s) // follows mangaA, user default "en"
	other, _ := s.db.createUser("rin", "pw", false)
	s.db.Exec(`UPDATE users SET langs='es-la,pt-br' WHERE id=?`, other)
	s.db.setFollow(other, mangaA, true)

	if langs, custom := s.db.followLangs(other, mangaA); strings.Join(langs, ",") != "es-la,pt-br" || custom {
		t.Fatalf("a new follow uses the defaults: %v %v", langs, custom)
	}
	want := s.db.wantedLangs(mangaA)
	if !want["en"] || !want["es-la"] || !want["pt-br"] || len(want) != 3 {
		t.Fatalf("wanted: %v", want)
	}

	// Changing the defaults reaches follows that use them.
	s.db.Exec(`UPDATE users SET langs='fr' WHERE id=?`, other)
	if langs, _ := s.db.followLangs(other, mangaA); strings.Join(langs, ",") != "fr" {
		t.Fatalf("defaults should apply live: %v", langs)
	}

	// Languages set for one manga override the defaults.
	s.db.setFollowLangs(other, mangaA, []string{"ja", "ko"})
	s.db.Exec(`UPDATE users SET langs='de' WHERE id=?`, other)
	if langs, custom := s.db.followLangs(other, mangaA); strings.Join(langs, ",") != "ja,ko" || !custom {
		t.Fatalf("override: %v %v", langs, custom)
	}

	// Picking exactly the defaults is the same as resetting.
	s.db.setFollowLangs(other, mangaA, []string{"de"})
	if _, custom := s.db.followLangs(other, mangaA); custom {
		t.Fatal("choosing the defaults should not count as an override")
	}

	s.db.setFollow(uid, mangaA, false)
	if s.db.wantedLangs(mangaA)["en"] {
		t.Fatal("en should no longer be wanted")
	}
}

func TestFollowsDefaultToMangaDex(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s) // followed mangaA in Kotodex, no MangaDex account yet
	const b, c = "b0000000-0000-0000-0000-000000000000", "c0000000-0000-0000-0000-000000000000"
	following := func(id string) bool {
		var v bool
		s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM follows WHERE user_id=? AND manga_id=?)`, uid, id).Scan(&v)
		return v
	}
	link := func() {
		s.db.Exec(`INSERT INTO mdx_accounts(user_id,username,client_id,client_secret,refresh) VALUES(?,'mio','c','s','r') ON CONFLICT DO NOTHING`, uid)
	}
	link()
	mdx := func(ids ...string) {
		var list []*Manga
		for _, id := range ids {
			list = append(list, &Manga{ID: id, Title: id})
		}
		if err := s.db.setMangaDexFollows(uid, list); err != nil {
			t.Fatal(err)
		}
	}

	mdx(b, c)
	if !following(b) || !following(c) || !following(mangaA) {
		t.Fatal("MangaDex follows are imported, and the follow made here stays")
	}
	if !s.db.followChoice(uid, mangaA) || s.db.followChoice(uid, b) {
		t.Fatal("only the follow made here differs from MangaDex")
	}

	// Unfollowing here is a choice that wins over MangaDex, and survives imports.
	s.db.setFollow(uid, b, false)
	mdx(b, c)
	if following(b) || !s.db.followChoice(uid, b) {
		t.Fatal("unfollowed here should stay unfollowed")
	}

	// Resetting matches MangaDex again.
	s.db.resetFollow(uid, b)
	if !following(b) || s.db.followChoice(uid, b) {
		t.Fatal("reset should follow again, like on MangaDex")
	}

	// Following here what MangaDex already follows is no choice at all.
	s.db.setFollow(uid, c, true)
	if s.db.followChoice(uid, c) {
		t.Fatal("same as MangaDex should not be a choice")
	}

	// Unfollowed on MangaDex: c goes, mangaA (chosen here) stays.
	mdx(b)
	if following(c) || !following(mangaA) {
		t.Fatal("unfollowing on MangaDex should only affect follows without a choice")
	}

	// Unlinking and keeping the imported follows turns them into choices.
	s.db.Exec(`INSERT INTO mdx_accounts(user_id,username,client_id,client_secret,refresh) VALUES(?,?,?,?,?)`, uid, "mio", "x", "x", "x")
	s.db.setFollow(uid, c, false) // c isn't on MangaDex, so this is no choice
	if err := s.db.unlinkMangaDex(uid, true); err != nil {
		t.Fatal(err)
	}
	if !following(b) || !following(mangaA) || following(c) {
		t.Fatal("keep should leave everything followed as it was")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM follow_choices WHERE user_id=? AND following=0`, uid).Scan(&n)
	if n != 0 {
		t.Fatal("unfollow choices mean nothing without MangaDex")
	}
}

func TestLongPasswords(t *testing.T) {
	s, _ := testServer(t)
	long := strings.Repeat("ア", 50) // 150 bytes, past bcrypt's 72
	uid, err := s.db.createUser("mio", long, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.checkPassword("mio", long); err != nil {
		t.Fatalf("long password: %v", err)
	}
	if _, err := s.db.checkPassword("mio", long[:72]); err == nil {
		t.Fatal("first 72 bytes alone signed in")
	}
	if err := s.db.setPassword(uid, long+"x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.checkPassword("mio", long+"x"); err != nil {
		t.Fatalf("changed long password: %v", err)
	}
}

func TestBcryptPasswordsUpgrade(t *testing.T) {
	s, _ := testServer(t)
	uid, _ := s.db.createUser("mio", "pw", false)
	old, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	s.db.Exec(`UPDATE users SET pass=? WHERE id=?`, string(old), uid)
	token, _ := s.db.createSession(uid, "web", "test")

	if _, err := s.db.checkPassword("mio", "nope"); err == nil {
		t.Fatal("wrong password accepted")
	}
	var hash string
	s.db.QueryRow(`SELECT pass FROM users WHERE id=?`, uid).Scan(&hash)
	if hash != string(old) {
		t.Fatal("hash changed after a wrong password")
	}

	if _, err := s.db.checkPassword("mio", "pw"); err != nil {
		t.Fatalf("bcrypt password: %v", err)
	}
	s.db.QueryRow(`SELECT pass FROM users WHERE id=?`, uid).Scan(&hash)
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("not upgraded: %s", hash)
	}
	if _, err := s.db.checkPassword("mio", "pw"); err != nil {
		t.Fatalf("after upgrade: %v", err)
	}
	if _, err := s.db.sessionUser(token); err != nil {
		t.Fatal("upgrade signed the user out")
	}
}

// A reverse proxy that rewrites Host (nginx's default proxy_pass) must not
// break posts from the site itself.
func TestSameSitePostBehindProxy(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	post := func(site, origin string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/unfollow", strings.NewReader("id="+mangaA))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		req.Header.Set("Sec-Fetch-Site", site)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post("same-origin", "https://manga.example"); code == http.StatusForbidden {
		t.Fatalf("same-origin post behind proxy got %d", code)
	}
	if code := post("cross-site", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-site post got %d", code)
	}
}

func TestDuplicateUserName(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	for _, name := range []string{"mio", "MIO", " mio "} {
		if _, err := s.db.createUser(name, "pw", false); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("%q: got %v", name, err)
		}
	}
}

func TestDuplicateUserNameRace(t *testing.T) {
	s, _ := testServer(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.db.createUser("rin", "pw", false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		} else if !strings.Contains(err.Error(), "already exists") {
			t.Errorf("got %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d users created", ok)
	}
}

func TestErrorTextStaysInLog(t *testing.T) {
	if got := userMessage(errors.New("UNIQUE constraint failed: users.name")); strings.Contains(got, "UNIQUE") {
		t.Fatalf("database text shown: %q", got)
	}
	if got := userMessage(fmt.Errorf("wrapped: %w", shownError("Password is required."))); got != "wrapped: Password is required." {
		t.Fatalf("got %q", got)
	}
	if got := userMessage(&url.Error{Op: "Get", URL: "https://api.mangadex.org", Err: errors.New("dial tcp: i/o timeout")}); strings.Contains(got, "dial") {
		t.Fatalf("network text shown: %q", got)
	}
}

func TestImportAfterUnlinkAddsNothing(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s)
	const b = "b0000000-0000-0000-0000-000000000000"
	s.db.Exec(`INSERT INTO mdx_accounts(user_id,username,client_id,client_secret,refresh) VALUES(?,'mio','c','s','r')`, uid)
	// An import that fetched the list before the unlink and saves it after.
	if err := s.db.unlinkMangaDex(uid, false); err != nil {
		t.Fatal(err)
	}
	if err := s.db.setMangaDexFollows(uid, []*Manga{{ID: b, Title: "b"}}); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM mdx_follows WHERE user_id=?`, uid).Scan(&n)
	var following bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM follows WHERE user_id=? AND manga_id=?)`, uid, b).Scan(&following)
	if n != 0 || following {
		t.Fatalf("import after unlink: %d MangaDex follows, following=%v", n, following)
	}
}

func TestAdminsDeletingEachOther(t *testing.T) {
	for i := 0; i < 20; i++ {
		s, ts := testServer(t)
		a, _ := s.db.createUser("a", "pw", true)
		b, _ := s.db.createUser("b", "pw", true)
		ta, _ := s.db.createSession(a, "browser", "test")
		tb, _ := s.db.createSession(b, "browser", "test")
		var wg sync.WaitGroup
		del := func(token string, target int64) {
			defer wg.Done()
			req, _ := http.NewRequest("POST", fmt.Sprintf("%s/settings/users/%d/delete", ts.URL, target), nil)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
			res, err := http.DefaultClient.Do(req)
			if err == nil {
				res.Body.Close()
			}
		}
		wg.Add(2)
		go del(ta, b)
		go del(tb, a)
		wg.Wait()
		var admins int
		s.db.QueryRow(`SELECT count(*) FROM users WHERE admin=1`).Scan(&admins)
		if admins == 0 {
			t.Fatal("no admin left")
		}
	}
}

func TestTitleFallbackIsStable(t *testing.T) {
	titles := map[string]string{"fr": "Le titre", "de": "Der Titel", "it": "Il titolo"}
	for i := 0; i < 50; i++ {
		if got := pickLang(titles, "en"); got != "Der Titel" {
			t.Fatalf("got %q", got)
		}
	}
}

func TestLibrarySearch(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s)
	s.db.upsertManga(&Manga{ID: mangaA, Title: "Akanabe", AltTitles: []string{"Rock & Roll", "100% Pure"}})
	for q, want := range map[string]int{"rock & roll": 1, "100%": 1, "10_%": 0, `"`: 0, ",": 0, "u0026": 0} {
		list, err := s.db.library(LibraryQuery{UserID: uid, Query: q})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != want {
			t.Errorf("%q: got %d results, want %d", q, len(list), want)
		}
	}
}

func TestLangCodes(t *testing.T) {
	got := langCodes([]string{"en", "es-la", "en", "en,ja", "", "EN", "pt-br"})
	if strings.Join(got, ",") != "en,es-la,pt-br" {
		t.Fatalf("got %v", got)
	}
}

func TestLoginLimiterForgets(t *testing.T) {
	l := &loginLimiter{fails: map[string][]time.Time{}}
	l.fails["user:old"] = []time.Time{time.Now().Add(-time.Hour)}
	for i := 0; i <= 10000; i++ {
		l.fails[fmt.Sprintf("user:x%d", i)] = []time.Time{time.Now().Add(-time.Hour)}
	}
	l.fail([]string{"user:new"})
	if len(l.fails) != 1 {
		t.Fatalf("%d entries kept", len(l.fails))
	}
}

func TestNewCoverNewLink(t *testing.T) {
	s, _ := testServer(t)
	u := &User{SessionID: "sid"}
	a := s.coverURL(u, &Manga{ID: mangaA, Cover: "one.jpg"}, mangaA+"/thumb.jpg")
	b := s.coverURL(u, &Manga{ID: mangaA, Cover: "two.jpg"}, mangaA+"/thumb.jpg")
	if a == b {
		t.Fatal("same link after the cover changed")
	}
}

func TestCoverLinkLoads(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "kotatsu", "test")
	u, _ := s.db.sessionUser(token)
	os.WriteFile(filepath.Join(s.mangaDir, mangaA, "thumb.jpg"), []byte("jpg"), 0o644)
	res, err := http.Get(ts.URL + s.coverURL(u, &Manga{ID: mangaA, Cover: "a b.jpg"}, mangaA+"/thumb.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("got %d", res.StatusCode)
	}
}

func TestNoticeShowsOnce(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(ts.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: token}})
	client := &http.Client{Jar: jar}
	page := func(res *http.Response) string {
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}

	res, err := client.PostForm(ts.URL+"/manga/"+mangaA+"/check", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Request.URL.RawQuery, "notice") {
		t.Fatalf("notice in URL: %s", res.Request.URL)
	}
	if !strings.Contains(page(res), "Checking for new chapters.") {
		t.Fatal("notice missing after redirect")
	}
	res, _ = client.Get(ts.URL + "/manga/" + mangaA)
	if strings.Contains(page(res), "Checking for new chapters.") {
		t.Fatal("notice shown twice")
	}
	// Another page loaded in between leaves the notice for the page it's for.
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, _ = noFollow.PostForm(ts.URL+"/manga/"+mangaA+"/check", url.Values{"next": {"/manga/" + mangaA + "?lang=en"}})
	res.Body.Close()
	res, _ = client.Get(ts.URL + "/downloads")
	if strings.Contains(page(res), "Checking for new chapters.") {
		t.Fatal("notice shown on another page")
	}
	res, _ = client.Get(ts.URL + "/manga/" + mangaA)
	if !strings.Contains(page(res), "Checking for new chapters.") {
		t.Fatal("notice lost")
	}
	res, _ = client.Get(ts.URL + "/manga/" + mangaA + "?notice=Sign+in+at+evil.example")
	if strings.Contains(page(res), `<p class="notice">`) {
		t.Fatal("notice taken from URL")
	}
}

// sseStream reads one live update stream.
type sseStream struct {
	res   *http.Response
	body  *bufio.Reader
	event string // name of the last event read
}

func openStream(t *testing.T, url, token string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept", "text/html, text/event-stream") // what htmx sends
	req.Header.Set(liveHeader, "1")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	return &sseStream{res: res, body: bufio.NewReader(res.Body)}
}

// next returns the data of the next event.
func (st *sseStream) next() (string, error) {
	var data []string
	st.event = "message"
	for {
		line, err := st.body.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "" && data != nil:
			return strings.Join(data, "\n"), nil
		case strings.HasPrefix(line, "event: "):
			st.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

// expectEnd checks that the stream tells the page to reload, then closes.
func (st *sseStream) expectEnd(t *testing.T) {
	t.Helper()
	if _, err := st.next(); err != nil || st.event != "end" {
		t.Fatalf("expected end, got %q %v", st.event, err)
	}
	if ev, err := st.next(); err == nil {
		t.Fatalf("stream still open after end: %s", ev)
	}
}

func TestLiveUpdates(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	st := openStream(t, ts.URL+"/downloads", token)

	ev, err := st.next()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"live-actions", "live-now", "live-queue", "live-summary", "live-history"} {
		if !strings.Contains(ev, `<div id="`+id+`" hx-swap-oob="innerMorph">`) {
			t.Fatalf("first event lacks %s: %s", id, ev)
		}
	}
	if strings.Contains(ev, "<html") || !strings.Contains(ev, "Nothing waiting to download.") {
		t.Fatalf("first event: %s", ev)
	}

	s.db.queueChapter(chapter1, now())
	s.dl.Wake()
	if ev, err = st.next(); err != nil || !strings.Contains(ev, "/chapter/"+chapter1+"/cancel") {
		t.Fatalf("after queueing: %v %s", err, ev)
	}
	// Only the sections that changed are sent.
	if strings.Contains(ev, `id="live-now"`) || strings.Contains(ev, `id="live-actions"`) {
		t.Fatalf("unchanged sections sent again: %s", ev)
	}

	s.db.deleteSession(token)
	st.expectEnd(t)
}

func TestLiveSignedOutBeforeReconnect(t *testing.T) {
	_, ts := testServer(t)
	openStream(t, ts.URL+"/downloads", "expired").expectEnd(t)
}

func TestLiveEndsWhenPageGone(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	st := openStream(t, ts.URL+"/manga/"+mangaA, token)
	if _, err := st.next(); err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`DELETE FROM follows WHERE manga_id=?`, mangaA)
	s.db.Exec(`DELETE FROM chapters WHERE manga_id=?`, mangaA)
	if _, err := s.db.Exec(`DELETE FROM manga WHERE id=?`, mangaA); err != nil {
		t.Fatal(err)
	}
	s.dl.Changed()
	st.expectEnd(t)
}

func TestLiveUsesCurrentUser(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	st := openStream(t, ts.URL+"/manga/"+mangaA, token)
	if ev, err := st.next(); err != nil || strings.Contains(ev, "/delete") {
		t.Fatalf("non-admin sees delete: %v %s", err, ev)
	}
	s.db.Exec(`UPDATE users SET admin=1 WHERE id=?`, uid)
	s.dl.Changed()
	if ev, err := st.next(); err != nil || !strings.Contains(ev, "/chapter/"+chapter1+"/delete") {
		t.Fatalf("admin doesn't see delete: %v %s", err, ev)
	}
}

func TestLiveProgress(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	s.dl.mu.Lock()
	s.dl.current = &Progress{ChapterID: chapter2, MangaID: mangaA, MangaTitle: "Akanabe", Label: "Ch. 9.5", Total: 20}
	s.dl.mu.Unlock()
	if s.downloadingManga(httptest.NewRequest("GET", "/", nil)) {
		t.Fatal("no id should match nothing")
	}
	st := openStream(t, ts.URL+"/downloads", token)
	first, err := st.next()
	if err != nil {
		t.Fatal(err)
	}
	s.dl.progress(func(p *Progress) { p.Done = 7 })
	ev, err := st.next()
	if err != nil || ev == first || !strings.Contains(ev, `value="7"`) {
		t.Fatalf("progress didn't update the page: %v %s", err, ev)
	}
	if strings.Contains(ev, `id="live-queue"`) || strings.Contains(ev, `id="live-history"`) {
		t.Fatalf("progress resent sections that didn't change: %s", ev)
	}
}

func TestLiveLibraryAndMangaHeader(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	lib := openStream(t, ts.URL+"/", token)
	manga := openStream(t, ts.URL+"/manga/"+mangaA, token)
	for _, st := range []*sseStream{lib, manga} {
		if _, err := st.next(); err != nil {
			t.Fatal(err)
		}
	}
	s.db.Exec(`UPDATE chapters SET state=2, files='["001.jpg"]' WHERE id=?`, chapter2)
	s.dl.Changed()
	if ev, err := lib.next(); err != nil || !strings.Contains(ev, `id="live-library"`) || !strings.Contains(ev, "2 / 2") {
		t.Fatalf("library didn't update: %v %s", err, ev)
	}
	ev, err := manga.next()
	if err != nil || !strings.Contains(ev, `id="live-facts"`) || !strings.Contains(ev, "2 of 2 chapters") || !strings.Contains(ev, `id="live-langs"`) {
		t.Fatalf("manga header didn't update: %v %s", err, ev)
	}
}

func TestMangaPageLanguages(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	s.db.insertChapter(&Chapter{ID: "c3", MangaID: mangaA, Number: "11", Lang: "fr", Title: "Chapitre onze", Published: 300})
	get := func(path string) string {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	langs := s.db.userLangs(uid)
	if len(langs) != 1 || langs[0] != "en" {
		t.Fatalf("seed user languages: %v", langs)
	}
	page := get("/manga/" + mangaA)
	if !strings.Contains(page, `<option value="en" selected>`) || strings.Contains(page, "Chapitre onze") {
		t.Fatal("page should start on the user's language")
	}
	if !strings.Contains(page, "Download all English chapters") {
		t.Fatal("download all should name the shown language")
	}
	page = get("/manga/" + mangaA + "?lang=all")
	if !strings.Contains(page, `<option value="all" selected>`) || !strings.Contains(page, "Chapitre onze") {
		t.Fatal("all languages should show every chapter")
	}

	// Several languages of your own get their own entry.
	s.db.setFollowLangs(uid, mangaA, []string{"en", "fr"})
	page = get("/manga/" + mangaA)
	if !strings.Contains(page, `<option value="" selected>Your languages (1 of 3 downloaded)`) || !strings.Contains(page, "Chapitre onze") {
		t.Fatal("several languages should start on your languages")
	}
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(ts.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: token}})
	client := &http.Client{Jar: jar}
	if _, err := client.PostForm(ts.URL+"/manga/"+mangaA+"/download-all", url.Values{"lang": {"en", "fr"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{chapter2, "c3"} {
		var state int
		s.db.QueryRow(`SELECT state FROM chapters WHERE id=?`, id).Scan(&state)
		if state != stateQueued {
			t.Fatalf("%s not queued by download all: %d", id, state)
		}
	}
}

func TestMangaPageNotFollowed(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	other := "2b3c4d5e-0000-4000-8000-000000000002"
	s.db.upsertManga(&Manga{ID: other, Title: "Nobody follows"})
	req, _ := http.NewRequest("GET", ts.URL+"/manga/"+other, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), "Follow this manga to fetch its chapters.") || strings.Contains(string(b), "Fetching") {
		t.Fatal("unfollowed manga should say to follow it")
	}
}

func TestSignOutWakesOnlyWhenSomethingWasRemoved(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s)
	woke := func() bool {
		select {
		case <-s.db.signedOut.wait():
			return true
		default:
			return false
		}
	}
	token, _ := s.db.createSession(uid, "browser", "test")
	s.db.signedOut.wait()
	s.db.deleteIdleSessions()
	s.db.deleteSession("unknown")
	if woke() {
		t.Fatal("woke with nothing removed")
	}
	ch := s.db.signedOut.wait()
	s.db.deleteSession(token)
	select {
	case <-ch:
	default:
		t.Fatal("didn't wake after a sign-out")
	}
}

func TestLiveOutlivesReadTimeout(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	s.closing = make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := s.httpServer()
	srv.ReadTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go serve(ctx, srv, ln)

	st := openStream(t, "http://"+ln.Addr().String()+"/downloads", token)
	if _, err := st.next(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * srv.ReadTimeout)
	s.db.queueChapter(chapter1, now())
	s.dl.Wake()
	if ev, err := st.next(); err != nil || !strings.Contains(ev, "/chapter/"+chapter1+"/cancel") {
		t.Fatalf("stream ended after the read timeout: %v %s", err, ev)
	}
}

func TestBoostedNavigationGetsPage(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	// A boosted link with the SSE extension loaded asks like this.
	req, _ := http.NewRequest("GET", ts.URL+"/downloads", nil)
	req.Header.Set("Accept", "text/html, text/event-stream")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") || !strings.Contains(string(b), "<html") {
		t.Fatalf("link got %q instead of the page", ct)
	}
}

func TestLivePageStillPlain(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	req, _ := http.NewRequest("GET", ts.URL+"/manga/"+mangaA, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), "<html") || !strings.Contains(string(b), `<div id="live-chapters">`) || !strings.Contains(string(b), `hx-sse:connect="/manga/`+mangaA+`?live=1"`) {
		t.Fatal("manga page should be a full page with its live section")
	}
}

func TestShutdownEndsStreams(t *testing.T) {
	s, _ := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	s.closing = make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s.httpServer(), ln) }()

	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/downloads", nil)
	req.Header.Set(liveHeader, "1")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body := bufio.NewReader(res.Body)
	if _, err := body.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(shutdownGrace):
		t.Fatal("open stream held up shutdown")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("shutdown took %v", d)
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("stream didn't end cleanly: %v", err)
	}
}

func TestShutdownGrace(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte("done"))
	})
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	mux.HandleFunc("/stuck", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		<-stuck
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, &http.Server{Handler: mux}, ln) }()
	base := "http://" + ln.Addr().String()

	stuckRes, err := http.Get(base + "/stuck")
	if err != nil {
		t.Fatal(err)
	}
	defer stuckRes.Body.Close()
	slow := make(chan string, 1)
	go func() {
		res, err := http.Get(base + "/slow")
		if err != nil {
			slow <- err.Error()
			return
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		slow <- string(b)
	}()
	time.Sleep(50 * time.Millisecond) // let /slow start

	start := time.Now()
	cancel()
	if got := <-slow; got != "done" {
		t.Fatalf("running request was cut off: %s", got)
	}
	select {
	case <-done:
	case <-time.After(shutdownGrace + time.Second):
		t.Fatal("stuck request kept the server up")
	}
	if d := time.Since(start); d < shutdownGrace-100*time.Millisecond {
		t.Fatalf("stopped after %v, before the grace period", d)
	}
	if _, err := io.ReadAll(stuckRes.Body); err == nil {
		t.Fatal("stuck request should have been cut off")
	}
	if _, err := http.Get(base + "/slow"); err == nil {
		t.Fatal("still taking connections")
	}
}

func TestErrorsArePages(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token, _ := s.db.createSession(uid, "browser", "test")
	get := func(path string, signedIn bool) (*http.Response, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if signedIn {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(b)
	}
	for _, path := range []string{"/manga/00000000-0000-0000-0000-000000000000", "/no/such/page"} {
		res, body := get(path, true)
		if res.StatusCode != http.StatusNotFound || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %s", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
		if !strings.Contains(body, "Page not found") || !strings.Contains(body, `href="/downloads"`) {
			t.Fatalf("%s: not a full page with the header: %s", path, body)
		}
	}
	if _, body := get("/no/such/page", false); strings.Contains(body, `href="/downloads"`) {
		t.Fatal("header shown when signed out")
	}
	req, _ := http.NewRequest("POST", ts.URL+"/chapter/"+chapter1+"/delete", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "You don&#39;t have permission to do this") || !strings.Contains(string(b), "<html") {
		t.Fatalf("admin-only: %d %s", res.StatusCode, b)
	}
	res, body := get("/api/v1/nope", false)
	if res.StatusCode != http.StatusNotFound || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("api: %d %s %s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
}

func TestQueueOrder(t *testing.T) {
	s, _ := testServer(t)
	seed(t, s)
	const mangaB = "0d1f6a52-9c1e-4f43-9a37-3a8c0f1f2b11"
	s.db.upsertManga(&Manga{ID: mangaB, Title: "Bocchi"})
	// Series A is queued first. Its chapters were uploaded out of order.
	s.db.insertChapter(&Chapter{ID: "a2", MangaID: mangaA, Volume: "1", Number: "2", Lang: "en", Published: 50})
	s.db.insertChapter(&Chapter{ID: "a1", MangaID: mangaA, Volume: "1", Number: "1", Lang: "en", Published: 900})
	s.db.insertChapter(&Chapter{ID: "a3", MangaID: mangaA, Volume: "2", Number: "3", Lang: "en", Published: 60})
	// Series B is queued later, though its chapters are older.
	s.db.insertChapter(&Chapter{ID: "b1", MangaID: mangaB, Number: "1", Lang: "en", Published: 1})
	s.db.insertChapter(&Chapter{ID: "b2", MangaID: mangaB, Number: "2", Lang: "en", Published: 2})
	for _, id := range []string{"a2", "a1", "a3"} {
		s.db.queueChapter(id, 1000)
	}
	for _, id := range []string{"b2", "b1"} {
		s.db.queueChapter(id, 2000)
	}
	// A new chapter of A found after B was queued goes after B.
	s.db.insertChapter(&Chapter{ID: "a4", MangaID: mangaA, Volume: "2", Number: "4", Lang: "en", Published: 3000, State: stateQueued, Queued: 3000})

	want := "a1 a2 a3 b1 b2 a4"
	queue, err := s.db.queue()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range queue {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("queue page: %s, want %s", got, want)
	}
	ids = nil
	for {
		c, err := s.db.nextQueued()
		if err != nil {
			break
		}
		ids = append(ids, c.ID)
		s.db.setChapterState(c.ID, stateDone)
	}
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("download order: %s, want %s", got, want)
	}
}

func TestQueuedColumnAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE chapters DROP COLUMN queued`,
		`INSERT INTO manga(id,title,added) VALUES('` + mangaA + `','Akanabe',1)`,
		`INSERT INTO chapters(id,manga_id,lang,published,state) VALUES('old','` + mangaA + `','en',123,1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var queued int64
	if err := db.QueryRow(`SELECT queued FROM chapters WHERE id='old'`).Scan(&queued); err != nil || queued != 123 {
		t.Fatalf("queued = %d, %v", queued, err)
	}
}

// Kotatsu sync

// syncManga is a manga object shaped like the app's MangaSyncDto.
func syncManga(id int64, url string) string {
	return fmt.Sprintf(`{"manga_id":%d,"title":"Manga %d","alt_title":null,"url":%q,"public_url":"https://mangadex.org/title/%s","rating":-1.0,"content_rating":null,"cover_url":"https://example.org/c.jpg","large_cover_url":null,"tags":[{"tag_id":5,"title":"Comedy","key":"comedy","source":%q}],"state":"ONGOING","author":null,"source":%q}`,
		id, id, url, url, kotodexSource, kotodexSource)
}

func historyRow(mangaID, chapterID, updated, deleted int64, page int) string {
	return fmt.Sprintf(`{"manga_id":%d,"created_at":1000,"updated_at":%d,"chapter_id":%d,"page":%d,"scroll":0.0,"percent":0.25,"deleted_at":%d,"chapters":12,"manga":%s}`,
		mangaID, updated, chapterID, page, deleted, syncManga(mangaID, mangaA))
}

func syncAuth(t *testing.T, ts *httptest.Server, email, password string) (int, string) {
	t.Helper()
	res, err := http.Post(ts.URL+"/auth", "application/json", strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func syncPost(t *testing.T, ts *httptest.Server, token, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func syncToken(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	code, body := syncAuth(t, ts, "mio@kotodex.home.arpa", testSyncPassword)
	var out struct{ Token string }
	json.Unmarshal([]byte(body), &out)
	if code != http.StatusOK || out.Token == "" {
		t.Fatalf("sign in: %d %s", code, body)
	}
	return out.Token
}

func TestSyncAuth(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	for _, email := range []string{"mio@kotodex.home.arpa", "Mio@manga.example.com", "mio"} {
		if code, body := syncAuth(t, ts, email, testSyncPassword); code != http.StatusOK || !strings.Contains(body, `"token"`) {
			t.Errorf("%s: %d %s", email, code, body)
		}
	}
	// The app shows the reply as the error, with the quotes removed.
	if code, body := syncAuth(t, ts, "mio@kotodex.home.arpa", "nope"); code != http.StatusUnauthorized || strings.TrimSpace(body) != `"Wrong username or sync password"` {
		t.Errorf("wrong password: %d %s", code, body)
	}
	// The Kotodex password doesn't work for sync.
	if code, _ := syncAuth(t, ts, "mio@kotodex.home.arpa", "pw"); code != http.StatusUnauthorized {
		t.Errorf("Kotodex password: %d", code)
	}
	sessions, _ := s.db.sessions(uid, "")
	n := 0
	for _, se := range sessions {
		if se.Kind == "sync" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("sync sessions = %d, want 3", n)
	}
	if code, _ := syncPost(t, ts, "bad", "/resource/history", `{"timestamp":1}`); code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", code)
	}
	// A sync sign-in only works for sync, and other sign-ins don't work for sync.
	syncTok := syncToken(t, ts)
	plugin, _ := s.db.createSession(uid, "kotatsu", "test")
	for _, c := range []struct {
		path, token string
		want        int
	}{
		{"/api/v1/me", syncTok, http.StatusUnauthorized},
		{"/settings", syncTok, http.StatusSeeOther},
		{"/api/v1/me", plugin, http.StatusOK},
	} {
		req, _ := http.NewRequest("GET", ts.URL+c.path, nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if res, err := client.Do(req); err != nil || res.StatusCode != c.want {
			t.Errorf("%s: %v %v, want %d", c.path, res.StatusCode, err, c.want)
		}
	}
	if code, _ := syncPost(t, ts, plugin, "/resource/history", `{"timestamp":1}`); code != http.StatusUnauthorized {
		t.Errorf("plugin token on sync: %d", code)
	}
	// A browser's session cookie can't write sync data.
	web, _ := s.db.createSession(uid, "web", "test")
	req, _ := http.NewRequest("POST", ts.URL+"/resource/history", strings.NewReader(`{"history":[`+historyRow(1, 1, 1, 0, 0)+`],"timestamp":1}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: web})
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Errorf("cookie only: %v %v", res.StatusCode, err)
	}
}

func TestSyncHistory(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token := syncToken(t, ts)
	events := func() int {
		var n int
		s.db.QueryRow(`SELECT count(*) FROM reading_events WHERE user_id=?`, uid).Scan(&n)
		return n
	}

	// First sync: the reply would be what was sent, so there's nothing to send back.
	body := `{"history":[` + historyRow(-5, 11, 2000, 0, 3) + `,` + historyRow(7, 12, 2500, 0, 0) + `],"timestamp":1}`
	if code, b := syncPost(t, ts, token, "/resource/history", body); code != http.StatusNoContent {
		t.Fatalf("first sync: %d %s", code, b)
	}
	if events() != 2 {
		t.Fatalf("events = %d, want 2", events())
	}
	// The same upload again logs nothing new.
	syncPost(t, ts, token, "/resource/history", body)
	if events() != 2 {
		t.Fatalf("repeat upload logged events: %d", events())
	}

	// A phone with older progress for -5 and nothing for 7: the newer row
	// stays, and the reply carries both rows with every field the app expects.
	code, b := syncPost(t, ts, token, "/resource/history", `{"history":[`+historyRow(-5, 10, 1500, 0, 9)+`],"timestamp":1}`)
	if code != http.StatusOK {
		t.Fatalf("stale upload: %d %s", code, b)
	}
	var reply struct {
		History   []map[string]json.RawMessage
		Timestamp int64
	}
	if err := json.Unmarshal(b, &reply); err != nil || len(reply.History) != 2 || reply.Timestamp == 0 {
		t.Fatalf("reply: %v %s", err, b)
	}
	want := []string{"chapter_id", "chapters", "created_at", "deleted_at", "manga", "manga_id", "page", "percent", "scroll", "updated_at"}
	for _, row := range reply.History {
		keys := slices.Sorted(maps.Keys(row))
		if !slices.Equal(keys, want) {
			t.Errorf("row fields = %v", keys)
		}
	}
	if string(reply.History[0]["manga"]) != syncManga(-5, mangaA) {
		t.Errorf("manga changed on the way back:\n%s\n%s", reply.History[0]["manga"], syncManga(-5, mangaA))
	}
	if string(reply.History[0]["chapter_id"]) != "11" || events() != 2 {
		t.Errorf("stale progress won: chapter %s, %d events", reply.History[0]["chapter_id"], events())
	}

	// Newer progress replaces the row and is logged.
	syncPost(t, ts, token, "/resource/history", `{"history":[`+historyRow(-5, 12, 3000, 0, 1)+`],"timestamp":1}`)
	var chapter, deleted int64
	s.db.QueryRow(`SELECT chapter_id,deleted_at FROM sync_history WHERE user_id=? AND manga_id=-5`, uid).Scan(&chapter, &deleted)
	if chapter != 12 || events() != 3 {
		t.Errorf("newer progress: chapter %d, %d events", chapter, events())
	}
	// Removing it on the phone sticks, even with the same save time, and
	// keeps its reading in the log.
	syncPost(t, ts, token, "/resource/history", `{"history":[`+historyRow(-5, 12, 3000, 4000, 1)+`],"timestamp":1}`)
	s.db.QueryRow(`SELECT deleted_at FROM sync_history WHERE user_id=? AND manga_id=-5`, uid).Scan(&deleted)
	if deleted != 4000 || events() != 3 {
		t.Errorf("removal: deleted_at %d, %d events", deleted, events())
	}

	// A big library goes through in batches.
	var rows []string
	for i := range 450 {
		rows = append(rows, historyRow(int64(1000+i), 1, int64(5000+i), 0, 0))
	}
	if code, b := syncPost(t, ts, token, "/resource/history", `{"history":[`+strings.Join(rows, ",")+`],"timestamp":1}`); code != http.StatusOK {
		t.Fatalf("big upload: %d %s", code, b)
	}
	if events() != 453 {
		t.Errorf("events after big upload = %d", events())
	}

	// A manga that doesn't match its row is refused, with a reason.
	bad := strings.Replace(historyRow(8, 1, 1, 0, 0), `"manga":{"manga_id":8`, `"manga":{"manga_id":9`, 1)
	if code, b := syncPost(t, ts, token, "/resource/history", `{"history":[`+bad+`],"timestamp":1}`); code != http.StatusBadRequest {
		t.Errorf("mismatched manga: %d %s", code, b)
	}
}

func TestSyncFavourites(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	token := syncToken(t, ts)
	cats := `"categories":[{"category_id":1,"created_at":10,"sort_key":0,"title":"Reading","order":"NEWEST","track":true,"show_in_lib":true,"deleted_at":0},` +
		`{"category_id":2,"created_at":11,"sort_key":1,"title":"Dropped","order":"NEWEST","track":false,"show_in_lib":true,"deleted_at":0}]`
	fav := func(manga, cat, created, deleted int64) string {
		return fmt.Sprintf(`{"manga_id":%d,"manga":%s,"category_id":%d,"sort_key":0,"pinned":false,"created_at":%d,"deleted_at":%d}`,
			manga, syncManga(manga, mangaA), cat, created, deleted)
	}
	kinds := func() string {
		rows, _ := s.db.Query(`SELECT kind,category_id FROM list_events WHERE user_id=? ORDER BY at,kind`, uid)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var k string
			var c int
			rows.Scan(&k, &c)
			out = append(out, fmt.Sprintf("%s %d", k, c))
		}
		return strings.Join(out, ", ")
	}

	body := `{` + cats + `,"favourites":[` + fav(-5, 1, 100, 0) + `],"timestamp":1}`
	if code, b := syncPost(t, ts, token, "/resource/favourites", body); code != http.StatusNoContent {
		t.Fatalf("first sync: %d %s", code, b)
	}
	// Moving it to Dropped removes it from one category and adds it to the other.
	code, b := syncPost(t, ts, token, "/resource/favourites", `{`+cats+`,"favourites":[`+fav(-5, 1, 100, 200)+`,`+fav(-5, 2, 200, 0)+`],"timestamp":1}`)
	if code != http.StatusNoContent {
		t.Fatalf("move: %d %s", code, b)
	}
	if got := kinds(); got != "added 1, added 2, removed 1" {
		t.Errorf("list events = %q", got)
	}
	// Another phone that hasn't caught up gets everything back.
	code, b = syncPost(t, ts, token, "/resource/favourites", `{"categories":[],"favourites":[],"timestamp":1}`)
	var reply struct {
		Categories []map[string]json.RawMessage
		Favourites []map[string]json.RawMessage
	}
	if err := json.Unmarshal(b, &reply); code != http.StatusOK || err != nil || len(reply.Categories) != 2 || len(reply.Favourites) != 2 {
		t.Fatalf("catch up: %d %s", code, b)
	}
	if keys := slices.Sorted(maps.Keys(reply.Favourites[0])); !slices.Equal(keys, []string{"category_id", "created_at", "deleted_at", "manga", "manga_id", "pinned", "sort_key"}) {
		t.Errorf("favorite fields = %v", keys)
	}
	if keys := slices.Sorted(maps.Keys(reply.Categories[0])); !slices.Equal(keys, []string{"category_id", "created_at", "deleted_at", "order", "show_in_lib", "sort_key", "title", "track"}) {
		t.Errorf("category fields = %v", keys)
	}
}

func TestStableID(t *testing.T) {
	// 64-bit FNV-1a test vector, and the plugin's IDs are the same hash.
	if got := uint64(stableID("a")); got != 0xaf63dc4c8601ec8c {
		t.Errorf("stableID(a) = %x", got)
	}
}

// otherManga is a manga from a source that isn't Kotodex.
func otherManga(id int64, title string) string {
	return fmt.Sprintf(`{"manga_id":%d,"title":%q,"alt_title":null,"url":"/m/1","public_url":"https://example.org/m/1","rating":-1.0,"content_rating":null,"cover_url":"https://example.org/c.jpg","large_cover_url":null,"tags":[],"state":null,"author":null,"source":"MANGAPARK"}`, id, title)
}

func webGet(t *testing.T, ts *httptest.Server, token, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// syncReading signs mio's app in and syncs a session on a Kotodex manga and
// some reading from another source, plus a list with both.
func syncReading(t *testing.T, ts *httptest.Server) {
	t.Helper()
	token := syncToken(t, ts)
	now := time.Now().UnixMilli()
	kid := stableID("manga:" + mangaA)
	kotodex := func(updated int64, page int) string {
		return fmt.Sprintf(`{"manga_id":%d,"created_at":1000,"updated_at":%d,"chapter_id":%d,"page":%d,"scroll":0.0,"percent":0.4,"deleted_at":0,"chapters":2,"manga":%s}`,
			kid, updated, stableID("chapter:"+chapter1), page, syncManga(kid, mangaA))
	}
	other := fmt.Sprintf(`{"manga_id":77,"created_at":1000,"updated_at":%d,"chapter_id":5,"page":0,"scroll":0.0,"percent":0.5,"deleted_at":0,"chapters":10,"manga":%s}`,
		now-60_000, otherManga(77, "Secret Manga"))
	for _, body := range []string{
		`{"history":[` + kotodex(now-20*60_000, 0) + `],"timestamp":1}`,
		`{"history":[` + kotodex(now-5*60_000, 3) + `,` + other + `],"timestamp":1}`,
	} {
		if code, b := syncPost(t, ts, token, "/resource/history", body); code >= 300 && code != http.StatusNoContent {
			t.Fatalf("history: %d %s", code, b)
		}
	}
	favs := fmt.Sprintf(`{"categories":[{"category_id":1,"created_at":10,"sort_key":0,"title":"Plan to read","order":"NEWEST","track":true,"show_in_lib":true,"deleted_at":0}],"favourites":[`+
		`{"manga_id":%d,"manga":%s,"category_id":1,"sort_key":0,"pinned":false,"created_at":%d,"deleted_at":0},`+
		`{"manga_id":77,"manga":%s,"category_id":1,"sort_key":0,"pinned":false,"created_at":%d,"deleted_at":0}],"timestamp":1}`,
		kid, syncManga(kid, mangaA), now-30*60_000, otherManga(77, "Secret Manga"), now-30*60_000)
	if code, b := syncPost(t, ts, token, "/resource/favourites", favs); code >= 300 && code != http.StatusNoContent {
		t.Fatalf("favourites: %d %s", code, b)
	}
}

func TestReadingSessions(t *testing.T) {
	s, ts := testServer(t)
	uid := seed(t, s)
	syncReading(t, ts)
	list, err := s.db.readSessions(uid, uid, 0, mangaA)
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions: %v %d", err, len(list))
	}
	if d := list[0].Duration(); d != 15*time.Minute {
		t.Errorf("duration = %v", d)
	}
	if d := s.db.readingTimes(uid, mangaA)[uid]; d != 15*time.Minute {
		t.Errorf("reading time = %v, want the same as the session", d)
	}
	// A save more than half an hour later starts a new session.
	s.db.Exec(`INSERT INTO reading_events(user_id,manga_id,chapter_id,page,percent,at) VALUES(?,?,?,?,?,?)`,
		uid, stableID("manga:"+mangaA), stableID("chapter:"+chapter1), 5, 0.5, time.Now().Add(40*time.Minute).UnixMilli())
	if list, _ = s.db.readSessions(uid, uid, 0, mangaA); len(list) != 2 {
		t.Errorf("sessions after a gap = %d, want 2", len(list))
	}
}

func TestReadingSharing(t *testing.T) {
	s, ts := testServer(t)
	mio := seed(t, s)
	syncReading(t, ts)
	rin, _ := s.db.createUser("rin", "pw", false)
	boss, _ := s.db.createUser("boss", "pw", true)
	token := func(uid int64) string {
		tk, err := s.db.createSession(uid, "web", "test")
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	mioT, rinT, bossT := token(mio), token(rin), token(boss)
	const where = "Ch. 10, page 4"

	for _, c := range []struct {
		share                string
		kotodex, other, list bool
	}{
		{shareAll, true, true, true},
		{shareKotodex, true, false, true},
		{shareNone, false, false, false},
	} {
		if err := s.db.setShare(mio, c.share); err != nil {
			t.Fatal(err)
		}
		// An admin sees the same as anyone else.
		for _, viewer := range []string{rinT, bossT} {
			_, profile := webGet(t, ts, viewer, "/people/mio")
			_, activity := webGet(t, ts, viewer, "/activity")
			_, manga := webGet(t, ts, viewer, "/manga/"+mangaA)
			if strings.Contains(profile, where) != c.kotodex || strings.Contains(activity, "Read Ch. 10") != c.kotodex || strings.Contains(manga, where) != c.kotodex {
				t.Errorf("%s: Kotodex reading shown = %v %v %v", c.share, strings.Contains(profile, where), strings.Contains(activity, "Read Ch. 10"), strings.Contains(manga, where))
			}
			if strings.Contains(profile, "Secret Manga") != c.other || strings.Contains(activity, "Secret Manga") != c.other {
				t.Errorf("%s: other source shown = %v %v", c.share, strings.Contains(profile, "Secret Manga"), strings.Contains(activity, "Secret Manga"))
			}
			if strings.Contains(manga, "Plan to read") != c.list {
				t.Errorf("%s: list on manga page = %v", c.share, strings.Contains(manga, "Plan to read"))
			}
			if code, _ := webGet(t, ts, viewer, "/people/mio/lists/1"); (code == http.StatusOK) != c.list {
				t.Errorf("%s: list page = %d", c.share, code)
			}
			// What mio follows shows either way.
			if !strings.Contains(profile, "Akanabe") {
				t.Errorf("%s: follows hidden", c.share)
			}
		}
		// mio always sees all of it.
		_, own := webGet(t, ts, mioT, "/people/mio")
		if !strings.Contains(own, where) || !strings.Contains(own, "Secret Manga") {
			t.Errorf("%s: own profile is missing reading", c.share)
		}
	}
	if code, _ := webGet(t, ts, rinT, "/people/mio/reading"); code != http.StatusSeeOther {
		t.Errorf("reading page while sharing nothing = %d", code)
	}
	if code, _ := webGet(t, ts, rinT, "/people/nobody"); code != http.StatusNotFound {
		t.Errorf("unknown person = %d", code)
	}
}

func TestReadingPages(t *testing.T) {
	s, ts := testServer(t)
	mio := seed(t, s)
	syncReading(t, ts)
	tk, _ := s.db.createSession(mio, "web", "test")
	for _, path := range []string{"/activity", "/activity?show=mine", "/people/mio", "/people/mio/reading", "/people/mio/reading?tab=finished&sort=title&q=sec",
		"/people/mio/lists/1?sort=added", "/people/mio/log", "/people/mio/following", "/manga/" + mangaA, "/settings"} {
		code, body := webGet(t, ts, tk, path)
		if code != http.StatusOK || !strings.Contains(body, "</html>") {
			t.Errorf("%s: %d", path, code)
		}
	}
	_, manga := webGet(t, ts, tk, "/manga/"+mangaA)
	for _, want := range []string{"Page 4", "In lists", "Plan to read (1)", "(you)"} {
		if !strings.Contains(manga, want) {
			t.Errorf("manga page is missing %q", want)
		}
	}
	_, settings := webGet(t, ts, tk, "/settings")
	if !strings.Contains(settings, "Kotatsu sync") || !strings.Contains(settings, `data-sync-email="mio"`) {
		t.Error("settings is missing the sync card")
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.20": false, "172.16.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fd00::1": false,
		"::ffff:127.0.0.1": false, "64:ff9b::a00:1": false, "2002:c0a8:101::1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v", addr, got)
		}
	}
}

// pngBytes is a 1x1 PNG.
var pngBytes = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 0x49, 0x48, 0x44, 0x52, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89, 0, 0, 0, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1, 0x0d, 0x0a, 0x2d, 0xb4, 0, 0, 0, 0, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

func TestOtherCovers(t *testing.T) {
	s, ts := testServer(t)
	mio := seed(t, s)
	syncReading(t, ts)
	var agent sync.Map // the test site's handler runs on its own goroutines
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent.Store("ua", r.UserAgent())
		if r.URL.Path == "/page.html" {
			w.Write([]byte("<html>not a cover</html>"))
			return
		}
		w.Write(pngBytes)
	}))
	defer site.Close()

	// The real client won't fetch from this machine, or anything else that
	// isn't on the public internet.
	if err := s.covers.fetch(context.Background(), site.URL+"/c.png"); err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("fetched from a local address: %v", err)
	}

	s.covers.client = site.Client() // allowed to reach the test site
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.covers.Run(ctx)
	s.db.Exec(`UPDATE sync_manga SET cover=? WHERE id=77`, site.URL+"/c.png")
	tk, _ := s.db.createSession(mio, "web", "test")

	// Off by default: nothing is downloaded.
	webGet(t, ts, tk, "/people/mio")
	time.Sleep(200 * time.Millisecond)
	if _, asked := agent.Load("ua"); s.covers.cached(site.URL+"/c.png") != "" || asked {
		t.Fatal("downloaded a cover with the setting off")
	}

	s.db.setSetting("other_covers", "1")
	webGet(t, ts, tk, "/people/mio")
	for i := 0; i < 50 && s.covers.cached(site.URL+"/c.png") == ""; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if ua, _ := agent.Load("ua"); ua != coverAgent {
		t.Errorf("User-Agent = %v", ua)
	}
	_, page := webGet(t, ts, tk, "/people/mio")
	i := strings.Index(page, "/other/")
	if i < 0 {
		t.Fatal("no cover link on the profile")
	}
	start := strings.LastIndex(page[:i], `"`) + 1
	link := page[start : start+strings.Index(page[start:], `"`)]
	res, err := http.Get(ts.URL + link)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("cover link %s: %v %v", link, res.StatusCode, err)
	}
	res.Body.Close()

	// Something that isn't an image is never kept.
	if err := s.covers.fetch(ctx, site.URL+"/page.html"); err == nil || s.covers.cached(site.URL+"/page.html") != "" {
		t.Errorf("kept a page that isn't an image: %v", err)
	}
}

func TestCoverLimit(t *testing.T) {
	s, _ := testServer(t)
	c := s.covers
	if !c.reserve(10) {
		t.Fatal("default limit refused 10 bytes")
	}
	s.db.setSetting("cover_limit", "1")
	if c.reserve(2 << 20) {
		t.Error("went over a 1 MB limit")
	}
	s.db.setSetting("cover_limit", "0")
	if !c.reserve(4 << 30) {
		t.Error("0 should mean no limit")
	}
	s.db.setSetting("cover_limit", "nonsense")
	if s.db.settings().CoverLimitMB != 1024 {
		t.Error("a bad stored value should fall back to the default")
	}
}

func TestLangFlag(t *testing.T) {
	for code, want := range map[string]string{"en": "🇬🇧", "es-la": "🇲🇽", "pt-br": "🇧🇷", "eo": "", "xx": ""} {
		if got := langFlag(code); got != want {
			t.Errorf("langFlag(%s) = %q, want %q", code, got, want)
		}
	}
	// A wrong code in the table shows no flag rather than junk.
	langFlags["zz"] = "gb"
	defer delete(langFlags, "zz")
	if got := langFlag("zz"); got != "" {
		t.Errorf("lowercase code gave %q", got)
	}
}

func TestChapterColumns(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	all, shown := chapterColumns(r)
	if len(all) != 7 || len(shown) != 7 || all[0].Name != "title" {
		t.Fatalf("defaults: %v", all)
	}
	// Order and hidden ones from the cookie, made-up and repeated names
	// skipped, and columns it doesn't mention added at the end.
	r.AddCookie(&http.Cookie{Name: colsCookie, Value: "status.-group.lang.bogus.lang.title"})
	all, shown = chapterColumns(r)
	var names []string
	for _, c := range shown {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "status,lang,title,date,pages,you" || len(all) != 7 {
		t.Errorf("shown = %s, all = %d", got, len(all))
	}
}

func TestDeleteReading(t *testing.T) {
	s, ts := testServer(t)
	mio := seed(t, s)
	syncReading(t, ts)
	rin, _ := s.db.createUser("rin", "pw", false)
	kid := stableID("manga:" + mangaA)
	s.db.Exec(`INSERT INTO reading_events(user_id,manga_id,chapter_id,page,percent,at) VALUES(?,?,1,1,0.1,?)`, rin, kid, time.Now().UnixMilli())
	events := func(uid int64) int {
		var n int
		s.db.QueryRow(`SELECT count(*) FROM reading_events WHERE user_id=?`, uid).Scan(&n)
		return n
	}
	list, _ := s.db.readSessions(mio, mio, 0, mangaA)
	if len(list) != 1 {
		t.Fatalf("sessions = %d", len(list))
	}
	before := events(mio)
	tk, _ := s.db.createSession(mio, "web", "test")
	_, page := webGet(t, ts, tk, "/people/mio/log")
	if !strings.Contains(page, `action="/reading/delete"`) {
		t.Error("own log has no delete buttons")
	}
	if _, page := webGet(t, ts, tk, "/people/mio"); !strings.Contains(page, `action="/reading/delete"`) {
		t.Error("own profile has no delete buttons")
	}
	rtk, _ := s.db.createSession(rin, "web", "test")
	if _, page := webGet(t, ts, rtk, "/people/mio/log"); strings.Contains(page, `action="/reading/delete"`) {
		t.Error("someone else's log has delete buttons")
	}

	post := func(token string, rs *ReadSession) int {
		form := url.Values{"manga": {strconv.FormatInt(rs.SyncedManga.ID, 10)}, "start": {strconv.FormatInt(rs.Start, 10)},
			"end": {strconv.FormatInt(rs.End, 10)}, "next": {"/people/mio/log"}}
		req, _ := http.NewRequest("POST", ts.URL+"/reading/delete", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	// Someone else posting the same session deletes only their own reading.
	all := &ReadSession{SyncedManga: SyncedManga{ID: kid}, Start: 0, End: time.Now().Add(time.Hour).UnixMilli()}
	post(rtk, all)
	if events(mio) != before || events(rin) != 0 {
		t.Errorf("after rin's delete: mio %d (was %d), rin %d", events(mio), before, events(rin))
	}
	if code := post(tk, list[0]); code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	if list, _ := s.db.readSessions(mio, mio, 0, mangaA); len(list) != 0 {
		t.Errorf("session still there: %d", len(list))
	}
	if events(mio) != before-list[0].Saves {
		t.Errorf("events = %d, want only the session's %d gone from %d", events(mio), list[0].Saves, before)
	}
	// Reading right at the profile's one-year cut may be cut short there, so
	// it can only be deleted from the full log.
	s.db.Exec(`INSERT INTO reading_events(user_id,manga_id,chapter_id,page,percent,at) VALUES(?,77,5,0,0.5,?)`,
		mio, time.Now().AddDate(0, 0, -371).Add(time.Minute).UnixMilli())
	if _, page := webGet(t, ts, tk, "/people/mio"); strings.Contains(page, `action="/reading/delete"`) {
		t.Error("profile offers to delete a session that may be cut short")
	}
	if _, page := webGet(t, ts, tk, "/people/mio/log"); !strings.Contains(page, `action="/reading/delete"`) {
		t.Error("full log lost its delete buttons")
	}
}

func TestCoversFull(t *testing.T) {
	s, _ := testServer(t)
	c := s.covers
	var asked atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Write(pngBytes)
	}))
	defer site.Close()
	c.client = site.Client()
	s.db.setSetting("cover_limit", "1")
	if !c.reserve(1 << 20) {
		t.Fatal("couldn't fill the limit")
	}
	// Full: nothing is queued or downloaded, and it isn't remembered as a
	// failure, so raising the limit lets it through.
	c.want(site.URL + "/c.png")
	if len(c.queue) != 0 {
		t.Error("queued a cover with the storage full")
	}
	err := c.fetch(context.Background(), site.URL+"/c.png")
	if !errors.Is(err, errCoversFull) || asked.Load() != 0 {
		t.Errorf("fetch with storage full: %v, %d requests", err, asked.Load())
	}
	s.db.setSetting("cover_limit", "2")
	c.want(site.URL + "/c.png")
	if len(c.queue) != 1 {
		t.Error("not queued after raising the limit")
	}

	// A cover that can't be saved gives its space back.
	size := c.size
	c.dir = filepath.Join(t.TempDir(), "file")
	os.WriteFile(c.dir, nil, 0o644) // MkdirAll fails on a file
	if err := c.fetch(context.Background(), site.URL+"/d.png"); err == nil {
		t.Fatal("saved into a file")
	}
	if c.size != size {
		t.Errorf("size = %d after a failed save, want %d", c.size, size)
	}
}

func TestCoverFailuresForgetOldest(t *testing.T) {
	s, _ := testServer(t)
	c := s.covers
	now := time.Now()
	for i := range coverMaxFails {
		c.failed[fmt.Sprint("https://x/", i)] = now.Add(-time.Duration(coverMaxFails-i) * time.Second)
	}
	c.want("https://x/new")
	if _, ok := c.failed["https://x/0"]; ok {
		t.Error("oldest failure kept")
	}
	if _, ok := c.failed[fmt.Sprint("https://x/", coverMaxFails-1)]; !ok {
		t.Error("newest failure forgotten")
	}
	if len(c.failed) < coverMaxFails*8/10 {
		t.Errorf("forgot too many: %d left", len(c.failed))
	}
}

func TestCoverLimitTooBig(t *testing.T) {
	s, ts := testServer(t)
	admin, _ := s.db.createUser("ad", "pw", true)
	tk, _ := s.db.createSession(admin, "web", "test")
	form := url.Values{"interval": {"60"}, "cover_limit": {"9000000000000"}}
	req, _ := http.NewRequest("POST", ts.URL+"/settings/server", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tk})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := s.db.setting("cover_limit"); got == "9000000000000" {
		t.Error("saved a limit that overflows")
	}
	s.db.setSetting("cover_limit", "9000000000000")
	if s.covers.limit() <= 0 {
		t.Error("a huge stored limit wrapped around")
	}
}

func TestPageQueryDropsLive(t *testing.T) {
	r := httptest.NewRequest("GET", "/manga/x?lang=en&live=1", nil)
	if q := pageQuery(r); q != "lang=en" {
		t.Errorf("pageQuery = %q", q)
	}
}

func TestSyncPassword(t *testing.T) {
	s, ts := testServer(t)
	uid, err := s.db.createUser("ritsu", "a-long-kotodex-password-over-24-characters", false)
	if err != nil {
		t.Fatal(err)
	}
	// No sync password yet: nothing signs in, not even an empty one.
	for _, pw := range []string{"", "a-long-kotodex-password-over-24-characters"} {
		if code, _ := syncAuth(t, ts, "ritsu@kotodex.home.arpa", pw); code != http.StatusUnauthorized {
			t.Errorf("before making one, %q: %d", pw, code)
		}
	}
	token, _ := s.db.createSession(uid, "browser", "test")
	makeOne := func() (string, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/settings/sync-password", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		m := regexp.MustCompile(`<dt>Sync password</dt><dd><code>([^<]+)</code>`).FindSubmatch(b)
		if res.StatusCode != http.StatusOK || m == nil {
			t.Fatalf("no sync password shown: %d", res.StatusCode)
		}
		return string(m[1]), res.Header.Get("Cache-Control")
	}
	first, cache := makeOne()
	if len(first) > 24 {
		t.Errorf("sync password is %d characters, Kotatsu takes 24", len(first))
	}
	if cache != "no-store" {
		t.Errorf("Cache-Control = %q", cache)
	}
	code, body := syncAuth(t, ts, "ritsu@kotodex.home.arpa", first)
	if code != http.StatusOK {
		t.Fatalf("sign in: %d %s", code, body)
	}
	var out struct{ Token string }
	json.Unmarshal([]byte(body), &out)

	// It's shown once.
	req, _ := http.NewRequest("GET", ts.URL+"/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	res, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(b), first) || !strings.Contains(string(b), "New sync password") {
		t.Error("settings page should say one was made without showing it")
	}

	// A new one replaces the old one. The phone already signed in stays.
	second, _ := makeOne()
	if second == first {
		t.Fatal("same password twice")
	}
	if code, _ := syncAuth(t, ts, "ritsu@kotodex.home.arpa", first); code != http.StatusUnauthorized {
		t.Errorf("old sync password: %d", code)
	}
	if code, _ := syncAuth(t, ts, "ritsu@kotodex.home.arpa", second); code != http.StatusOK {
		t.Errorf("new sync password: %d", code)
	}
	if code, _ := syncPost(t, ts, out.Token, "/resource/history", `{"timestamp":1}`); code == http.StatusUnauthorized {
		t.Error("phone signed out by a new sync password")
	}
	// It doesn't sign in anywhere but sync.
	res, _ = http.Post(ts.URL+"/api/v1/login", "application/json", strings.NewReader(fmt.Sprintf(`{"username":"ritsu","password":%q}`, second)))
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("plugin sign-in with sync password: %d", res.StatusCode)
	}
}
