package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		mangaDir: mangaDir, limiter: &loginLimiter{fails: map[string][]time.Time{}},
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

func seed(t *testing.T, s *Server) int64 {
	t.Helper()
	uid, err := s.db.createUser("mio", "pw", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.upsertManga(&Manga{ID: mangaA, Title: "Akanabe", Tags: []string{"Comedy"}, Cover: "x.jpg"}); err != nil {
		t.Fatal(err)
	}
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
	if strings.Contains(page(res), "evil.example") {
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
	req.Header.Set("Accept", "text/event-stream")
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
	if !strings.HasPrefix(ev, "<div data-live>") || strings.Contains(ev, "<html") || !strings.Contains(ev, "Nothing waiting to download.") {
		t.Fatalf("first event: %s", ev)
	}

	s.db.setChapterState(chapter1, stateQueued)
	s.dl.Wake()
	if ev, err = st.next(); err != nil || !strings.Contains(ev, "/chapter/"+chapter1+"/cancel") {
		t.Fatalf("after queueing: %v %s", err, ev)
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
	if err != nil || ev == first {
		t.Fatalf("progress didn't update the page: %v %s", err, ev)
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
	s.db.setChapterState(chapter1, stateQueued)
	s.dl.Wake()
	if ev, err := st.next(); err != nil || !strings.Contains(ev, "/chapter/"+chapter1+"/cancel") {
		t.Fatalf("stream ended after the read timeout: %v %s", err, ev)
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
	if !strings.Contains(string(b), "<html") || !strings.Contains(string(b), "<div data-live>") {
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
	req.Header.Set("Accept", "text/event-stream")
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
