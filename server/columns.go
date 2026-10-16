package main

import (
	"net/http"
	"slices"
	"strings"
)

// Chapter table columns each person can hide and reorder. The choice lives
// in a cookie, so it stays in their browser and the server draws the table
// that way from the start. Chapter stays first and the buttons stay last.

type chapterCol struct {
	Name, Label string
	Hidden      bool
}

var columnChoices = []chapterCol{
	{Name: "title", Label: "Title"}, {Name: "lang", Label: "Language"}, {Name: "group", Label: "Group"},
	{Name: "date", Label: "Published"}, {Name: "pages", Label: "Pages"}, {Name: "you", Label: "You"},
	{Name: "status", Label: "Status"},
}

// colsCookie holds names in order, hidden ones marked with "-", joined with
// dots: "lang.title.-group.date.pages.you.status".
const colsCookie = "chapter_columns"

// chapterColumns returns the columns in the person's order, and the ones to
// show. Unknown names are skipped and columns missing from the cookie (new
// ones) are added at the end, shown.
func chapterColumns(r *http.Request) (all, shown []chapterCol) {
	label := map[string]string{}
	for _, c := range columnChoices {
		label[c.Name] = c.Label
	}
	seen := map[string]bool{}
	if ck, err := r.Cookie(colsCookie); err == nil {
		for _, part := range strings.Split(ck.Value, ".") {
			name, hidden := strings.CutPrefix(part, "-")
			if l, ok := label[name]; ok && !seen[name] {
				seen[name] = true
				all = append(all, chapterCol{Name: name, Label: l, Hidden: hidden})
			}
		}
	}
	for _, c := range columnChoices {
		if !seen[c.Name] {
			all = append(all, c)
		}
	}
	shown = slices.DeleteFunc(slices.Clone(all), func(c chapterCol) bool { return c.Hidden })
	return all, shown
}
