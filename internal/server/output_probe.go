package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// maxOutputProbeBytes bounds how much pre-output upstream data Tiller buffers to
// decide whether an ordered-fallback target actually produced assistant output.
// Beyond this the probe is inconclusive and the caller commits to the target
// (never guesses a fallback), so buffering stays bounded.
const maxOutputProbeBytes int64 = 1 << 20

type probeOutcome int

const (
	// probeOutput: a non-empty text/reasoning/tool delta was observed; commit.
	probeOutput probeOutcome = iota
	// probeStreamError: the upstream reported an explicit stream failure.
	probeStreamError
	// probeEmpty: the stream reached a terminal event with no assistant output.
	probeEmpty
	// probeInconclusive: the buffer cap was hit before a decision; commit.
	probeInconclusive
)

// probeUpstreamOutput peeks an already-preflighted upstream response to decide
// whether it carries assistant output, an explicit upstream stream error, or a
// terminal-but-empty completion. Nothing is written to the client and
// resp.Body is always rewound, so the normal relay replays the full stream.
//
// Only consulted for ordered-fallback virtual routes, where a target that
// delivers a 2xx with no usable output must be eligible for fallback.
func probeUpstreamOutput(resp *http.Response, target providers.Protocol) (probeOutcome, error) {
	if !isStreamingResponse(resp) {
		// Non-streaming 2xx responses keep the existing success contract: an
		// upstream failure surfaces as a non-2xx status, which the caller
		// already treats as fallback-eligible before this point.
		return probeOutput, nil
	}

	recorded := &bytes.Buffer{}
	underlying := resp.Body
	reader := bufio.NewReader(io.TeeReader(underlying, recorded))
	defer func() {
		resp.Body = bufferedReadCloser{
			Reader: io.MultiReader(bytes.NewReader(recorded.Bytes()), underlying),
			closer: underlying,
		}
	}()

	state := &streamState{reasoningIndex: -1, messageIndex: -1, toolIndex: -1}
	for {
		if int64(recorded.Len()) > maxOutputProbeBytes {
			return probeInconclusive, nil
		}
		event, err := readSSEEvent(reader)
		data := event.Data
		if len(data) > 0 {
			if string(data) == "[DONE]" {
				return probeEmpty, nil
			}
			var payload map[string]any
			if json.Unmarshal(data, &payload) == nil {
				deltas, done := canonicalDeltas(event.Name, payload, target, state)
				for _, delta := range deltas {
					if delta.Kind == "error" {
						return probeStreamError, nil
					}
					if probeDeltaHasOutput(delta) {
						return probeOutput, nil
					}
				}
				if done {
					return probeEmpty, nil
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				// The stream ended without a terminal event. Output already
				// observed returns above; otherwise treat it as empty.
				return probeEmpty, nil
			}
			return probeInconclusive, err
		}
	}
}

// probeDeltaHasOutput reports whether a canonical delta carries client-visible
// assistant output. Reasoning counts: a reasoning-only reply is a valid
// (non-empty) completion even when no visible text follows.
func probeDeltaHasOutput(delta canonicalDelta) bool {
	switch delta.Kind {
	case "text", "reasoning":
		return delta.Text != ""
	case "tool":
		return delta.CallID != "" || delta.Name != "" || delta.Arguments != ""
	}
	return false
}
