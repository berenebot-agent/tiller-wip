package gateway

import (
	"encoding/json"
	"net/http"
	"time"
)

type Profile string

const (
	Tiller     Profile = "tiller"
	OpenRouter Profile = "openrouter"
)

// Config describes one immutable connection and principal. Zero limits select
// bounded defaults; Timeout covers the entire request, including stream reads.
// CacheTTL defaults to five minutes. MaxBodyBytes bounds encoded requests,
// aggregate catalog pages, non-stream replies, and stream wire data.
type Config struct {
	Profile          Profile
	BaseURL          string
	APIKey           string `json:"-"`
	HTTPClient       *http.Client
	Timeout          time.Duration
	CacheTTL         time.Duration
	MaxBodyBytes     int64
	MaxLineBytes     int
	MaxEventBytes    int
	MaxArgumentBytes int
	MaxCatalogPages  int
	CatalogPath      string
}

type Support int

const (
	Unknown Support = iota
	Unsupported
	Supported
)

type Reasoning struct {
	EffortsKnown        bool
	EffortsUnrestricted bool
	Efforts             []string
	BudgetMin           *int
	BudgetMax           *int
	SupportsMaxTokens   Support
	Toggle              Support
	Modes               []string
	Mandatory           *bool
	DefaultEnabled      *bool
	DefaultEffort       string
	Raw                 json.RawMessage
}

type Model struct {
	ID               string
	Name             string
	ContextLength    *int
	MaxOutputTokens  *int
	InputModalities  []string
	OutputModalities []string
	Vision           Support
	Tools            Support
	Reasoning        Support
	JSONObject       Support
	StructuredOutput Support
	ReasoningOptions Reasoning
	Raw              json.RawMessage
}

type Requirements struct {
	Vision           bool
	Tools            bool
	Reasoning        bool
	StructuredOutput bool
	JSONObject       bool
	MinOutput        int
	MinContext       int
	InputModalities  []string
	OutputModalities []string
}

func FilterModels(models []Model, r Requirements) []Model {
	out := make([]Model, 0)
	for _, m := range models {
		if r.Vision && m.Vision != Supported || r.Tools && m.Tools != Supported || r.Reasoning && m.Reasoning != Supported || r.StructuredOutput && m.StructuredOutput != Supported {
			continue
		}
		if r.JSONObject && m.JSONObject != Supported || r.MinOutput > 0 && (m.MaxOutputTokens == nil || *m.MaxOutputTokens < r.MinOutput) {
			continue
		}
		if r.MinContext > 0 && (m.ContextLength == nil || *m.ContextLength < r.MinContext) {
			continue
		}
		if !containsAll(m.InputModalities, r.InputModalities) || !containsAll(m.OutputModalities, r.OutputModalities) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Message preserves endpoint-native fields for assistant replay and tool results.
// The SDK never executes tools or validates user-provided JSON schemas.
type Message map[string]any

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type Request struct {
	Model          string
	Messages       []Message
	Tools          []Tool
	ToolChoice     any
	ResponseFormat any
	MaxTokens      *int
	Temperature    *float64
	TopP           *float64
	Reasoning      any
	Extra          map[string]any
}

type ToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Usage keeps unreported counts nil and preserves provider accounting in Raw.
// CacheReadTokens prefers prompt details, then input details, then native counts.
type Usage struct {
	CacheReadTokens  *int            `json:"-"`
	CacheWriteTokens *int            `json:"-"`
	PromptTokens     *int            `json:"prompt_tokens"`
	CompletionTokens *int            `json:"completion_tokens"`
	TotalTokens      *int            `json:"total_tokens"`
	Raw              json.RawMessage `json:"-"`
}

type Choice struct {
	Index        int
	Message      Message
	Content      any
	ToolCalls    []ToolCall
	Reasoning    any
	FinishReason string
	Native       json.RawMessage
}

type Result struct {
	ID        string
	Model     string
	Choices   []Choice
	Usage     *Usage
	RequestID string
	Raw       json.RawMessage
}

type EventKind string

const (
	FrameEvent      EventKind = "frame"
	TextEvent       EventKind = "text"
	ReasoningEvent  EventKind = "reasoning"
	ToolEvent       EventKind = "tool"
	FinishEvent     EventKind = "finish"
	UsageEvent      EventKind = "usage"
	CompletionEvent EventKind = "completion"
)

type Event struct {
	Kind         EventKind
	ChoiceIndex  int
	Text         string
	Reasoning    json.RawMessage
	Tool         *ToolCall
	FinishReason string
	Usage        *Usage
	Raw          json.RawMessage
}

type Error struct {
	Status     int
	Category   string
	Code       string
	Type       string
	RequestID  string
	RetryAfter string
}

func (e *Error) Error() string          { return "gateway request failed (" + e.Category + ")" }
func (c Config) String() string         { return "gateway.Config" }
func (c Config) GoString() string       { return c.String() }
func (r Request) String() string        { return "gateway.Request" }
func (r Request) GoString() string      { return r.String() }
func (r Result) String() string         { return "gateway.Result" }
func (r Result) GoString() string       { return r.String() }
func (m Message) String() string        { return "gateway.Message" }
func (m Message) GoString() string      { return m.String() }
func (e Event) String() string          { return "gateway.Event(" + string(e.Kind) + ")" }
func (e Event) GoString() string        { return e.String() }
func (c Choice) String() string         { return "gateway.Choice" }
func (c Choice) GoString() string       { return c.String() }
func (t ToolCall) String() string       { return "gateway.ToolCall" }
func (t ToolCall) GoString() string     { return t.String() }
func (t Tool) String() string           { return "gateway.Tool" }
func (t Tool) GoString() string         { return t.String() }
func (t ToolFunction) String() string   { return "gateway.ToolFunction" }
func (t ToolFunction) GoString() string { return t.String() }
func (u Usage) String() string          { return "gateway.Usage" }
func (u Usage) GoString() string        { return u.String() }
func (e *Error) String() string         { return e.Error() }
func (e *Error) GoString() string       { return e.Error() }
