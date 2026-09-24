package legal

import (
	"strings"
	"testing"
)

func TestDocumentsAreDraftsWithBannerAndBodies(t *testing.T) {
	docs := Documents()
	if len(docs) == 0 {
		t.Fatal("no embedded legal documents")
	}
	seen := map[string]bool{}
	for _, doc := range docs {
		if doc.Slug == "" || doc.Title == "" || doc.Body == "" {
			t.Fatalf("incomplete document: %+v", doc)
		}
		if seen[doc.Slug] {
			t.Fatalf("duplicate slug %q", doc.Slug)
		}
		seen[doc.Slug] = true
		if !strings.Contains(doc.Body, "DRAFT FOR LEGAL REVIEW") {
			t.Fatalf("document %q missing draft banner", doc.Slug)
		}
	}
	for _, want := range []string{"terms", "privacy", "aup", "subprocessors", "security", "signup-notice"} {
		if !seen[want] {
			t.Fatalf("missing required slug %q", want)
		}
	}
}

func TestKnownSlugAndPlatformEditable(t *testing.T) {
	if !KnownSlug("terms") {
		t.Fatal("terms should be a known slug")
	}
	if KnownSlug("nope") {
		t.Fatal("nope should not be a known slug")
	}
	for _, slug := range PlatformSlugs() {
		if !PlatformEditable(slug) {
			t.Fatalf("%q should be platform-editable", slug)
		}
	}
	if PlatformEditable("signup-notice") {
		t.Fatal("signup-notice is a collection notice, not platform-editable")
	}
	if PlatformEditable("terms-typo") {
		t.Fatal("unknown slug must not be platform-editable")
	}
}
