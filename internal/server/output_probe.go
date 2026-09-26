package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/tiller-router/tiller-router/internal/providers"
)

// maxOutputProbeBytes bounds how much pre-output upstream data Tiller buffers to
// decide whether an ordered-fallback target actually produced assistant output.
// Beyond this the probe is inconclusive and the caller commits to the target
// (never guesses a fallback), so buffering stays bounded.
//
// The budget is enforced at the reader boundary, not just between SSE events:
// every byte the probe consumes — including SSE comment/blank lines that
// readSSEEvent skips forever, and any buffered read-ahead inside the
// bufio.Reader — is counted by probeBudgetReader. Without that boundary a
// comment-only stream would grow the recorded buffer without bound while
// readSSEEvent never returned.
const maxOutputProbeBytes int64 = 1 << 20

// probeBudgetSlack lets the reader boundary tolerate one over-budget read
// (the read that crosses the cap) without misclassifying an exactly-at-budget
// stream. The recorded-buffer check below remains the authoritative decision.
const probeBudgetSlack int64 = 64 << 10

// errProbeBudgetExceeded is the sentinel returned by probeBudgetReader once the
// cumulative bytes read pass the probe budget. The probe maps it to the
// documented bounded fallback (probeInconclusive, nil).
var errProbeBudgetExceeded = errors.New("output probe budget exceeded")

// probeBudgetReader counts every byte read from the wrapped upstream body and
// refuses to read past the budget. It sits between resp.Body and the
// bufio.Reader/TeeReader so the counter sees comment lines and buffered
// read-ahead too, not only the bytes that become an SSE event.
type probeBudgetReader struct {
	reader io.Reader
	budget int64
	read   int64
}

func (r *probeBudgetReader) Read(p []byte) (int, error) {
	if r.read > r.budget {
		return 0, errProbeBudgetExceeded
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if r.read > r.budget {
		// Report the bytes just read so the caller can record them, then
		// surface the sentinel on the next call. The probe treats hitting the
		// budget as inconclusive rather than an upstream failure.
		return n, errProbeBudgetExceeded
	}
	return n, err
}

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
	// The budget reader wraps the raw body before bufio/Tee so comment lines
	// and bufio read-ahead are counted too. `underlying` still refers to the
	// raw body so the deferred rewind replays recorded bytes + remainder.
	budgeted := &probeBudgetReader{reader: underlying, budget: maxOutputProbeBytes + probeBudgetSlack}
	reader := bufio.NewReader(io.TeeReader(budgeted, recorded))
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
			if errors.Is(err, errProbeBudgetExceeded) {
				// The comment-only / read-ahead budget was exhausted before a
				// decision. Bounded fallback: commit to the target rather than
				// buffering without limit.
				return probeInconclusive, nil
			}
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
