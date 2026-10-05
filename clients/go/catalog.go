package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

func (c *Client) ListModels(ctx context.Context) ([]Model, error) { return c.listModels(ctx, false) }

// RefreshModels forces discovery. Failure retains the prior cache and timestamp.
func (c *Client) RefreshModels(ctx context.Context) ([]Model, error) { return c.listModels(ctx, true) }
func (c *Client) CatalogCachedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cachedAt
}

// CachedModels returns an isolated last-success snapshot without fetching,
// including after a refresh failure. A zero timestamp means no successful fetch.
func (c *Client) CachedModels() ([]Model, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneModels(c.catalog), c.cachedAt
}

func cloneModels(in []Model) []Model {
	out := make([]Model, len(in))
	for i, m := range in {
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &out[i])
	}
	return out
}

func (c *Client) listModels(ctx context.Context, force bool) ([]Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lifetime.Err() != nil {
		return nil, failure("cancelled")
	}
	if ctx.Err() != nil {
		return nil, transportError(ctx, ctx.Err())
	}
	if !force && !c.cachedAt.IsZero() && time.Since(c.cachedAt) < c.cfg.CacheTTL {
		return cloneModels(c.catalog), nil
	}
	ctx, cancel := c.requestContext(ctx)
	defer cancel()
	path := c.cfg.CatalogPath
	if path == "" {
		path = "models"
		if c.cfg.Profile == OpenRouter {
			path = "models/user"
		}
	}
	address := c.endpoint(path)
	seen := map[string]bool{}
	models := make([]Model, 0)
	var totalBytes int64
	for page := 0; ; page++ {
		if page >= c.cfg.MaxCatalogPages || seen[address] {
			return nil, failure("malformed_response")
		}
		seen[address] = true
		res, err := c.send(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		b, err := c.readBounded(ctx, res, c.cfg.MaxBodyBytes-totalBytes)
		if err != nil {
			return nil, err
		}
		totalBytes += int64(len(b))
		if totalBytes > c.cfg.MaxBodyBytes {
			return nil, failure("malformed_response")
		}
		var wire struct {
			Data  []json.RawMessage `json:"data"`
			Links struct {
				Next *string `json:"next"`
			} `json:"links"`
		}
		if json.Unmarshal(b, &wire) != nil || wire.Data == nil {
			return nil, failure("malformed_response")
		}
		for _, raw := range wire.Data {
			m := normalize(raw, c.cfg.Profile)
			if m.ID == "" {
				return nil, failure("malformed_response")
			}
			models = append(models, m)
		}
		if wire.Links.Next == nil || *wire.Links.Next == "" {
			break
		}
		current, _ := url.Parse(address)
		next, err := url.Parse(*wire.Links.Next)
		if err != nil {
			return nil, failure("malformed_response")
		}
		next = current.ResolveReference(next)
		if next.Scheme != c.base.Scheme || next.Host != c.base.Host || next.User != nil || next.Fragment != "" {
			return nil, failure("malformed_response")
		}
		address = next.String()
	}
	c.catalog = cloneModels(models)
	c.cachedAt = time.Now()
	return models, nil
}

func normalize(raw json.RawMessage, profile Profile) Model {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	m := Model{Raw: append(json.RawMessage(nil), raw...)}
	_ = json.Unmarshal(fields["id"], &m.ID)
	_ = json.Unmarshal(fields["name"], &m.Name)
	m.ContextLength = integer(fields["context_length"])
	m.MaxOutputTokens = integer(fields["max_output_tokens"])
	m.Vision = support(fields["supports_vision"])
	m.Tools = support(fields["supports_tools"])
	m.Reasoning = support(fields["supports_reasoning"])
	m.StructuredOutput = support(fields["supports_structured_output"])
	m.JSONObject = support(fields["supports_json_object"])
	_ = json.Unmarshal(fields["input_modalities"], &m.InputModalities)
	_ = json.Unmarshal(fields["output_modalities"], &m.OutputModalities)
	if profile == OpenRouter {
		var arch struct {
			Input  []string `json:"input_modalities"`
			Output []string `json:"output_modalities"`
		}
		_ = json.Unmarshal(fields["architecture"], &arch)
		if _, present := fields["input_modalities"]; !present {
			m.InputModalities = arch.Input
		}
		if _, present := fields["output_modalities"]; !present {
			m.OutputModalities = arch.Output
		}
		var top map[string]json.RawMessage
		_ = json.Unmarshal(fields["top_provider"], &top)
		if _, present := fields["max_output_tokens"]; !present {
			m.MaxOutputTokens = integer(top["max_completion_tokens"])
		}
		var params []string
		if json.Unmarshal(fields["supported_parameters"], &params) == nil && params != nil {
			if _, present := fields["supports_tools"]; !present {
				m.Tools = fromList(params, "tools")
			}
			if _, present := fields["supports_reasoning"]; !present {
				m.Reasoning = fromList(params, "reasoning")
			}
			if _, present := fields["supports_structured_output"]; !present {
				m.StructuredOutput = fromList(params, "structured_outputs")
			}
			if _, present := fields["supports_json_object"]; !present {
				m.JSONObject = fromList(params, "response_format")
			}
		}
		if _, present := fields["supports_vision"]; !present && m.InputModalities != nil {
			m.Vision = fromList(m.InputModalities, "image")
		}
	}
	var reasoning map[string]json.RawMessage
	_ = json.Unmarshal(fields["reasoning"], &reasoning)
	opts := Reasoning{Raw: append(json.RawMessage(nil), fields["reasoning"]...)}
	if v, ok := reasoning["supported_efforts"]; ok {
		opts.EffortsKnown = true
		opts.EffortsUnrestricted = string(v) == "null"
		if !opts.EffortsUnrestricted && json.Unmarshal(v, &opts.Efforts) != nil {
			opts.EffortsKnown = false
		}
	}
	opts.BudgetMin = integer(reasoning["min_budget_tokens"])
	opts.BudgetMax = integer(reasoning["max_budget_tokens"])
	if opts.BudgetMin == nil {
		opts.BudgetMin = integer(reasoning["budget_min"])
	}
	if opts.BudgetMax == nil {
		opts.BudgetMax = integer(reasoning["budget_max"])
	}
	opts.SupportsMaxTokens = support(reasoning["supports_max_tokens"])
	opts.Toggle = support(reasoning["supports_toggle"])
	if _, present := reasoning["supports_toggle"]; !present {
		opts.Toggle = support(reasoning["toggle"])
	}
	opts.Mandatory = boolean(reasoning["mandatory"])
	opts.DefaultEnabled = boolean(reasoning["default_enabled"])
	_ = json.Unmarshal(reasoning["default_effort"], &opts.DefaultEffort)
	_ = json.Unmarshal(reasoning["modes"], &opts.Modes)
	var options []map[string]json.RawMessage
	_ = json.Unmarshal(fields["reasoning_options"], &options)
	for _, o := range options {
		var kind string
		_ = json.Unmarshal(o["type"], &kind)
		switch kind {
		case "effort":
			if _, present := reasoning["supported_efforts"]; !present {
				if values, present := o["values"]; present {
					opts.EffortsUnrestricted = string(values) == "null"
					opts.EffortsKnown = opts.EffortsUnrestricted || json.Unmarshal(values, &opts.Efforts) == nil
				}
			}
		case "budget_tokens":
			if _, present := reasoning["supports_max_tokens"]; !present {
				opts.SupportsMaxTokens = Supported
			}
			if opts.BudgetMin == nil {
				opts.BudgetMin = integer(o["min"])
			}
			if opts.BudgetMax == nil {
				opts.BudgetMax = integer(o["max"])
			}
		case "enabled", "toggle":
			if _, present := reasoning["supports_toggle"]; !present {
				if _, present := reasoning["toggle"]; !present {
					opts.Toggle = Supported
				}
			}
		}
	}
	m.ReasoningOptions = opts
	return m
}
func integer(raw json.RawMessage) *int {
	var n int
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &n) != nil || n < 0 {
		return nil
	}
	return &n
}
func boolean(raw json.RawMessage) *bool {
	var b bool
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &b) != nil {
		return nil
	}
	return &b
}
func support(raw json.RawMessage) Support {
	if b := boolean(raw); b != nil {
		if *b {
			return Supported
		}
		return Unsupported
	}
	if n := integer(raw); n != nil {
		if *n == 1 {
			return Supported
		}
		if *n == 0 {
			return Unsupported
		}
	}
	return Unknown
}
func fromList(values []string, item string) Support {
	if containsAll(values, []string{item}) {
		return Supported
	}
	return Unsupported
}
