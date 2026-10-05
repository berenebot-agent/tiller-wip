package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gateway "github.com/tiller-router/tiller-router/clients/go"
)

func TestGeneratedPNG(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pngDataURL(), "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(strings.NewReader(string(raw)))
	if err != nil || img.Bounds().Dx() != 8 || img.Bounds().Dy() != 8 {
		t.Fatal("invalid PNG", err)
	}
}

func TestReplyVerificationAndToolContinuation(t *testing.T) {
	for _, body := range []string{"", "   ", "OK", "Acknowledged"} {
		result := &gateway.Result{Choices: []gateway.Choice{{Content: body}}}
		if usableReply(result) != (strings.TrimSpace(body) != "") {
			t.Fatal("reply validation")
		}
	}
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			io.WriteString(w, `{"data":[{"id":"test","supports_tools":1,"supports_structured_output":0}]}`)
			return
		}
		calls++
		var req struct {
			ToolChoice string           `json:"tool_choice"`
			Messages   []map[string]any `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("request decode")
		}
		if calls == 1 {
			if req.ToolChoice != "auto" {
				t.Error("not auto")
			}
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call","type":"function","function":{"name":"acknowledge","arguments":"{\"status\":\"OK\"}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			if req.ToolChoice != "none" || len(req.Messages) != 3 || req.Messages[2]["tool_call_id"] != "call" {
				t.Error("continuation invalid")
			}
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
		}
	}))
	defer s.Close()
	c, err := gateway.New(gateway.Config{Profile: gateway.Tiller, BaseURL: s.URL + "/v1", APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := gateway.Request{Model: "test", Messages: []gateway.Message{{"role": "user", "content": "OK"}}}
	if err = feature(context.Background(), c, req, "tools"); err != nil || calls != 2 {
		t.Fatal("tool flow", err)
	}
}

func TestEmptyChatAndStreamFail(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":" "},"finish_reason":"stop"}]}`)
	}))
	defer s.Close()
	c, err := gateway.New(gateway.Config{Profile: gateway.Tiller, BaseURL: s.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := gateway.Request{Model: "test", Messages: []gateway.Message{{"role": "user", "content": "OK"}}}
	if chat(context.Background(), c, req, "chat") == nil {
		t.Fatal("empty chat accepted")
	}
	if stream(context.Background(), c, req) == nil {
		t.Fatal("empty stream accepted")
	}
}
