package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Live updates. A page with a [data-live] section opens an EventSource on its
// own URL. The same handler then runs again whenever something changes, and
// only the "live" template is rendered and sent as an event. Nothing is sent
// when the section looks the same as last time.

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
	// Changes within this window go out as one update. Page progress can
	// change many times a second.
	liveCoalesce = 250 * time.Millisecond
	// Re-render this often even without changes, so relative times ("in 5
	// minutes") stay right and proxies don't close an idle stream.
	liveRefresh = 30 * time.Second
)

type liveKey struct{}

// liveFragment reports whether render should only write the "live" template.
func liveFragment(r *http.Request) bool {
	return r.Context().Value(liveKey{}) != nil
}

// live serves h as usual, or as an event stream of its live section when the
// browser asks for one.
func (s *Server) live(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			h(w, r)
			return
		}
		rc := http.NewResponseController(w)
		hd := w.Header()
		hd.Set("Content-Type", "text/event-stream")
		hd.Set("Cache-Control", "no-store")
		hd.Set("X-Accel-Buffering", "no") // nginx buffers responses otherwise
		w.WriteHeader(http.StatusOK)
		if rc.Flush() != nil {
			return
		}

		ctx := r.Context()
		fr := r.WithContext(context.WithValue(ctx, liveKey{}, true))
		token := sessionToken(r)
		var last []byte
		for {
			changed := s.dl.changed.wait()
			signedOut := s.db.signedOut.wait()
			// Signing out or deleting the user ends the stream. The event
			// tells the page not to reconnect.
			if _, err := s.db.sessionUser(token); err != nil {
				fmt.Fprint(w, "event: signedout\ndata:\n\n")
				rc.Flush()
				return
			}
			var frag fragmentWriter
			h(&frag, fr)
			if frag.status != 0 && frag.status != http.StatusOK {
				return
			}
			out := frag.buf.Bytes()
			if !bytes.Equal(out, last) {
				last = bytes.Clone(out)
				writeEvent(w, out)
			} else {
				fmt.Fprint(w, ": ping\n\n")
			}
			if rc.Flush() != nil {
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-s.closing:
				return
			case <-time.After(liveRefresh):
				continue
			case <-changed:
			case <-signedOut:
			}
			select {
			case <-ctx.Done():
				return
			case <-s.closing:
				return
			case <-time.After(liveCoalesce):
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

// fragmentWriter collects one render of a live section.
type fragmentWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (f *fragmentWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}

func (f *fragmentWriter) WriteHeader(code int) {
	if f.status == 0 {
		f.status = code
	}
}

func (f *fragmentWriter) Write(p []byte) (int, error) {
	if f.status == 0 {
		f.status = http.StatusOK
	}
	return f.buf.Write(p)
}
