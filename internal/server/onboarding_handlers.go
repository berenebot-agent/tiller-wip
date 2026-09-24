package server

import (
	"archive/zip"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/store"
)

// WS3 — onboarding + export handlers. Own this file.
// Implements the endpoint contract in docs/stage_d_api_contract.md.

// onboardingSettingKey is the account setting that remembers "skip setup".
const onboardingSettingKey = "onboarding_dismissed"

// writeOnboardingState reports whether the first-run wizard should be shown.
// `needs_onboarding` is true until the account has a 2xx routed request in
// Activity or the user dismisses the wizard. When Activity is unavailable the
// successful-request check degrades to "not yet" (the wizard still shows, and
// dismissal still works) rather than failing the page.
func (s *Server) writeOnboardingState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc := s.scope(r)
	dismissed := false
	if value, err := sc.GetBool(ctx, onboardingSettingKey); err == nil {
		dismissed = value
	}
	firstRequestAt, err := sc.FirstRoutedRequestAt(ctx)
	if err != nil && !errors.Is(err, store.ErrActivityUnavailable) {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not load onboarding state.")
		return
	}
	needsOnboarding := !dismissed && firstRequestAt == ""
	writeJSON(w, http.StatusOK, map[string]any{
		"needs_onboarding": needsOnboarding,
		"dismissed":        dismissed,
		"first_request_at": firstRequestAt,
	})
}

// setOnboardingDismissed remembers that the user skipped setup.
func (s *Server) setOnboardingDismissed(w http.ResponseWriter, r *http.Request) {
	if err := s.scope(r).SetSetting(r.Context(), onboardingSettingKey, "true"); err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not save your preference.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// accountExportConfig is the config.json payload: account profile, plan limits,
// account settings, catalogues, virtual routing, and non-secret client-key
// metadata. It never carries provider credentials, OAuth tokens, API-key
// secrets, selectors or hashes.
type accountExportConfig struct {
	Profile        identity.AccountProfile            `json:"profile"`
	Plan           store.Plan                         `json:"plan"`
	Settings       map[string]string                  `json:"settings"`
	Providers      []store.AccountExportProvider      `json:"providers"`
	ProviderModels []store.AccountExportModel         `json:"provider_models"`
	VirtualGroups  []store.AccountExportVirtualGroup  `json:"virtual_groups"`
	VirtualModels  []store.AccountExportVirtualModel  `json:"virtual_models"`
	VirtualTargets []store.AccountExportVirtualTarget `json:"virtual_targets"`
	ClientKeys     []store.AccountExportClientKey     `json:"client_keys"`
}

// writeAccountExport streams the account-scoped ZIP export. All reads used to
// build config.json and audit.csv are completed before any byte is written, so
// a failure can still surface as a clean error status. Activity is streamed
// into the ZIP in bounded pages (never one long tenant transaction).
func (s *Server) writeAccountExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc := s.scope(r)
	user := r.Context().Value(userKey).(identity.User)

	if !sc.ActivityAvailable() {
		activityReadError(w, store.ErrActivityUnavailable, "Could not export account data.")
		return
	}
	activityCount, err := sc.CountAccountActivityRows(ctx)
	if err != nil {
		activityReadError(w, err, "Could not export account data.")
		return
	}
	auditCount, err := sc.CountAccountAuditRows(ctx)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not export account data.")
		return
	}
	if activityCount+auditCount > store.AccountExportRowCap {
		adminError(w, http.StatusRequestEntityTooLarge, "export_too_large", "This account has too much data to export in one archive.")
		return
	}

	profile, err := s.identity.AccountProfile(ctx, user.ID)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not export account data.")
		return
	}
	plan, err := s.storeHandle().EntitlementsForAccount(ctx, sc.AccountID())
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not export account data.")
		return
	}
	config, err := sc.BuildAccountExportConfig(ctx)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not export account data.")
		return
	}
	auditRows, err := sc.ListAccountAudit(ctx, store.AccountExportRowCap, 0)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "database_error", "Could not export account data.")
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "tiller-account-export-"+time.Now().UTC().Format("2006-01-02")+".zip"))
	w.Header().Set("Cache-Control", "no-store")

	zw := zip.NewWriter(w)
	defer zw.Close()

	configJSON := accountExportConfig{
		Profile: profile, Plan: plan, Settings: config.Settings,
		Providers: config.Providers, ProviderModels: config.Models,
		VirtualGroups: config.VirtualGroups, VirtualModels: config.VirtualModels,
		VirtualTargets: config.VirtualTargets, ClientKeys: config.ClientKeys,
	}
	configWriter, err := zw.CreateHeader(&zip.FileHeader{Name: "config.json", Method: zip.Deflate, Modified: time.Now().UTC()})
	if err != nil {
		s.logger.Error("account export failed", "entry", "config.json", "error_class", fmt.Sprintf("%T", err))
		return
	}
	encoder := json.NewEncoder(configWriter)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(configJSON); err != nil {
		s.logger.Error("account export failed", "entry", "config.json", "error_class", fmt.Sprintf("%T", err))
		return
	}

	activityWriter, err := zw.CreateHeader(&zip.FileHeader{Name: "activity.csv", Method: zip.Deflate, Modified: time.Now().UTC()})
	if err != nil {
		s.logger.Error("account export failed", "entry", "activity.csv", "error_class", fmt.Sprintf("%T", err))
		return
	}
	if err := s.writeAccountActivityCSV(activityWriter, func(fn func(store.ActivityRow) error) error {
		return sc.ExportAccountActivity(ctx, fn)
	}); err != nil {
		s.logger.Warn("account export activity failed", "error_class", fmt.Sprintf("%T", err))
		return
	}

	auditWriter, err := zw.CreateHeader(&zip.FileHeader{Name: "audit.csv", Method: zip.Deflate, Modified: time.Now().UTC()})
	if err != nil {
		s.logger.Error("account export failed", "entry", "audit.csv", "error_class", fmt.Sprintf("%T", err))
		return
	}
	if err := writeAccountAuditCSV(auditWriter, auditRows); err != nil {
		s.logger.Warn("account export audit failed", "error_class", fmt.Sprintf("%T", err))
	}
}

// writeAccountActivityCSV streams Activity metadata as CSV into the export ZIP.
// Unlike the admin CSV export it deliberately omits request_body and error_body
// content, and it never includes provider credentials.
func (s *Server) writeAccountActivityCSV(out io.Writer, run func(fn func(store.ActivityRow) error) error) error {
	cw := csv.NewWriter(out)
	write := func(record []string) error {
		if err := cw.Write(record); err != nil {
			return err
		}
		return cw.Error()
	}
	if err := write([]string{
		"timestamp", "client_key", "client_requested_model", "client_exposed_model",
		"virtual_model", "bound_target", "final_provider", "final_model", "protocol",
		"streaming", "http_status", "latency_ms", "input_tokens", "output_tokens",
		"cached_input_tokens", "cache_creation_input_tokens", "attempt_count",
		"fallback_used", "fallback_reason", "error_message", "provider_request_id",
		"client_request_id", "route_kind",
	}); err != nil {
		return err
	}
	written := 0
	if err := run(func(row store.ActivityRow) error {
		virtualModel := ""
		if row.RouteKind != nil && *row.RouteKind == "virtual" && row.RouteModel != nil {
			virtualModel = *row.RouteModel
		}
		if err := write([]string{
			row.CreatedAt, neutralizeCSVField(row.ClientName), neutralizeCSVField(row.RequestedModel),
			neutralizeCSVField(strPtrOrEmpty(row.ExposedModel)), neutralizeCSVField(virtualModel),
			neutralizeCSVField(strPtrOrEmpty(row.RouteModel)), neutralizeCSVField(strPtrOrEmpty(row.ResolvedProvider)),
			neutralizeCSVField(strPtrOrEmpty(row.ResolvedModel)), row.Protocol,
			boolString(row.Streaming), intString(row.HTTPStatus), int64String(row.LatencyMs),
			int64PtrOrEmpty(row.InputTokens), int64PtrOrEmpty(row.OutputTokens), int64PtrOrEmpty(row.CacheReadInputTokens),
			int64PtrOrEmpty(row.CacheCreationInputTokens), intString(row.AttemptCount), boolString(row.FallbackUsed),
			strPtrOrEmpty(row.FallbackReason), neutralizeCSVField(strPtrOrEmpty(row.ErrorMessage)),
			neutralizeCSVField(strPtrOrEmpty(row.ProviderRequestID)), row.ClientRequestID,
			strPtrOrEmpty(row.RouteKind),
		}); err != nil {
			return err
		}
		written++
		if written%64 == 0 {
			cw.Flush()
			return cw.Error()
		}
		return nil
	}); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// writeAccountAuditCSV writes the account's audit events as CSV into the export
// ZIP.
func writeAccountAuditCSV(out io.Writer, rows []store.AuditRow) error {
	cw := csv.NewWriter(out)
	write := func(record []string) error {
		if err := cw.Write(record); err != nil {
			return err
		}
		return cw.Error()
	}
	if err := write([]string{"id", "event", "actor_type", "actor_id", "target_type", "target_id", "outcome", "metadata", "created_at"}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := write([]string{
			row.ID, neutralizeCSVField(row.Event), neutralizeCSVField(row.ActorType),
			neutralizeCSVField(row.ActorID), neutralizeCSVField(row.TargetType),
			neutralizeCSVField(row.TargetID), row.Outcome, neutralizeCSVField(row.Metadata), row.CreatedAt,
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func intString(v int) string { return fmt.Sprintf("%d", v) }

func int64String(v int64) string { return fmt.Sprintf("%d", v) }
