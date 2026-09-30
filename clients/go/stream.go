package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"
)

type Stream struct {
	ctx       context.Context
	cancel    context.CancelFunc
	body      io.ReadCloser
	scanner   *bufio.Scanner
	stop      func() bool
	once      sync.Once
	nextMu    sync.Mutex
	cfg       Config
	pending   []Event
	terminal  bool
	args      int
	bytes     int64
	requestID string
}

func (s *Stream) String() string   { return "gateway.Stream" }
func (s *Stream) GoString() string { return s.String() }

func (c *Client) Stream(ctx context.Context, r Request) (*Stream, error) {
	b, err := encodeRequest(r, true)
	if err != nil {
		return nil, err
	}
	if c.lifetime.Err() != nil {
		return nil, failure("cancelled")
	}
	ctx, cancel := c.requestContext(ctx)
	res, err := c.send(ctx, http.MethodPost, c.endpoint("chat/completions"), b)
	if err != nil {
		cancel()
		return nil, err
	}
	media, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" {
		res.Body.Close()
		cancel()
		return nil, failure("malformed_response")
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, min(4096, c.cfg.MaxLineBytes+2)), c.cfg.MaxLineBytes+2)
	s := &Stream{ctx: ctx, cancel: cancel, body: res.Body, scanner: scanner, cfg: c.cfg, requestID: requestID(res.Header, c.cfg.APIKey)}
	s.stop = context.AfterFunc(ctx, func() { res.Body.Close() })
	return s, nil
}

func (s *Stream) Close() error {
	s.once.Do(func() {
		s.cancel()
		if s.stop != nil {
			s.stop()
		}
		s.body.Close()
	})
	return nil
}

// Next yields ordered native frames and typed fragments. Completion occurs only
// after [DONE]; malformed/error frames fail immediately, even before [DONE].
// Call Close when abandoning iteration to release the upstream response.
func (s *Stream) Next() (Event, error) {
	s.nextMu.Lock()
	defer s.nextMu.Unlock()
	if s.terminal {
		return Event{}, io.EOF
	}
	if s.ctx.Err() != nil {
		return s.fail(transportError(s.ctx, s.ctx.Err()))
	}
	if len(s.pending) > 0 {
		e := s.pending[0]
		s.pending = s.pending[1:]
		return e, nil
	}
	for {
		data := make([]string, 0)
		size := 0
		eof := false
		for {
			if !s.scanner.Scan() {
				if s.ctx.Err() != nil {
					return s.fail(transportError(s.ctx, s.ctx.Err()))
				}
				if err := s.scanner.Err(); err != nil {
					if err == bufio.ErrTooLong {
						return s.fail(failure("stream"))
					}
					return s.fail(transportError(s.ctx, err))
				}
				eof = true
				break
			}
			line := s.scanner.Text()
			s.bytes += int64(len(line) + 1)
			size += len(line) + 1
			if len(line) > s.cfg.MaxLineBytes || size > s.cfg.MaxEventBytes || s.bytes > s.cfg.MaxBodyBytes {
				return s.fail(failure("stream"))
			}
			if line == "" {
				break
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			field, value, has := strings.Cut(line, ":")
			if !has {
				value = ""
			}
			value = strings.TrimPrefix(value, " ")
			if field == "data" {
				data = append(data, value)
			}
		}
		if len(data) == 0 {
			if eof {
				return s.fail(failure("stream"))
			}
			continue
		}
		raw := strings.Join(data, "\n")
		if raw == "[DONE]" {
			s.terminal = true
			s.Close()
			return Event{Kind: CompletionEvent, Raw: json.RawMessage(`"[DONE]"`)}, nil
		}
		events, err := s.decode([]byte(raw))
		if err != nil {
			return s.fail(err)
		}
		s.pending = events
		if len(events) > 0 {
			e := s.pending[0]
			s.pending = s.pending[1:]
			return e, nil
		}
		if eof {
			return s.fail(failure("stream"))
		}
	}
}

func (s *Stream) fail(err error) (Event, error) {
	s.terminal = true
	s.pending = nil
	s.Close()
	if e, ok := err.(*Error); ok {
		e.RequestID = s.requestID
	}
	return Event{}, err
}

func (s *Stream) decode(raw json.RawMessage) ([]Event, error) {
	var wire struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content          *string         `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				ReasoningDetails json.RawMessage `json:"reasoning_details"`
				ToolCalls        []ToolCall      `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &wire) != nil || !json.Valid(raw) || len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw))[0] != '{' {
		return nil, failure("malformed_response")
	}
	if len(wire.Error) > 0 && string(wire.Error) != "null" {
		e := responseError(&http.Response{StatusCode: 200, Header: http.Header{}}, raw, s.cfg.APIKey)
		if e.Category == "http" {
			e.Category = "stream"
		}
		return nil, e
	}
	if wire.Choices == nil {
		return nil, failure("malformed_response")
	}
	var shape struct {
		Choices []map[string]json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(raw, &shape) != nil {
		return nil, failure("malformed_response")
	}
	for _, choice := range shape.Choices {
		if choice == nil {
			return nil, failure("malformed_response")
		}
		delta, hasDelta := choice["delta"]
		finish, hasFinish := choice["finish_reason"]
		if !hasDelta && (!hasFinish || string(finish) == "null") {
			return nil, failure("malformed_response")
		}
		if hasDelta {
			var object map[string]json.RawMessage
			if json.Unmarshal(delta, &object) != nil || object == nil {
				return nil, failure("malformed_response")
			}
		}
	}
	out := []Event{{Kind: FrameEvent, Raw: raw}}
	for _, ch := range wire.Choices {
		if ch.Index < 0 {
			return nil, failure("malformed_response")
		}
		if ch.FinishReason != nil && *ch.FinishReason == "error" {
			return nil, failure("stream")
		}
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			out = append(out, Event{Kind: TextEvent, ChoiceIndex: ch.Index, Text: *ch.Delta.Content, Raw: raw})
		}
		for _, reason := range []json.RawMessage{ch.Delta.ReasoningContent, ch.Delta.Reasoning, ch.Delta.ReasoningDetails} {
			if len(reason) > 0 && string(reason) != "null" && string(reason) != `""` {
				var text string
				_ = json.Unmarshal(reason, &text)
				out = append(out, Event{Kind: ReasoningEvent, ChoiceIndex: ch.Index, Text: text, Reasoning: reason, Raw: raw})
			}
		}
		for _, tool := range ch.Delta.ToolCalls {
			if ch.Index < 0 || tool.Index < 0 {
				return nil, failure("malformed_response")
			}
			s.args += len(tool.Function.Arguments)
			if s.args > s.cfg.MaxArgumentBytes {
				return nil, failure("stream")
			}
			t := tool
			out = append(out, Event{Kind: ToolEvent, ChoiceIndex: ch.Index, Tool: &t, Raw: raw})
		}
		if ch.FinishReason != nil {
			out = append(out, Event{Kind: FinishEvent, ChoiceIndex: ch.Index, FinishReason: *ch.FinishReason, Raw: raw})
		}
	}
	usage, err := parseUsage(wire.Usage)
	if err != nil {
		return nil, err
	}
	if usage != nil {
		out = append(out, Event{Kind: UsageEvent, Usage: usage, Raw: raw})
	}
	return out, nil
}
