package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// All page styling lives in static/gateway42.css. These tests keep it there:
// a <style> block or style="…" attribute that creeps back into a template
// works, so nothing else would notice — and the next change to the shared
// stylesheet would then silently miss that page.

// inlineStyle matches a style attribute or a <style> element. JavaScript that
// sets a data-driven value (the pull progress bar's width) uses el.style.x,
// which this deliberately does not match.
var inlineStyle = regexp.MustCompile(`(?i)<style[\s>]|\sstyle\s*=`)

func TestTemplatesHaveNoInlineStyles(t *testing.T) {
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := templateFS.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if inlineStyle.MatchString(line) {
				t.Errorf("%s:%d: inline style; move it to static/gateway42.css: %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLayoutsLinkStylesheet(t *testing.T) {
	// base.html covers every admin page; login.html is standalone.
	for _, path := range []string{"templates/base.html", "templates/login.html"} {
		b, err := templateFS.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `<link rel="stylesheet" href="/static/gateway42.css">`) {
			t.Errorf("%s does not link /static/gateway42.css", path)
		}
	}
}

func TestStylesheetIsServed(t *testing.T) {
	rr := httptest.NewRecorder()
	setupRouter().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/gateway42.css", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
	if !strings.Contains(rr.Body.String(), ".login-page") {
		t.Error("served stylesheet is missing the login-page rules")
	}
}
