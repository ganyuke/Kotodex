// SPDX-License-Identifier: GPL-3.0-only
// Adapted from Kotatsu-Redo/kotatsu-parsers-redo, MangaFireParser.kt.
// Upstream revision: dc223c3d8098415b29cf1acfedc401a88d01c9fe.
// See third_party/kotatsu-parsers-redo/NOTICE.md and COPYING.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const mangaFireOrigin = "https://mangafire.to"

// Keep the upstream API canonicalization and signing together: they change
// with the site. HTTP requests retain Go's default user-agent, with no cookies.
type mangaFireSource struct {
	client *http.Client
	lim    limiter
}

func newMangaFireSource() *mangaFireSource {
	return &mangaFireSource{client: coverClient(), lim: limiter{gap: 500 * time.Millisecond}}
}

var mangaFireHID = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

func mangaFireTitle(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "mangafire.to" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("use a MangaFire HTTPS title URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] != "title" {
		return "", errors.New("use a MangaFire title URL")
	}
	hid, _, _ := strings.Cut(parts[1], "-")
	if !mangaFireHID.MatchString(hid) || len(hid) > 40 {
		return "", errors.New("invalid MangaFire title ID")
	}
	return hid, nil
}

// The fork signs decoded query values, sorts names, and expands repeated []
// keys to indexed keys in the signature while leaving the URL keys unchanged.
func mangaFireCanonical(apiPath string, q url.Values) string {
	keys := make([]string, 0, len(q))
	for key := range q {
		if key != "vrf" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var params []string
	for _, key := range keys {
		for i, value := range q[key] {
			name := key
			if strings.HasSuffix(key, "[]") {
				name = strings.TrimSuffix(key, "[]") + "[" + strconv.Itoa(i) + "]"
			}
			params = append(params, name+"="+value)
		}
	}
	canonical := strings.TrimPrefix(apiPath, "/api")
	if len(params) > 0 {
		canonical += "?" + strings.Join(params, "&")
	}
	return canonical
}

func mangaFireVRF(canonical string) string {
	data := []byte(canonical)
	for _, stage := range mangaFireVRFStages {
		table, _ := base64.StdEncoding.DecodeString(stage.table)
		key, _ := base64.StdEncoding.DecodeString(stage.key)
		previous := stage.iv
		out := make([]byte, len(data))
		for i, b := range data {
			previous = table[b^key[i%len(key)]^previous]
			out[i] = previous
		}
		data = out
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func (s *mangaFireSource) api(ctx context.Context, apiPath string, q url.Values, out any) error {
	if err := s.lim.wait(ctx); err != nil {
		return err
	}
	params := url.Values{}
	for key, values := range q {
		params[key] = slices.Clone(values)
	}
	params.Set("vrf", mangaFireVRF(mangaFireCanonical(apiPath, q)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mangaFireOrigin+apiPath+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", mangaFireOrigin+"/")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("MangaFire returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return errors.New("MangaFire response exceeds 8 MiB")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("MangaFire returned an unexpected response")
	}
	return nil
}

func (s *mangaFireSource) Chapters(ctx context.Context, ref string, _ bool) ([]*Chapter, error) {
	hid, err := mangaFireTitle(ref)
	if err != nil {
		return nil, err
	}
	var chapters []*Chapter
	seen := map[string]bool{}
	for page := 1; page <= 100; page++ {
		var response struct {
			Items []struct {
				ID        json.Number `json:"id"`
				Number    json.Number `json:"number"`
				Name      string      `json:"name"`
				Language  string      `json:"language"`
				CreatedAt int64       `json:"createdAt"`
			} `json:"items"`
			Meta struct {
				LastPage int `json:"lastPage"`
			} `json:"meta"`
		}
		err := s.api(ctx, "/api/titles/"+hid+"/chapters", url.Values{"sort": {"number"}, "order": {"desc"}, "page": {strconv.Itoa(page)}, "limit": {"200"}}, &response)
		if err != nil {
			return nil, err
		}
		for _, item := range response.Items {
			id := item.ID.String()
			if _, err := strconv.ParseUint(id, 10, 64); err != nil || seen[id] || !validChapterMetadata("", item.Number.String(), item.Language) {
				return nil, errors.New("MangaFire returned an invalid chapter")
			}
			seen[id] = true
			chapters = append(chapters, &Chapter{Source: "mangafire", SourceRef: id, SourceURL: mangaFireOrigin + "/title/" + hid + "/chapter/" + id,
				Number: normalizeChapterNumber(item.Number.String()), Title: truncate(item.Name, 500), Lang: item.Language, Published: item.CreatedAt})
		}
		if response.Meta.LastPage <= page {
			return chapters, nil
		}
	}
	return nil, errors.New("MangaFire chapter list exceeds 100 pages")
}

func (s *mangaFireSource) Pages(ctx context.Context, c *Chapter, _ bool) ([]SourcePage, error) {
	if _, err := strconv.ParseUint(c.SourceRef, 10, 64); err != nil {
		return nil, errors.New("invalid MangaFire chapter ID")
	}
	var response struct {
		Data struct {
			Pages []struct {
				URL string `json:"url"`
			} `json:"pages"`
		} `json:"data"`
	}
	if err := s.api(ctx, "/api/chapters/"+c.SourceRef, url.Values{}, &response); err != nil {
		return nil, err
	}
	if len(response.Data.Pages) == 0 || len(response.Data.Pages) > 1000 {
		return nil, errors.New("MangaFire returned an invalid page count")
	}
	var pages []SourcePage
	for _, p := range response.Data.Pages {
		if !webAddress(p.URL) {
			return nil, errors.New("MangaFire returned an invalid page URL")
		}
		u, _ := url.Parse(p.URL)
		ext := strings.ToLower(path.Ext(u.Path))
		if !imageExtension(ext) {
			ext = ".img"
		}
		pages = append(pages, SourcePage{p.URL, ext})
	}
	return pages, nil
}

func (s *mangaFireSource) Fetch(ctx context.Context, raw string, w io.Writer) (int64, error) {
	if !webAddress(raw) {
		return 0, errors.New("invalid MangaFire page URL")
	}
	if err := s.lim.wait(ctx); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Referer", mangaFireOrigin+"/")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("MangaFire image returned HTTP %d", resp.StatusCode)
	}
	return copySourceImage(resp.Body, w)
}

var mangaFireVRFStages = []struct {
	table, key string
	iv         byte
}{
	{"yINlmUNho8VYJT+ibTIP+9ESiULpVEtMOoD6U6lRE0R/xwXo/Xp9NrUgC4cw/Lmo33vUyjUE40kUoEWIr/fxfNNcq2s79ShQ5NhNrFnJ4hXPwOu/SuXzIbuTQKGFvfm08E9jvCfqAtoDqvQq3dVWPQFmJjgvkISBeXY3BgANR+yVnjGbcxZ47d6kLNfZPIayTq3/YGySb1KuVZodWp/WGNAO5pfMcpaK53Hhs0allBszaMaxuouOwdxbwgxIw6YunSsXjI05Yi0j9j4eHKfSXR8Ifo/Od+8iamRfCXTyvm7NGRGYdcQ0ywcK/u6RXhrbcCm4t2eCtrDgQVecJGkQ+A==", "0Ec58JOY3uBzJK9m3zqIOpdlF7UFiax9DmA=", 0x5a},
	{"IUFltCxD3Oc2cwCgkJffthaOg9cgPUb0LgW6H/VtfcF0kc5F25t+aWj6JH9VOhOaY0rAFdUxlDnl5BLNvwEJvQtP5qcw7vdb/K+chnbwnspSHT8mz5lqwz41TezG0hkO06FTjJZhsyNuFLDpD2ZZxQj/QIRcF90zpmQ7Byu483WsQqUE0C342HL+JXngRB6fRzxRyVTaKu83h7UYTJ0QMt6ixFh6S3F8gqkKwrGTL3jHNBsD45UnifK8+RGtishQV2K3rujLKEkiZxpr2dYcudFW4oFsDKhad3CLBvuyTqsCo4B7mL5IKQ1vXo/MOOvq1I1d8ar9X6Ttu5KF4fZgiA==", "AAdjb1iPY8CiDmq9H34tKTBF8a3oDQ==", 0x35},
	{"NQHlu1/wVO5EmkwQymF810qqY2xG1k2obcas4Z9mCsPEIFl9pRIjFxbJ7ybMHbBckT5Ton85E0FOeHezbh/mjlEYpmpnlXOS8dgrqeq2KfxImTh1YK9y0PeMNhzA1OQzSY9brYOJq/l2QnE/hwOeZIhPixVSKIUlDb5vLcH6RWKxkIEMuP0bDwIqQ71AJJaEaMJL7A6YtyIwoRT+L5v4aZzodN/0+3nOGsfblFjgxSfPzVDjNFeNl5P26+kEC/8AHgdrpAbt3hHz3HrRN1Y6e+JHgF7ncFWnoF0y3THL1S71WgWGCa6KtSzTCCG58n68nTyj2T3Sshk7utqCtMi/ZQ==", "DELOJgPsVaCcblDtTGMdHzM=", 0xba},
}

func (s *mangaFireSource) NormalizeReference(raw string) (string, error) {
	hid, err := mangaFireTitle(raw)
	if err != nil {
		return "", err
	}
	return mangaFireOrigin + "/title/" + hid, nil
}
