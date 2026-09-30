package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	gateway "github.com/tiller-router/tiller-router/clients/go"
)

type connection struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

func main() {
	config := flag.String("config", "", "protected JSON connection file for live acceptance")
	mode := flag.String("mode", "basic", "basic, catalog, stream, tools, image, structured, or live")
	features := flag.String("features", "", "optional comma-separated tools,structured,image live checks")
	flag.Parse()
	if err := run(*config, *mode, *features); err != nil {
		if e, ok := err.(*gateway.Error); ok {
			fmt.Printf("status=failed category=%s http_status=%d\n", e.Category, e.Status)
		} else {
			fmt.Println("status=failed category=invalid_configuration")
		}
		os.Exit(1)
	}
}

func run(path, mode, features string) error {
	conn := connection{BaseURL: os.Getenv("GATEWAY_BASE_URL"), APIKey: os.Getenv("GATEWAY_API_KEY"), Model: os.Getenv("GATEWAY_MODEL")}
	if path != "" {
		f, e := os.Open(path)
		if e != nil {
			return &gateway.Error{Category: "invalid_request"}
		}
		defer f.Close()
		stat, e := f.Stat()
		if e != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 || stat.Size() > 65536 {
			return &gateway.Error{Category: "invalid_request"}
		}
		conn = connection{}
		if json.NewDecoder(io.LimitReader(f, 65536)).Decode(&conn) != nil {
			return &gateway.Error{Category: "invalid_request"}
		}
	}
	if mode == "live" && path == "" {
		return &gateway.Error{Category: "invalid_request"}
	}
	if mode == "live" && conn.Model != "virtual/free" {
		return &gateway.Error{Category: "invalid_request"}
	}
	if conn.Model == "" {
		return &gateway.Error{Category: "invalid_request"}
	}
	profile := gateway.Tiller
	if mode != "live" && os.Getenv("GATEWAY_PROFILE") == "openrouter" {
		profile = gateway.OpenRouter
	}
	c, e := gateway.New(gateway.Config{Profile: profile, BaseURL: conn.BaseURL, APIKey: conn.APIKey, Timeout: 90 * time.Second})
	if e != nil {
		return e
	}
	defer c.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	tokens := 64
	req := gateway.Request{Model: conn.Model, Messages: []gateway.Message{{"role": "user", "content": "Reply with exactly OK"}}, MaxTokens: &tokens}
	if mode == "catalog" || mode == "live" {
		models, e := c.ListModels(ctx)
		if e != nil {
			return e
		}
		fmt.Printf("catalog status=ok count=%d\n", len(models))
		if mode == "catalog" {
			return nil
		}
		found := false
		for _, m := range models {
			if m.ID == conn.Model {
				fmt.Printf("capabilities tools=%d image=%d structured=%d json_object=%d reasoning=%d\n", m.Tools, m.Vision, m.StructuredOutput, m.JSONObject, m.Reasoning)
				found = true
			}
		}
		if !found {
			return &gateway.Error{Category: "model_unavailable"}
		}
	}
	switch mode {
	case "basic":
		return chat(ctx, c, req, "chat")
	case "stream":
		return stream(ctx, c, req)
	case "tools", "image", "structured", "json_object":
		return feature(ctx, c, req, mode)
	case "live":
		if e = chat(ctx, c, req, "chat"); e != nil {
			return e
		}
		if e = stream(ctx, c, req); e != nil {
			return e
		}
		if e = feature(ctx, c, req, "json_object"); e != nil {
			return e
		}
		if features != "" {
			for _, f := range strings.Split(features, ",") {
				if e = feature(ctx, c, req, f); e != nil {
					return e
				}
			}
		}
		return nil
	default:
		return &gateway.Error{Category: "invalid_request"}
	}
}

func chat(ctx context.Context, c *gateway.Client, req gateway.Request, label string) error {
	result, e := c.Chat(ctx, req)
	if e != nil {
		return e
	}
	if !usableReply(result) {
		return &gateway.Error{Category: "malformed_response"}
	}
	fmt.Printf("%s status=ok choices=%d", label, len(result.Choices))
	printUsage(result.Usage)
	return nil
}

func printUsage(u *gateway.Usage) {
	if u == nil {
		fmt.Println(" usage=unknown")
		return
	}
	fmt.Printf(" prompt_tokens=%s completion_tokens=%s total_tokens=%s cache_read_tokens=%s cache_write_tokens=%s\n", count(u.PromptTokens), count(u.CompletionTokens), count(u.TotalTokens), count(u.CacheReadTokens), count(u.CacheWriteTokens))
}
func count(n *int) string {
	if n == nil {
		return "unknown"
	}
	return fmt.Sprint(*n)
}

func stream(ctx context.Context, c *gateway.Client, req gateway.Request) error {
	s, e := c.Stream(ctx, req)
	if e != nil {
		return e
	}
	defer s.Close()
	events := 0
	done := false
	nonempty := false
	var usage *gateway.Usage
	for {
		ev, e := s.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		events++
		if ev.Kind == gateway.TextEvent && strings.TrimSpace(ev.Text) != "" {
			nonempty = true
		}
		if ev.Kind == gateway.CompletionEvent {
			done = true
		}
		if ev.Kind == gateway.UsageEvent {
			usage = ev.Usage
		}
	}
	if !done || !nonempty {
		return &gateway.Error{Category: "stream"}
	}
	fmt.Printf("stream status=ok events=%d", events)
	printUsage(usage)
	return nil
}

func feature(ctx context.Context, c *gateway.Client, req gateway.Request, feature string) error {
	requirements := gateway.Requirements{}
	switch feature {
	case "tools":
		requirements.Tools = true
	case "structured":
		requirements.StructuredOutput = true
	case "json_object":
		// JSON object is an explicit baseline probe, not a strict-schema capability claim.
	case "image":
		requirements.Vision = true
	default:
		return &gateway.Error{Category: "invalid_request"}
	}
	models, e := c.ListModels(ctx)
	if e != nil {
		return e
	}
	supported := false
	for _, m := range gateway.FilterModels(models, requirements) {
		if m.ID == req.Model {
			supported = true
		}
	}
	if !supported {
		fmt.Printf("%s status=skipped capability=not_known_supported\n", feature)
		return nil
	}
	tokens := 256
	req.MaxTokens = &tokens
	switch feature {
	case "tools":
		req.Tools = []gateway.Tool{{Type: "function", Function: gateway.ToolFunction{Name: "acknowledge", Description: "Acknowledge the request", Parameters: map[string]any{"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"OK"}}}, "required": []string{"status"}, "additionalProperties": false}}}}
		req.ToolChoice = "auto"
		req.Messages = []gateway.Message{{"role": "user", "content": "Call acknowledge with status OK. After the tool result, reply OK."}}
	case "json_object":
		req.Messages = []gateway.Message{{"role": "user", "content": "Return a JSON object with exactly one field status equal to OK."}}
		req.ResponseFormat = map[string]any{"type": "json_object"}
	case "structured":
		req.ResponseFormat = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "acknowledgement", "strict": true, "schema": map[string]any{"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"OK"}}}, "required": []string{"status"}, "additionalProperties": false}}}
	case "image":
		req.Messages = []gateway.Message{{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Reply with exactly OK"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": pngDataURL()}}}}}
	}
	result, e := c.Chat(ctx, req)
	if e != nil {
		return e
	}
	if feature == "tools" {
		found := false
		for _, choice := range result.Choices {
			for _, tool := range choice.ToolCalls {
				if tool.Function.Name == "acknowledge" && validStatus(tool.Function.Arguments) {
					found = true
				}
			}
		}
		if !found {
			return &gateway.Error{Category: "malformed_response"}
		}
		choice := result.Choices[0]
		if len(choice.ToolCalls) == 0 {
			return &gateway.Error{Category: "malformed_response"}
		}
		req.Messages = append(req.Messages, choice.Message)
		for _, tool := range choice.ToolCalls {
			if tool.Function.Name != "acknowledge" || !validStatus(tool.Function.Arguments) {
				return &gateway.Error{Category: "malformed_response"}
			}
			// Return a fixed acknowledgement; never dispatch or execute model-supplied tools.
			req.Messages = append(req.Messages, gateway.Message{"role": "tool", "tool_call_id": tool.ID, "content": `{"status":"OK"}`})
		}
		req.ToolChoice = "none"
		if e := chat(ctx, c, req, "tools_continuation"); e != nil {
			return e
		}
	}
	if feature == "structured" || feature == "json_object" {
		found := false
		for _, choice := range result.Choices {
			if content, ok := choice.Content.(string); ok && validStatus(content) {
				found = true
			}
		}
		if !found {
			return &gateway.Error{Category: "malformed_response"}
		}
	}
	if feature == "image" && !usableReply(result) {
		return &gateway.Error{Category: "malformed_response"}
	}
	fmt.Printf("%s status=ok choices=%d", feature, len(result.Choices))
	printUsage(result.Usage)
	return nil
}

func usableReply(result *gateway.Result) bool {
	for _, choice := range result.Choices {
		if content, ok := choice.Content.(string); ok && strings.TrimSpace(content) != "" {
			return true
		}
	}
	return false
}

func pngDataURL() string {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func validStatus(text string) bool {
	var v map[string]any
	return json.Unmarshal([]byte(text), &v) == nil && len(v) == 1 && v["status"] == "OK"
}
