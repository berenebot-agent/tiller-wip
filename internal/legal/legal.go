// Package legal holds the hosted legal document registry and generates the
// placeholder body that every registered slug is seeded with.
//
// The registry exists to constrain what a slug may be: the public read endpoint
// serves whatever the database holds, while the platform editor accepts a
// publish only for a slug registered here.
//
// The documents are NOT in the source tree. Unreviewed draft legal text does
// not belong in the tree or in any image built from it, so the approved drafts
// are maintained outside the repository and published by the operator through
// the platform dashboard. What the code ships is an instructional placeholder:
// it names the publish path and lists the bracketed fields the operator has to
// fill in, so a fresh deploy never serves a document that looks final but is
// not.
//
// Seeding writes only into rows still carrying the migration placeholder
// (updated_by IS NULL). Once an operator publishes, that row is never written
// again, so a re-deploy cannot clobber approved text.
//
// Legal text is rendered as plain text at serve time. There is no markdown
// renderer (the body is returned verbatim in the JSON "body" field and rendered
// by the browser as preformatted text).
package legal

// Document is one publishable legal document. Slug is the stable identifier
// stored in legal_documents, Title is the human title, and Body is the
// plain-text content.
type Document struct {
	Slug  string
	Title string
	Body  string
}

// placeholderField is one operator-supplied detail a published document has to
// carry, shown in the placeholder as a bracketed fill-in token.
type placeholderField struct {
	Label string
	Token string
}

// sharedPlaceholderFields are the details every published hosted document has
// to carry, regardless of which document it is.
var sharedPlaceholderFields = []placeholderField{
	{Label: "Operator legal name", Token: "[OPERATOR_LEGAL_NAME]"},
	{Label: "Registration number (ABN)", Token: "[ABN]"},
	{Label: "Registered address", Token: "[REGISTERED_ADDRESS]"},
	{Label: "Privacy contact", Token: "[PRIVACY_EMAIL]"},
	{Label: "Security contact", Token: "[SECURITY_EMAIL]"},
	{Label: "Effective date", Token: "[EFFECTIVE_DATE]"},
}

// documentMeta is one registered document: its stable public slug, human title,
// what publishing it is for, and the fill-in fields specific to it.
type documentMeta struct {
	Slug    string
	Title   string
	Purpose string
	Fields  []placeholderField
}

// documentFiles is the registry, in canonical publication order.
//
// Hosted Tiller publishes exactly two legal documents: the Terms of Service
// (which carry the acceptable-use rules) and the Privacy Policy (which carries
// the subprocessor and security/data-handling disclosures). Keeping the public
// set to the two documents users actually accept avoids presenting supporting
// disclosures as if they were separate agreements.
var documentFiles = []documentMeta{
	{
		Slug:    "terms",
		Title:   "Terms of Service",
		Purpose: "the Terms of Service, including the acceptable-use rules folded into them",
		Fields: []placeholderField{
			{Label: "Governing law state", Token: "[GOVERNING_LAW_STATE]"},
		},
	},
	{
		Slug:    "privacy",
		Title:   "Privacy Policy",
		Purpose: "the Privacy Policy, including the subprocessor and data-handling disclosures",
		Fields: []placeholderField{
			{Label: "Hosting / infrastructure", Token: "[CLOUD_VENDOR]"},
			{Label: "Edge / CDN", Token: "[EDGE_VENDOR]"},
			{Label: "Email delivery", Token: "[EMAIL_VENDOR]"},
			{Label: "Payment processing", Token: "[BILLING_VENDOR]"},
			{Label: "External monitoring", Token: "[MONITORING_VENDOR]"},
		},
	},
}

// allFields returns the shared fields followed by the document's own.
func (m documentMeta) allFields() []placeholderField {
	fields := make([]placeholderField, 0, len(sharedPlaceholderFields)+len(m.Fields))
	fields = append(fields, sharedPlaceholderFields...)
	fields = append(fields, m.Fields...)
	return fields
}

// fieldLabelWidth is the column width the placeholder aligns fill-in tokens to.
// It is derived from the labels rather than hardcoded so adding a field cannot
// leave the placeholder ragged.
func fieldLabelWidth() int {
	width := 0
	for _, meta := range documentFiles {
		for _, field := range meta.allFields() {
			if len(field.Label) > width {
				width = len(field.Label)
			}
		}
	}
	return width
}

// placeholderBody renders the instructional placeholder for one document: an
// explicit unpublished banner, the publish path, and the bracketed fields the
// operator has to fill in. It deliberately contains no drafted legal prose, so
// nothing here can be mistaken for approved text.
func placeholderBody(meta documentMeta) string {
	width := fieldLabelWidth()

	var b []byte
	b = append(b, "*** NOT PUBLISHED - PLACEHOLDER ***\n\n"...)
	b = append(b, "This is not a legal document and not legal advice. No approved\n"...)
	b = append(b, "text has been published for this slug yet.\n\n"...)
	b = append(b, "Before accepting signups, publish:\n"...)
	b = append(b, "  "+meta.Purpose+"\n\n"...)
	b = append(b, "Publish with either:\n"...)
	b = append(b, "  Platform dashboard -> Legal\n"...)
	b = append(b, "  PUT /api/platform/legal/"+meta.Slug+"\n\n"...)
	b = append(b, "The approved draft is maintained outside this repository. Replace\n"...)
	b = append(b, "every bracketed field below with the operator's real details, then\n"...)
	b = append(b, "publish. This placeholder is re-seeded on every deploy until then.\n\n"...)
	b = append(b, "FILL IN BEFORE PUBLISHING\n\n"...)
	for _, field := range meta.allFields() {
		b = append(b, field.Label+":"...)
		for i := len(field.Label) + 1; i < width+3; i++ {
			b = append(b, ' ')
		}
		b = append(b, field.Token+"\n"...)
	}
	return string(b)
}

// SeedDocuments returns the placeholder document for every registered slug, in
// canonical publication order. It is what a fresh or not-yet-published hosted
// database is seeded with; a published document is never touched.
func SeedDocuments() []Document {
	out := make([]Document, 0, len(documentFiles))
	for _, meta := range documentFiles {
		out = append(out, Document{Slug: meta.Slug, Title: meta.Title, Body: placeholderBody(meta)})
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

// KnownSlug reports whether slug names a registered document.
func KnownSlug(slug string) bool {
	for _, meta := range documentFiles {
		if meta.Slug == slug {
			return true
		}
	}
	return false
}

// platformEditableSlugs are the slugs the platform editor may publish. It is
// the same set as the registered documents.
var platformEditableSlugs = []string{"terms", "privacy"}

// PlatformSlugs returns the slugs the platform editor may publish, in canonical
// order.
func PlatformSlugs() []string {
	out := make([]string, len(platformEditableSlugs))
	copy(out, platformEditableSlugs)
	return out
}

// PlatformEditable reports whether the platform editor may publish to slug. It
// requires the slug to be a registered document.
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
