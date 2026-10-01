package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/tiller-router/tiller-router/internal/legal"
	"github.com/tiller-router/tiller-router/internal/store"
)

// WS2 — legal handlers. Own this file.
// Implements the endpoint contract in docs/stage_d_api_contract.md.

// SeedLegalDocuments writes the generated placeholder for every registered slug
// into the database. It is idempotent and safe to run on every boot:
// store.SeedLegalDoc only writes a row whose updated_by IS NULL, so a document
// an operator has published is never clobbered by a deploy.
//
// The placeholder carries no drafted legal prose — it names the publish path and
// lists the bracketed fields the operator must fill in — so a fresh deploy never
// serves something that looks like approved text. The composition root calls
// this once at startup. A seed failure is logged, not fatal: the migration
// placeholder row is still served rather than a 404.
func (s *Server) SeedLegalDocuments(ctx context.Context) error {
	for _, doc := range legal.SeedDocuments() {
		if err := s.storeHandle().SeedLegalDoc(ctx, store.LegalDoc{Slug: doc.Slug, Title: doc.Title, Body: doc.Body}); err != nil {
			return err
		}
	}
	return nil
}

// legalDoc serves a current published legal document publicly (signup links it
// before authentication). Hosted mode registers it; the document is a
// platform-global row, so no account scope applies.
func (s *Server) legalDoc(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	doc, err := s.storeHandle().GetLegalDoc(r.Context(), slug)
	if errors.Is(err, store.ErrLegalDocNotFound) {
		adminError(w, http.StatusNotFound, "legal_not_found", "Legal document not found.")
		return
	}
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load the legal document.")
		return
	}
	// Return only the published fields; updated_by is operator provenance and
	// must not leak to the public endpoint.
	writeJSON(w, http.StatusOK, map[string]any{
		"slug":       doc.Slug,
		"title":      doc.Title,
		"body":       doc.Body,
		"updated_at": doc.UpdatedAt,
	})
}

// writePlatformLegalDocs lists documents for the platform editor.
func (s *Server) writePlatformLegalDocs(w http.ResponseWriter, r *http.Request) {
	docs, err := s.storeHandle().ListLegalDocs(r.Context())
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not list legal documents.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": docs})
}

// writePlatformLegalUpdate publishes a document.
func (s *Server) writePlatformLegalUpdate(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if !legal.PlatformEditable(slug) {
		adminError(w, http.StatusNotFound, "legal_not_found", "Legal document not found.")
		return
	}
	var input struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	title := strings.TrimSpace(input.Title)
	if title == "" || strings.TrimSpace(input.Body) == "" {
		adminError(w, http.StatusBadRequest, "invalid_request", "Both title and body are required.")
		return
	}
	doc := store.LegalDoc{Slug: slug, Title: title, Body: input.Body}
	if err := s.storeHandle().UpsertLegalDoc(r.Context(), doc, "platform"); err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not publish the legal document.")
		return
	}
	s.recordPlatformAudit(r.Context(), store.AuditEvent{
		Event:      "platform.legal_document_published",
		ActorType:  "platform",
		TargetType: "legal",
		TargetID:   slug,
		Metadata:   map[string]string{"slug": slug},
	})
	w.WriteHeader(http.StatusNoContent)
}
