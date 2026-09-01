package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	buildversion "github.com/tiller-router/tiller-router/internal/version"
)

func TestVersionHealthEndpoint(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health/version", nil)
	(&Server{}).versionHealth(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var got map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["version"] != buildversion.Version || got["commit"] != buildversion.Commit {
		t.Fatalf("version response = %#v, want version=%q commit=%q", got, buildversion.Version, buildversion.Commit)
	}
}
