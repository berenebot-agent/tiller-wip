package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

func TestSafeRefreshErrorDoesNotExposeRequestDetails(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "deadline", err: context.DeadlineExceeded, want: "Provider discovery timed out."},
		{name: "cancelled", err: context.Canceled, want: "Provider discovery was cancelled."},
		{name: "http status", err: errors.New("model discovery returned HTTP 401"), want: "Provider discovery returned HTTP 401."},
		{name: "decode", err: errors.New(`decode model catalogue: invalid character 's' looking for beginning of value`), want: "Provider discovery failed."},
		{name: "transport URL", err: &url.Error{Op: "Get", URL: "https://example.test/models?api_key=secret", Err: fmt.Errorf("dial failed")}, want: "Provider discovery request failed."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeRefreshError(tt.err); got != tt.want {
				t.Fatalf("safeRefreshError() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRefreshDueRefreshesOverdueProviders locks in the background-refresh
// cadence contract: refreshDue refreshes enabled providers whose
// next_refresh_at is NULL or past (pushing the next run ~24h out) and leaves
// providers with a future next_refresh_at untouched. A newly-added upstream
// model (e.g. a new OpenCode free-tier model) is therefore picked up without
// a manual admin refresh.
func TestRefreshDueRefreshesOverdueProviders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "deepseek-v4-flash-free"},
		}})
	}))
	defer upstream.Close()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := database.Now()
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('overdue','real','provider-overdue'),('fresh','real','provider-fresh')`); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO providers(id,name,type,base_url,credential_secret,enabled,protocols,next_refresh_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`
	if _, err := db.SQL.Exec(insert, "provider-overdue", "overdue", "opencode-free", upstream.URL+"/v1", nil, 1, EncodeProtocols([]Protocol{ProtocolChat, ProtocolResponses}), past, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(insert, "provider-fresh", "fresh", "opencode-free", upstream.URL+"/v1", nil, 1, EncodeProtocols([]Protocol{ProtocolChat, ProtocolResponses}), future, now, now); err != nil {
		t.Fatal(err)
	}

	m := NewManager(db.SQL, NewRegistry())
	m.refreshDue(context.Background())

	deadline := time.Now().Add(30 * time.Second)
	for {
		var lastRefresh sql.NullString
		if err := db.SQL.QueryRow(`SELECT last_refresh_at FROM providers WHERE id='provider-overdue'`).Scan(&lastRefresh); err != nil {
			t.Fatal(err)
		}
		if lastRefresh.Valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("overdue provider was not refreshed within 30s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	var next string
	var lastErr sql.NullString
	if err := db.SQL.QueryRow(`SELECT next_refresh_at,last_refresh_error FROM providers WHERE id='provider-overdue'`).Scan(&next, &lastErr); err != nil {
		t.Fatal(err)
	}
	if lastErr.Valid {
		t.Fatalf("overdue provider refresh recorded error %q", lastErr.String)
	}
	nextTime, err := time.Parse(time.RFC3339Nano, next)
	if err != nil {
		t.Fatalf("next_refresh_at %q is not RFC3339Nano: %v", next, err)
	}
	if delta := time.Until(nextTime); delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("next_refresh_at is %v out, want ~24h", delta)
	}
	var modelCount int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM provider_models WHERE provider_id='provider-overdue' AND upstream_model_id='deepseek-v4-flash-free' AND available=1`).Scan(&modelCount); err != nil {
		t.Fatal(err)
	}
	if modelCount != 1 {
		t.Fatalf("expected discovered free model stored for overdue provider, got %d rows", modelCount)
	}

	var freshLast sql.NullString
	var freshModels int
	if err := db.SQL.QueryRow(`SELECT last_refresh_at FROM providers WHERE id='provider-fresh'`).Scan(&freshLast); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM provider_models WHERE provider_id='provider-fresh'`).Scan(&freshModels); err != nil {
		t.Fatal(err)
	}
	if freshLast.Valid || freshModels != 0 {
		t.Fatalf("provider with future next_refresh_at must be untouched, got last_refresh_at valid=%v models=%d", freshLast.Valid, freshModels)
	}
}
