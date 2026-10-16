package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Covers of manga from other sources
//
// Synced manga from other sources carry the cover address the app uses. When
// an admin turns it on, Kotodex downloads those covers and serves them like
// its own, so browsers only ever talk to Kotodex. The download goes to
// whatever address someone's app synced, so it only reaches public internet
// addresses, sends nothing that identifies the server or its users, and keeps
// only real images of a sane size.

const (
	coverAgent    = "Kotodex-covers"
	coverMaxBytes = 5 << 20
	coverTimeout  = 20 * time.Second
	coverRetry    = 24 * time.Hour // after a failed download
	coverWorkers  = 2
	coverMaxFails = 10000 // failed addresses remembered at once

	maxCoverLimitMB = 1 << 30 // 1 PB, keeps the limit in bytes from overflowing
)

// errCoversFull means the storage limit is reached. It isn't the address's
// fault, so it isn't remembered as a failure, and raising the limit lets the
// cover download right away.
var errCoversFull = errors.New("cover storage is full")

var coverTypes = map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp", "image/gif": "gif"}

// publicAddr reports whether ip is on the public internet. Anything else
// (this machine, home and office networks, carrier NAT, link-local, and so
// on) could reach services that aren't meant to be reachable from outside.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64, can wrap private IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"), // 6to4, same
}

// coverClient fetches covers. The check runs on the address actually
// connected to, after DNS and for every redirect, so a name that resolves to
// a home address or a redirect to one is refused too. Proxy settings from the
// environment are ignored for the same reason.
func coverClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !publicAddr(ap.Addr()) {
				return fmt.Errorf("not a public address: %s", address)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: coverTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConns:          4,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("not a web address")
			}
			req.Header.Del("Referer")
			return nil
		},
	}
}

// Covers downloads and keeps covers of manga from other sources.
type Covers struct {
	dir    string
	client *http.Client
	queue  chan string
	mu     sync.Mutex
	busy   map[string]bool
	failed map[string]time.Time
	size   int64 // bytes stored, counted on first use
	sized  sync.Once
	limit  func() int64 // bytes all stored covers may take, 0 for no limit
}

func newCovers(db *DB, mangaDir string) *Covers {
	return &Covers{
		dir: filepath.Join(mangaDir, "other"), client: coverClient(), queue: make(chan string, 256),
		busy: map[string]bool{}, failed: map[string]time.Time{},
		limit: func() int64 { return int64(min(db.settings().CoverLimitMB, maxCoverLimitMB)) << 20 },
	}
}

func coverKey(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:])
}

// cached returns the stored file's path under the manga directory, or "".
func (c *Covers) cached(u string) string {
	key := coverKey(u)
	for _, ext := range coverTypes {
		name := key + "." + ext
		if _, err := os.Stat(filepath.Join(c.dir, name)); err == nil {
			return "other/" + name
		}
	}
	return ""
}

// want queues a cover to download unless it's stored, on its way, or failed
// lately. Pages don't wait for it. The cover shows the next time.
func (c *Covers) want(u string) {
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return
	}
	if c.full() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy[u] || time.Since(c.failed[u]) < coverRetry {
		return
	}
	if len(c.failed) >= coverMaxFails {
		c.forgetFailures()
	}
	select {
	case c.queue <- u:
		c.busy[u] = true
	default: // plenty queued already, it'll be asked for again
	}
}

// forgetFailures drops failures past the retry wait, then the oldest tenth
// if that wasn't enough, so the rest keep their wait. c.mu is held.
func (c *Covers) forgetFailures() {
	for k, t := range c.failed {
		if time.Since(t) >= coverRetry {
			delete(c.failed, k)
		}
	}
	if len(c.failed) < coverMaxFails {
		return
	}
	keys := slices.SortedFunc(maps.Keys(c.failed), func(a, b string) int { return c.failed[a].Compare(c.failed[b]) })
	for _, k := range keys[:len(keys)/10+1] {
		delete(c.failed, k)
	}
}

func (c *Covers) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range coverWorkers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case u := <-c.queue:
					err := c.fetch(ctx, u)
					c.mu.Lock()
					delete(c.busy, u)
					if err != nil && !errors.Is(err, errCoversFull) {
						c.failed[u] = time.Now()
					}
					c.mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
}

// measure counts what's stored, once.
func (c *Covers) measure() {
	c.sized.Do(func() {
		entries, _ := os.ReadDir(c.dir)
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				c.size += info.Size()
			}
		}
	})
}

// full reports whether the admin's storage limit is reached. Covers already
// stored are never deleted, so a full limit only stops new downloads.
func (c *Covers) full() bool {
	c.measure()
	limit := c.limit()
	c.mu.Lock()
	defer c.mu.Unlock()
	return limit > 0 && c.size >= limit
}

// reserve counts n more bytes against the storage limit, or reports that
// they don't fit. A cover that isn't saved after all gives them back with
// release.
func (c *Covers) reserve(n int64) bool {
	c.measure()
	limit := c.limit()
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit > 0 && c.size+n > limit {
		return false
	}
	c.size += n
	return true
}

func (c *Covers) release(n int64) {
	c.mu.Lock()
	c.size -= n
	c.mu.Unlock()
}

func (c *Covers) fetch(ctx context.Context, u string) (err error) {
	// Queued before the limit was reached, so checked again here.
	if c.full() {
		return errCoversFull
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", coverAgent)
	req.Header.Set("Accept", "image/*")
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("cover: %s", res.Status)
	}
	if res.ContentLength > coverMaxBytes {
		return errors.New("cover too large")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, coverMaxBytes+1))
	if err != nil {
		return err
	}
	if len(data) > coverMaxBytes {
		return errors.New("cover too large")
	}
	ext, ok := coverTypes[http.DetectContentType(data)]
	if !ok {
		return errors.New("not an image")
	}
	n := int64(len(data))
	if !c.reserve(n) {
		return errCoversFull
	}
	defer func() {
		if err != nil {
			c.release(n)
		}
	}()
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, ".cover-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(c.dir, coverKey(u)+"."+ext))
}
