package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every bridge names a setup page and a reference page that exist, and the links
// open the docs of this build's ref.
func TestBridgeDocsExistAndFollowTheBuild(t *testing.T) {
	old := DocsRef
	defer func() { DocsRef = old }()
	DocsRef = "v9.9.9"
	for _, b := range newBridgeRegistry().list {
		for _, p := range []string{b.Setup, b.Runbook} {
			if _, err := os.Stat(filepath.Join("..", "..", p)); err != nil || p == "" {
				t.Errorf("%s: doc %q: %v", b.ID, p, err)
			}
		}
		if want := "https://github.com/c4lyp5o/ezllm/blob/v9.9.9/"; b.Docs != want {
			t.Errorf("%s: docs = %q, want %q", b.ID, b.Docs, want)
		}
	}
}

var docLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)
var docHeading = regexp.MustCompile(`(?m)^#+ +(.+)$`)
var docFence = regexp.MustCompile("(?ms)^ *```.*?^ *```")
var docSlugDrop = regexp.MustCompile(`[^\p{L}\p{N}\-_ ]`)

// Relative links between the repo's Markdown files resolve, heading anchors
// included, so moving or renaming a page cannot leave a dead link behind.
func TestMarkdownLinksResolve(t *testing.T) {
	root := filepath.Join("..", "..")
	var files []string
	for _, g := range []string{"*.md", "docs/*.md", "docs/*/*.md"} {
		m, _ := filepath.Glob(filepath.Join(root, g))
		files = append(files, m...)
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return docFence.ReplaceAllString(string(b), "")
	}
	anchors := func(p string) map[string]bool {
		out := map[string]bool{}
		for _, m := range docHeading.FindAllStringSubmatch(read(p), -1) {
			h := regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`).ReplaceAllString(m[1], "$1")
			out[strings.ReplaceAll(docSlugDrop.ReplaceAllString(strings.ToLower(h), ""), " ", "-")] = true
		}
		return out
	}
	for _, f := range files {
		for _, m := range docLink.FindAllStringSubmatch(read(f), -1) {
			link := m[1]
			if strings.Contains(link, "://") || strings.HasPrefix(link, "mailto:") {
				continue
			}
			path, frag, _ := strings.Cut(link, "#")
			target := f
			if path != "" {
				target = filepath.Join(filepath.Dir(f), path)
			}
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s: dead link %s", f, link)
				continue
			}
			if frag != "" && strings.HasSuffix(target, ".md") && !anchors(target)[frag] {
				t.Errorf("%s: no heading for %s", f, link)
			}
		}
	}
}
