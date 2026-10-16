package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Live updates. A page's live sections are templates named "live-*", shown in
// an element with the template's name as its id. The page opens an htmx SSE
// connection on its own URL (hx-sse:connect in layout.html). The same handler then runs again whenever
// something changes, only the live templates are rendered, and the sections
// that look different from last time are sent as one message of htmx
// out-of-band swaps. Nothing is sent when no section changed.

// changes wakes everyone waiting when something on the live pages changes.
// The zero value is ready to use.
type changes struct {
	mu sync.Mutex
	ch chan struct{}
}

// wait returns a channel that is closed on the next notify.
func (c *changes) wait() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch == nil {
		c.ch = make(chan struct{})
	}
	return c.ch
}

func (c *changes) notify() {
	c.mu.Lock()
	if c.ch != nil {
		close(c.ch)
		c.ch = nil
	}
	c.mu.Unlock()
}

const (
	// Changes within this window go out as one update.
	liveCoalesce = 250 * time.Millisecond
	// Page progress changes many times a second. It goes out at most this
	// often, since every update re-runs the page's queries.
	liveProgressEvery = time.Second
	// Re-render this often even without changes, so relative times ("in 5
	// minutes") stay right and proxies don't close an idle stream.
	liveRefresh = 30 * time.Second
)

type liveKey struct{}

// liveSections holds one render of a page's live sections, by template name.
type liveSections map[string][]byte

// liveCollector returns where render should put the live sections instead of
// writing the page, or nil for a normal render.
func liveCollector(r *http.Request) liveSections {
	sections, _ := r.Context().Value(liveKey{}).(liveSections)
	return sections
}

// liveNames lists the live sections a page template defines.
func liveNames(t *template.Template) []string {
	var names []string
	for _, tt := range t.Templates() {
		if strings.HasPrefix(tt.Name(), "live-") {
			names = append(names, tt.Name())
		}
	}
	slices.Sort(names)
	return names
}

// liveHeader marks the request for a live update stream, sent by the
// hx-sse:connect element in layout.html. The Accept header can't tell it
// apart, since htmx's SSE extension adds text/event-stream to every request,
// links and forms included.
const liveHeader = "Kotodex-Live"

// wantsStream reports whether the page asks for a live update stream.
func wantsStream(r *http.Request) bool {
	return r.Header.Get(liveHeader) != ""
}

// startStream sends the headers of an event stream. EventSource treats any
// other status or content type as a network error and retries, so even a
// stream that ends right away starts this way.
func startStream(w http.ResponseWriter) (*http.ResponseController, error) {
	rc := http.NewResponseController(w)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no") // nginx buffers responses otherwise
	w.WriteHeader(http.StatusOK)
	return rc, rc.Flush()
}

// endStream tells the page to stop listening and reload, so it shows what
// replaced it: the sign-in page or a "not found".
func endStream(w http.ResponseWriter, rc *http.ResponseController) {
	fmt.Fprint(w, "event: end\ndata:\n\n")
	rc.Flush()
}

// live serves h as usual, or as an event stream of its live section when the
// browser asks for one. showsProgress reports whether the page shows the
// chapter being downloaded. Only those pages wake on page progress. nil means
// never.
func (s *Server) live(h http.HandlerFunc, showsProgress func(r *http.Request) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !wantsStream(r) {
			h(w, r)
			return
		}
		rc, err := startStream(w)
		if err != nil {
			return
		}

		ctx := r.Context()
		token := sessionToken(r)
		u := currentUser(r) // web() just checked the session
		checkUser := false
		last := liveSections{}
		for {
			changed := s.dl.changed.wait()
			signedOut := s.db.signedOut.wait()
			var progressed <-chan struct{}
			if showsProgress != nil && showsProgress(r) {
				progressed = s.dl.progressed.wait()
			}
			// Signing out or deleting the user ends the stream. A fresh
			// copy of the user is used for the render.
			if checkUser {
				if u, err = s.db.sessionUser(token); err != nil {
					endStream(w, rc)
					return
				}
			}
			sections := liveSections{}
			fr := r.WithContext(context.WithValue(context.WithValue(ctx, liveKey{}, sections), userKey, u))
			var st statusWriter
			h(&st, fr)
			// The page is gone (a deleted manga). The reload shows that.
			if st.status != 0 && st.status != http.StatusOK {
				endStream(w, rc)
				return
			}
			var msg bytes.Buffer
			for _, name := range slices.Sorted(maps.Keys(sections)) {
				if bytes.Equal(sections[name], last[name]) {
					continue
				}
				last[name] = sections[name]
				fmt.Fprintf(&msg, `<div id="%s" hx-swap-oob="innerMorph">`, name)
				msg.Write(sections[name])
				msg.WriteString("</div>\n")
			}
			if msg.Len() > 0 {
				writeEvent(w, msg.Bytes())
			} else {
				fmt.Fprint(w, ": ping\n\n")
			}
			if rc.Flush() != nil {
				return
			}

			wait := liveCoalesce
			checkUser = true
			select {
			case <-ctx.Done():
				return
			case <-s.closing:
				return
			case <-time.After(liveRefresh):
				continue
			case <-changed:
			case <-signedOut:
			case <-progressed:
				// Progress alone doesn't touch sessions.
				wait = liveProgressEvery
				checkUser = false
			}
			select {
			case <-ctx.Done():
				return
			case <-s.closing:
				return
			case <-time.After(wait):
			}
			select {
			case <-signedOut:
				checkUser = true
			default:
			}
		}
	}
}

// writeEvent sends html as one message. Each line needs its own "data:".
func writeEvent(w http.ResponseWriter, html []byte) {
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimSpace(string(html)), "\n") {
		b.WriteString("data: ")
		b.WriteString(strings.TrimSuffix(line, "\r"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	w.Write([]byte(b.String()))
}

// statusWriter records the status of a live render. The sections go into
// the liveSections in the request instead. Anything else written, like an
// error page, is dropped.
type statusWriter struct {
	header http.Header
	status int
}

func (f *statusWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}

func (f *statusWriter) WriteHeader(code int) {
	if f.status == 0 {
		f.status = code
	}
}

func (f *statusWriter) Write(p []byte) (int, error) {
	if f.status == 0 {
		f.status = http.StatusOK
	}
	return len(p), nil
}
