package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func sseFrames(t *testing.T, out string) []map[string]any {
	t.Helper()
	frames := []map[string]any{}
	for _, line := range strings.Split(out, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("bad SSE frame %q: %v", data, err)
		}
		frames = append(frames, payload)
	}
	return frames
}
