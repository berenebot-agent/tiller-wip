package legal

import (
	"strings"
	"testing"
)

// TestRegistryHasExactlyTermsAndPrivacy pins the published document set and the
// canonical order the platform editor and public endpoint rely on.
func TestRegistryHasExactlyTermsAndPrivacy(t *testing.T) {
	slugs := Slugs()
	if len(slugs) == 0 {
		t.Fatal("no registered legal documents")
	}
	seen := map[string]bool{}
	for _, slug := range slugs {
		if slug == "" {
			t.Fatal("empty slug in registry")
		}
		if seen[slug] {
			t.Fatalf("duplicate slug %q", slug)
		}
		seen[slug] = true
	}
	for _, want := range []string{"terms", "privacy"} {
		if !seen[want] {
			t.Fatalf("missing required slug %q", want)
		}
	}
	if len(slugs) != 2 {
		t.Fatalf("registered legal documents = %d, want exactly terms and privacy", len(slugs))
	}
	// Canonical publication order is terms then privacy.
	if slugs[0] != "terms" || slugs[1] != "privacy" {
		t.Fatalf("canonical order = %v, want [terms privacy]", slugs)
	}
}

// TestPlatformSlugsReturnsAnIndependentCopy guards the copy-on-return contract
// the exported slice relies on: a caller mutating the result must not corrupt the
// registry.
func TestPlatformSlugsReturnsAnIndependentCopy(t *testing.T) {
	got := PlatformSlugs()
	if len(got) != 2 {
		t.Fatalf("PlatformSlugs len = %d, want 2", len(got))
	}
	got[0] = "mutated"
	if again := PlatformSlugs(); again[0] != "terms" {
		t.Fatalf("platformEditableSlugs corrupted through PlatformSlugs result: %v", again)
	}
}

// TestSeedDocumentsAreActionablePlaceholders is the guard that keeps unreviewed
// legal prose out of the binary. Every seeded document must announce that it is
// unpublished, name the publish path, and carry bracketed fill-in tokens — but it
// must not contain drafted clause text.
func TestSeedDocumentsAreActionablePlaceholders(t *testing.T) {
	docs := SeedDocuments()
	if len(docs) != len(Slugs()) {
		t.Fatalf("SeedDocuments returned %d docs, want %d", len(docs), len(Slugs()))
	}
	for _, doc := range docs {
		if doc.Slug == "" || doc.Title == "" || doc.Body == "" {
			t.Fatalf("incomplete document: %+v", doc)
		}
		if !strings.Contains(doc.Body, "NOT PUBLISHED") {
			t.Fatalf("document %q missing the unpublished banner", doc.Slug)
		}
		if !strings.Contains(doc.Body, "not legal advice") {
			t.Fatalf("document %q missing the not-legal-advice disclaimer", doc.Slug)
		}
		// The operator has to be told how to publish, not just that it is
		// unpublished.
		if !strings.Contains(doc.Body, "PUT /api/platform/legal/"+doc.Slug) {
			t.Fatalf("document %q missing its own publish path", doc.Slug)
		}
		if !strings.Contains(doc.Body, "FILL IN BEFORE PUBLISHING") {
			t.Fatalf("document %q missing the fill-in section", doc.Slug)
		}
		if !strings.Contains(doc.Body, "[") || !strings.Contains(doc.Body, "]") {
			t.Fatalf("document %q carries no bracketed fill-in token", doc.Slug)
		}
	}
}

// TestSharedAndSpecificFillInFields proves each document advertises the shared
// operator details plus its own, and that the tokens the surviving drafts use are
// the ones the placeholder asks for.
func TestSharedAndSpecificFillInFields(t *testing.T) {
	bodies := map[string]string{}
	for _, doc := range SeedDocuments() {
		bodies[doc.Slug] = doc.Body
	}

	shared := []string{
		"[OPERATOR_LEGAL_NAME]", "[ABN]", "[REGISTERED_ADDRESS]",
		"[PRIVACY_EMAIL]", "[SECURITY_EMAIL]", "[EFFECTIVE_DATE]",
	}
	for slug, body := range bodies {
		for _, token := range shared {
			if !strings.Contains(body, token) {
				t.Fatalf("document %q missing shared field %s", slug, token)
			}
		}
	}

	// Terms additionally needs the governing-law state.
	if !strings.Contains(bodies["terms"], "[GOVERNING_LAW_STATE]") {
		t.Fatal("terms missing [GOVERNING_LAW_STATE]")
	}
	// Privacy additionally needs the subprocessor vendors.
	for _, token := range []string{
		"[CLOUD_VENDOR]", "[EDGE_VENDOR]", "[EMAIL_VENDOR]",
		"[BILLING_VENDOR]", "[MONITORING_VENDOR]",
	} {
		if !strings.Contains(bodies["privacy"], token) {
			t.Fatalf("privacy missing subprocessor field %s", token)
		}
	}
	// Document-specific fields must not leak across documents.
	if strings.Contains(bodies["terms"], "[CLOUD_VENDOR]") {
		t.Fatal("terms should not advertise privacy subprocessor fields")
	}
	if strings.Contains(bodies["privacy"], "[GOVERNING_LAW_STATE]") {
		t.Fatal("privacy should not advertise the governing-law state")
	}
}

// TestPlaceholderBodyAlignsTokens guards the presentation: a longer label added
// later must not leave the fill-in list ragged.
func TestPlaceholderBodyAlignsTokens(t *testing.T) {
	width := fieldLabelWidth()
	for _, meta := range documentFiles {
		body := placeholderBody(meta)
		for _, line := range strings.Split(body, "\n") {
			idx := strings.Index(line, "[")
			if idx < 0 {
				continue
			}
			if idx != width+3 {
				t.Fatalf("%s: token column = %d, want %d (line %q)", meta.Slug, idx, width+3, line)
			}
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
	// Retired slugs are no longer known or editable.
	for _, slug := range []string{"aup", "subprocessors", "security", "signup-notice"} {
		if KnownSlug(slug) {
			t.Fatalf("retired slug %q should not be known", slug)
		}
		if PlatformEditable(slug) {
			t.Fatalf("retired slug %q should not be platform-editable", slug)
		}
	}
	if PlatformEditable("terms-typo") {
		t.Fatal("unknown slug must not be platform-editable")
	}
}
