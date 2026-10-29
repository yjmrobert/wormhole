package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestBuildSite(t *testing.T) {
	out := t.TempDir()
	root := filepath.Join("..", "..")
	if err := build(root, out, "/wormhole/", "v9.9.9", "https://github.com/yjmrobert/wormhole", time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, s := range sheets {
		p := filepath.Join(out, s.Slug, "index.html")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", s.Source, err)
		}
		html := string(b)
		if !strings.Contains(html, `<h1 class="title">`) {
			t.Errorf("%s: no title", s.Source)
		}
		if !strings.Contains(html, "v9.9.9") {
			t.Errorf("%s: release tag missing from the title block", s.Source)
		}
		if strings.Contains(html, `href="/assets/`) || strings.Contains(html, `src="/assets/`) {
			t.Errorf("%s: asset link ignores the base path", s.Source)
		}
		if regexp.MustCompile(`href="[^":]*\.md(#[^"]*)?"`).MatchString(html) {
			t.Errorf("%s: a Markdown link was not rewritten", s.Source)
		}
		// Internal links must point at sheets that exist.
		for _, m := range regexp.MustCompile(`href="/wormhole/([a-z0-9-]*)/?(#[^"]*)?"`).FindAllStringSubmatch(html, -1) {
			slug := m[1]
			if slug == "assets" || slug == "search.json" {
				continue
			}
			if _, err := os.Stat(filepath.Join(out, slug, "index.html")); err != nil {
				t.Errorf("%s: dangling link to %q", s.Source, m[0])
			}
		}
	}
	for _, f := range []string{"404.html", ".nojekyll", "search.json", "assets/style.css", "assets/site.js", "assets/fonts.css", "assets/favicon.svg", "assets/fonts/barlow-400-latin.woff2", "assets/fonts/OFL-Barlow.txt"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	var idx []searchEntry
	b, _ := os.ReadFile(filepath.Join(out, "search.json"))
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx) != len(sheets) {
		t.Fatalf("search index has %d entries, want %d", len(idx), len(sheets))
	}
	if idx[0].Title != "Wormhole" || len(idx[0].Headings) == 0 || idx[0].Headings[0].ID == "" {
		t.Fatalf("search index entry: %+v", idx[0])
	}
	home, _ := os.ReadFile(filepath.Join(out, "index.html"))
	if !strings.Contains(string(home), `class="drawing drawing-landscape"`) || !strings.Contains(string(home), `class="lede"`) {
		t.Error("home sheet lacks the schematic or the lede")
	}
	skill, _ := os.ReadFile(filepath.Join(out, "skill", "index.html"))
	if strings.Contains(string(skill), "user-invocable") {
		t.Error("skill frontmatter leaked into the page")
	}
}

func TestStripFrontmatter(t *testing.T) {
	got := string(stripFrontmatter([]byte("---\nname: x\n---\n# Title\n")))
	if got != "# Title\n" {
		t.Fatalf("%q", got)
	}
	if string(stripFrontmatter([]byte("# No front\n"))) != "# No front\n" {
		t.Fatal("stripped something that was not frontmatter")
	}
}
