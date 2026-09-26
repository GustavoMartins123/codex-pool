package main

import (
	"strings"
	"testing"
)

// htmlTemplateLiterals returns every backtick template literal in the page
// that contains HTML markup. Literals without tags (plain strings assigned
// to properties such as element.title) never enter the HTML parser and are
// excluded.
func htmlTemplateLiterals(page string) []string {
	var out []string
	rest := page
	for {
		open := strings.IndexByte(rest, '`')
		if open < 0 {
			break
		}
		tail := rest[open+1:]
		close := strings.IndexByte(tail, '`')
		if close < 0 {
			break
		}
		literal := tail[:close]
		if strings.Contains(literal, "</") {
			out = append(out, literal)
		}
		rest = tail[close+1:]
	}
	return out
}

// The friend landing page renders pool API data through innerHTML template
// literals. Every dynamic interpolation inside an HTML-bearing template
// literal must go through the escapeHTML helper defined in the page itself.
// This test fails if a raw (unescaped) dynamic interpolation is reintroduced.
func TestFriendLandingEscapesDynamicHTMLSinks(t *testing.T) {
	raw, err := friendContent.ReadFile("templates/friend_landing.html")
	if err != nil {
		t.Fatalf("read friend landing template: %v", err)
	}
	page := string(raw)

	if !strings.Contains(page, "function escapeHTML(") {
		t.Fatal("friend landing lost its escapeHTML helper")
	}
	if !strings.Contains(page, "function escapeAttr(") {
		t.Fatal("friend landing lost its escapeAttr helper")
	}

	forbidden := []string{
		"${acc.id}",
		"${acc.status}",
		"${acc.plan_type}",
		"${user.user_id}",
		"${origin.origin_id}",
		"${err.message}",
		"${data.account_id}",
		"${surface}",
		"${type}",
	}
	checked := 0
	for _, literal := range htmlTemplateLiterals(page) {
		checked++
		for _, pattern := range forbidden {
			if strings.Contains(literal, pattern) {
				t.Errorf("friend landing interpolates %q into an HTML template literal without escaping: %.80s", pattern, literal)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no HTML template literals found; the sink scanner is broken")
	}
}
