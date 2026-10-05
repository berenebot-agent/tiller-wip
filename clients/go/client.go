package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Client struct {
	cfg      Config
	base     *url.URL
	http     *http.Client
	mu       sync.Mutex
	catalog  []Model
	cachedAt time.Time
	owned    bool
	lifetime context.Context
	shutdown context.CancelFunc
}

func (c *Client) String() string   { return "gateway.Client" }
func (c *Client) GoString() string { return c.String() }

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, failure("invalid_request")
	}
	if cfg.Profile != Tiller && cfg.Profile != OpenRouter {
		return nil, failure("invalid_request")
	}
	for _, r := range cfg.APIKey {
		if r <= 32 || r >= 127 {
			return nil, failure("invalid_request")
		}
	}
	if cfg.APIKey == "" {
		return nil, failure("invalid_request")
	}
	if cfg.CatalogPath != "" {
		p, e := url.Parse(cfg.CatalogPath)
		if e != nil || p.IsAbs() || p.Host != "" || strings.HasPrefix(p.Path, "/") || p.RawQuery != "" || p.Fragment != "" {
			return nil, failure("invalid_request")
		}
		decoded := cfg.CatalogPath
		for {
			for _, r := range decoded {
				if r < 32 || r == 127 || r == '\\' {
					return nil, failure("invalid_request")
				}
			}
			if strings.HasPrefix(decoded, "/") {
				return nil, failure("invalid_request")
			}
			for _, segment := range strings.Split(decoded, "/") {
				if segment == "." || segment == ".." {
					return nil, failure("invalid_request")
				}
			}
			next, err := url.PathUnescape(decoded)
			if err != nil {
				return nil, failure("invalid_request")
			}
			if next == decoded {
				break
			}
			decoded = next
		}
	}
	if cfg.Timeout < 0 || cfg.CacheTTL < 0 || cfg.MaxBodyBytes < 0 || cfg.MaxLineBytes < 0 || cfg.MaxEventBytes < 0 || cfg.MaxArgumentBytes < 0 || cfg.MaxCatalogPages < 0 {
		return nil, failure("invalid_request")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 8 << 20
	}
	if cfg.MaxLineBytes == 0 {
		cfg.MaxLineBytes = 1 << 20
	}
	if cfg.MaxEventBytes == 0 {
		cfg.MaxEventBytes = 2 << 20
	}
	if cfg.MaxArgumentBytes == 0 {
		cfg.MaxArgumentBytes = 1 << 20
	}
	if cfg.MaxCatalogPages == 0 {
		cfg.MaxCatalogPages = 100
	}
	h := &http.Client{}
	owned := cfg.HTTPClient == nil
	if !owned {
		*h = *cfg.HTTPClient
	} else {
		if transport, ok := http.DefaultTransport.(*http.Transport); ok {
			h.Transport = transport.Clone()
		} else {
			h.Transport = (&http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second}).Clone()
		}
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	u.Path = strings.TrimRight(u.Path, "/") + "/"
	u.RawPath = ""
	cfg.HTTPClient = nil
	lifetime, shutdown := context.WithCancel(context.Background())
	return &Client{cfg: cfg, base: u, http: h, owned: owned, lifetime: lifetime, shutdown: shutdown}, nil
}

// Close prevents further requests and cancels active requests and streams.
// It closes idle connections only on HTTP clients created by this SDK.
func (c *Client) Close() {
	c.shutdown()
	if c.owned {
		c.http.CloseIdleConnections()
	}
}
func (c *Client) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, c.cfg.Timeout)
	stop := context.AfterFunc(c.lifetime, cancel)
	if c.lifetime.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func failure(category string) *Error { return &Error{Category: category} }
func transportError(ctx context.Context, err error) *Error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return failure("cancelled")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return failure("timeout")
	}
	var n interface{ Timeout() bool }
	if errors.As(err, &n) && n.Timeout() {
		return failure("timeout")
	}
	return failure("transport")
}

func (c *Client) endpoint(path string) string { return c.base.String() + path }
func (c *Client) send(ctx context.Context, method, address string, body []byte) (*http.Response, error) {
	if int64(len(body)) > c.cfg.MaxBodyBytes {
		return nil, failure("invalid_request")
	}
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		return nil, failure("invalid_request")
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	var envelope struct {
		Stream bool `json:"stream"`
	}
	if body != nil && json.Unmarshal(body, &envelope) == nil && envelope.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
		return nil, transportError(ctx, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		b, err := io.ReadAll(io.LimitReader(res.Body, c.cfg.MaxBodyBytes))
		if err != nil {
			return nil, transportError(ctx, err)
		}
		return nil, responseError(res, b, c.cfg.APIKey)
	}
	return res, nil
}

func safeToken(s, secret string) string {
	if len(s) > 128 || secret != "" && strings.Contains(s, secret) {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == ':') {
			return ""
		}
	}
	return s
}
func requestID(headers http.Header, secret string) string {
	for _, key := range []string{"X-Request-ID", "Request-Id", "X-Generation-Id", "X-Tiller-Request-Id"} {
		if value := safeToken(headers.Get(key), secret); value != "" {
			return value
		}
	}
	return ""
}

var errorCategories = map[string]string{
	"invalid_request_error": "invalid_request", "invalid_api_key": "authentication", "authentication_error": "authentication",
	"permission_denied": "authorization", "model_not_found": "model_unavailable", "model_unavailable": "model_unavailable",
	"rate_limit_exceeded": "rate_limit", "rate_limit_error": "rate_limit", "context_length_exceeded": "context_limit",
	"context_limit_exceeded": "context_limit", "unsupported_feature": "unsupported_feature", "unsupported_parameter": "unsupported_feature", "server_error": "http",
}

func responseError(res *http.Response, b []byte, secret string) *Error {
	e := failure("http")
	e.Status = res.StatusCode
	e.RequestID = requestID(res.Header, secret)
	retry := res.Header.Get("Retry-After")
	if len(retry) <= 128 && !strings.Contains(retry, secret) {
		valid := true
		for _, r := range retry {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(" ,:+-", r)) {
				valid = false
			}
		}
		if valid {
			e.RetryAfter = retry
		}
	}
	var env struct {
		Error struct {
			Code json.RawMessage `json:"code"`
			Type string          `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil {
		var code string
		if json.Unmarshal(env.Error.Code, &code) != nil {
			code = string(env.Error.Code)
		}
		if _, ok := errorCategories[code]; ok {
			e.Code = safeToken(code, secret)
		}
		if _, ok := errorCategories[env.Error.Type]; ok {
			e.Type = safeToken(env.Error.Type, secret)
		}
	}
	switch res.StatusCode {
	case 408, 504:
		e.Category = "timeout"
	case 400, 422:
		e.Category = "invalid_request"
	case 401:
		e.Category = "authentication"
	case 402:
		e.Category = "billing"
	case 403:
		e.Category = "authorization"
	case 404:
		e.Category = "model_unavailable"
	case 429:
		e.Category = "rate_limit"
	}
	if res.StatusCode != 401 && res.StatusCode != 403 && res.StatusCode != 402 && res.StatusCode != 429 {
		if category, ok := errorCategories[e.Type]; ok {
			e.Category = category
		}
		if category, ok := errorCategories[e.Code]; ok {
			e.Category = category
		}
	}
	return e
}

func (c *Client) read(ctx context.Context, res *http.Response) ([]byte, error) {
	return c.readBounded(ctx, res, c.cfg.MaxBodyBytes)
}

func (c *Client) readBounded(ctx context.Context, res *http.Response, limit int64) ([]byte, error) {
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if int64(len(b)) > limit {
		return nil, failure("malformed_response")
	}
	return b, nil
}

func encodeRequest(r Request, stream bool) ([]byte, error) {
	if strings.TrimSpace(r.Model) == "" || len(r.Messages) == 0 {
		return nil, failure("invalid_request")
	}
	m := map[string]any{"model": r.Model, "messages": r.Messages, "stream": stream}
	reserved := map[string]bool{"model": true, "messages": true, "stream": true, "tools": true, "tool_choice": true, "response_format": true, "max_tokens": true, "temperature": true, "top_p": true, "reasoning": true, "stream_options": true, "authorization": true, "api_key": true, "headers": true, "base_url": true}
	for k, v := range r.Extra {
		if reserved[strings.ToLower(k)] {
			return nil, failure("invalid_request")
		}
		m[k] = v
	}
	if r.Tools != nil {
		m["tools"] = r.Tools
	}
	if r.ToolChoice != nil {
		m["tool_choice"] = r.ToolChoice
	}
	if r.ResponseFormat != nil {
		m["response_format"] = r.ResponseFormat
	}
	if r.MaxTokens != nil {
		m["max_tokens"] = *r.MaxTokens
	}
	if r.Temperature != nil {
		m["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		m["top_p"] = *r.TopP
	}
	if r.Reasoning != nil {
		m["reasoning"] = r.Reasoning
	}
	if stream {
		m["stream_options"] = map[string]bool{"include_usage": true}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, failure("invalid_request")
	}
	return b, nil
}

func parseUsage(raw json.RawMessage) (*Usage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var u Usage
	if json.Unmarshal(raw, &u) != nil {
		return nil, failure("malformed_response")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, failure("malformed_response")
	}
	if _, present := fields["prompt_tokens"]; !present {
		u.PromptTokens = integer(fields["input_tokens"])
	}
	if _, present := fields["completion_tokens"]; !present {
		u.CompletionTokens = integer(fields["output_tokens"])
	}
	for _, name := range []string{"prompt_tokens_details", "input_tokens_details"} {
		var details map[string]json.RawMessage
		_ = json.Unmarshal(fields[name], &details)
		if u.CacheReadTokens == nil {
			u.CacheReadTokens = integer(details["cached_tokens"])
		}
	}
	if u.CacheReadTokens == nil {
		u.CacheReadTokens = integer(fields["cache_read_input_tokens"])
	}
	u.CacheWriteTokens = integer(fields["cache_creation_input_tokens"])
	u.Raw = append(json.RawMessage(nil), raw...)
	return &u, nil
}

func (c *Client) Chat(ctx context.Context, r Request) (*Result, error) {
	b, err := encodeRequest(r, false)
	if err != nil {
		return nil, err
	}
	if c.lifetime.Err() != nil {
		return nil, failure("cancelled")
	}
	ctx, cancel := c.requestContext(ctx)
	defer cancel()
	res, err := c.send(ctx, http.MethodPost, c.endpoint("chat/completions"), b)
	if err != nil {
		return nil, err
	}
	b, err = c.read(ctx, res)
	if err != nil {
		return nil, err
	}
	var wire struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Error   json.RawMessage `json:"error"`
		Usage   json.RawMessage `json:"usage"`
		Choices []struct {
			Index        int             `json:"index"`
			Message      json.RawMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &wire) != nil {
		return nil, failure("malformed_response")
	}
	if len(wire.Error) > 0 && string(wire.Error) != "null" {
		return nil, responseError(res, b, c.cfg.APIKey)
	}
	if len(wire.Choices) == 0 {
		return nil, failure("malformed_response")
	}
	out := &Result{ID: wire.ID, Model: wire.Model, RequestID: requestID(res.Header, c.cfg.APIKey), Raw: b}
	out.Usage, err = parseUsage(wire.Usage)
	if err != nil {
		return nil, err
	}
	argumentBytes := 0
	for _, w := range wire.Choices {
		if w.FinishReason == "error" {
			return nil, responseError(res, nil, c.cfg.APIKey)
		}
		var m Message
		var typed struct {
			ToolCalls []ToolCall `json:"tool_calls"`
		}
		decoder := json.NewDecoder(bytes.NewReader(w.Message))
		decoder.UseNumber()
		if decoder.Decode(&m) != nil || m == nil || json.Unmarshal(w.Message, &typed) != nil {
			return nil, failure("malformed_response")
		}
		if m["role"] != "assistant" {
			return nil, failure("malformed_response")
		}
		if content := m["content"]; content != nil {
			switch content.(type) {
			case string, []any:
			default:
				return nil, failure("malformed_response")
			}
		}
		for _, tool := range typed.ToolCalls {
			argumentBytes += len(tool.Function.Arguments)
			if tool.ID == "" || tool.Type != "function" || tool.Function.Name == "" || !json.Valid([]byte(tool.Function.Arguments)) || argumentBytes > c.cfg.MaxArgumentBytes {
				return nil, failure("malformed_response")
			}
		}
		if _, contentPresent := m["content"]; !contentPresent && len(typed.ToolCalls) == 0 && m["reasoning_content"] == nil && m["reasoning"] == nil && m["reasoning_details"] == nil {
			return nil, failure("malformed_response")
		}
		reasoning := m["reasoning_content"]
		if reasoning == nil {
			reasoning = m["reasoning"]
		}
		out.Choices = append(out.Choices, Choice{Index: w.Index, Message: m, Content: m["content"], ToolCalls: typed.ToolCalls, Reasoning: reasoning, FinishReason: w.FinishReason, Native: w.Message})
	}
	return out, nil
}
