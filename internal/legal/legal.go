// Package legal holds the first-draft hosted legal documents as embedded
// plain-text files.
//
// These drafts are served as plain text: there is no markdown renderer at serve
// time (the body is returned verbatim in the JSON "body" field and rendered by
// the browser as preformatted text). Headings and lists are written as plain
// lines for that reason.
//
// The text is a COMPLIANT ENGINEERING FIRST DRAFT, not legal advice. Every
// document carries a "DRAFT FOR LEGAL REVIEW" banner and explicit
// [PLACEHOLDER] tokens that must be resolved, and the whole pack must be
// reviewed by a qualified lawyer before unrestricted public signup.
package legal

import (
	"embed"
	"sort"
)

//go:embed *.md
var files embed.FS

// Document is one publishable legal document. Slug is the stable identifier
// stored in legal_documents; Title is the human title; Body is the plain-text
// document content.
type Document struct {
	Slug  string
	Title string
	Body  string
}

// documentFiles maps the stable public slug to its embedded filename and human
// title. The order of this table is the canonical publication order.
var documentFiles = []struct {
	Slug  string
	Title string
	File  string
}{
	{Slug: "terms", Title: "Terms of Service", File: "terms.md"},
	{Slug: "privacy", Title: "Privacy Policy", File: "privacy.md"},
	{Slug: "aup", Title: "Acceptable Use Policy", File: "aup.md"},
	{Slug: "subprocessors", Title: "Subprocessor List", File: "subprocessors.md"},
	{Slug: "security", Title: "Security and Data Handling", File: "security.md"},
	{Slug: "signup-notice", Title: "Signup Privacy Notice", File: "signup-notice.md"},
}

// Documents returns every embedded draft in canonical publication order. It
// panics only if an embedded file declared in documentFiles is missing, which
// would be a build-time mistake (the embed directive guarantees the files are
// present).
func Documents() []Document {
	out := make([]Document, 0, len(documentFiles))
	for _, meta := range documentFiles {
		body, err := files.ReadFile(meta.File)
		if err != nil {
			panic("legal: embedded document " + meta.File + " missing: " + err.Error())
		}
		out = append(out, Document{Slug: meta.Slug, Title: meta.Title, Body: string(body)})
	}
	return out
}

// Slugs returns the known document slugs in canonical order.
func Slugs() []string {
	out := make([]string, 0, len(documentFiles))
	for _, meta := range documentFiles {
		out = append(out, meta.Slug)
	}
	return out
}

// KnownSlug reports whether slug names an embedded document. The platform
// editor uses this to reject publishing to an unknown slug.
func KnownSlug(slug string) bool {
	for _, meta := range documentFiles {
		if meta.Slug == slug {
			return true
		}
	}
	return false
}

// DocumentBySlug returns the embedded draft for a slug, if present.
func DocumentBySlug(slug string) (Document, bool) {
	for _, meta := range documentFiles {
		if meta.Slug == slug {
			body, err := files.ReadFile(meta.File)
			if err != nil {
				return Document{}, false
			}
			return Document{Slug: meta.Slug, Title: meta.Title, Body: string(body)}, true
		}
	}
	return Document{}, false
}

// platformEditableSlugs are the slugs the platform editor may publish. The
// signup-notice draft is a collection notice resource, not a document published
// through the platform editor, so it is excluded.
var platformEditableSlugs = []string{"terms", "privacy", "aup", "subprocessors", "security"}

// PlatformSlugs returns the slugs the platform editor may publish, in canonical
// order.
func PlatformSlugs() []string {
	out := make([]string, len(platformEditableSlugs))
	copy(out, platformEditableSlugs)
	return out
}

// PlatformEditable reports whether the platform editor may publish to slug. It
// requires the slug to be a known embedded document and not the signup notice.
func PlatformEditable(slug string) bool {
	if !KnownSlug(slug) {
		return false
	}
	for _, s := range platformEditableSlugs {
		if s == slug {
			return true
		}
	}
	return false
}

// SortedSlugs returns the known slugs alphabetically (used by tests/diagnostics
// where the canonical order is not required).
func SortedSlugs() []string {
	out := Slugs()
	sort.Strings(out)
	return out
}
