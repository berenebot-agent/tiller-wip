package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers"
)

type unsupportedFeature struct{ feature string }

func (u unsupportedFeature) Error() string {
	return fmt.Sprintf("%s is only supported through a native Responses provider", u.feature)
}

func translateRequest(body []byte, from, to providers.Protocol, model string) ([]byte, error) {
	if from == to {
		return body, nil
	}
	var source map[string]any
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, err
	}
	if from == providers.ProtocolResponses {
		for _, key := range []string{"conversation", "previous_response_id", "store", "background", "files"} {
			if value, ok := source[key]; ok && value != nil && value != false && value != "" {
				return nil, unsupportedFeature{key}
			}
		}
		if tools, ok := source["tools"].([]any); ok {
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				if tool["type"] != "function" {
					return nil, unsupportedFeature{"provider-hosted tools"}
				}
			}
		}
	}
	chat, err := requestToChat(source, from)
	if err != nil {
		return nil, err
	}
	chat["model"] = model
	if to == providers.ProtocolChat {
		return json.Marshal(chat)
	}
	if to == providers.ProtocolMessages {
		return json.Marshal(chatToMessagesRequest(chat))
	}
	if to == providers.ProtocolResponses {
		translated, err := chatToResponsesRequest(chat)
		if err != nil {
			return nil, err
		}
		return json.Marshal(translated)
	}
	return nil, errors.New("unsupported protocol translation")
}

func requestToChat(source map[string]any, from providers.Protocol) (map[string]any, error) {
	if from == providers.ProtocolChat {
		return source, nil
	}
	chat := map[string]any{}
	for _, key := range []string{"temperature", "top_p", "stream", "stop", "metadata"} {
		if value, ok := source[key]; ok {
			chat[key] = value
		}
	}
	if max, ok := source["max_tokens"]; ok {
		chat["max_tokens"] = max
	}
	if max, ok := source["max_output_tokens"]; ok {
		chat["max_completion_tokens"] = max
	}
	var messages []any
	if from == providers.ProtocolMessages {
		if system, ok := source["system"]; ok {
			messages = append(messages, map[string]any{"role": "system", "content": system})
		}
		for _, raw := range asSlice(source["messages"]) {
			message, _ := raw.(map[string]any)
			role, _ := message["role"].(string)
			content := message["content"]
			blocks := asSlice(content)
			if blocks == nil {
				messages = append(messages, map[string]any{"role": role, "content": content})
				continue
			}
			textBlocks := []any{}
			toolCalls := []any{}
			for _, item := range blocks {
				block, _ := item.(map[string]any)
				switch block["type"] {
				case "text":
					textBlocks = append(textBlocks, map[string]any{"type": "text", "text": block["text"]})
				case "image":
					textBlocks = append(textBlocks, anthropicImageToOpenAI(block))
				case "tool_use":
					toolCalls = append(toolCalls, map[string]any{"id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": jsonString(block["input"])}})
				case "tool_result":
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": block["tool_use_id"], "content": block["content"]})
				}
			}
			if len(textBlocks) > 0 || len(toolCalls) > 0 {
				m := map[string]any{"role": role, "content": textBlocks}
				if len(toolCalls) > 0 {
					m["tool_calls"] = toolCalls
				}
				messages = append(messages, m)
			}
		}
		if tools, ok := source["tools"]; ok {
			converted := []any{}
			for _, raw := range asSlice(tools) {
				tool, _ := raw.(map[string]any)
				converted = append(converted, map[string]any{"type": "function", "function": map[string]any{"name": tool["name"], "description": tool["description"], "parameters": tool["input_schema"]}})
			}
			chat["tools"] = converted
		}
		if choice, ok := source["tool_choice"].(map[string]any); ok {
			switch choice["type"] {
			case "auto":
				chat["tool_choice"] = "auto"
			case "any":
				chat["tool_choice"] = "required"
			case "tool":
				chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": choice["name"]}}
			}
		}
	} else {
		if instructions, ok := source["instructions"].(string); ok && instructions != "" {
			messages = append(messages, map[string]any{"role": "system", "content": instructions})
		}
		input := source["input"]
		if text, ok := input.(string); ok {
			messages = append(messages, map[string]any{"role": "user", "content": text})
		} else {
			for _, raw := range asSlice(input) {
				item, _ := raw.(map[string]any)
				kind, _ := item["type"].(string)
				switch kind {
				case "message", "":
					messages = append(messages, map[string]any{"role": item["role"], "content": responsesContentToChat(item["content"])})
				case "function_call":
					messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}}}})
				case "function_call_output":
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": item["call_id"], "content": item["output"]})
				}
			}
		}
		if tools, ok := source["tools"]; ok {
			converted := []any{}
			for _, raw := range asSlice(tools) {
				tool, _ := raw.(map[string]any)
				converted = append(converted, map[string]any{"type": "function", "function": map[string]any{"name": tool["name"], "description": tool["description"], "parameters": tool["parameters"], "strict": tool["strict"]}})
			}
			chat["tools"] = converted
		}
		if choice, ok := source["tool_choice"]; ok {
			chat["tool_choice"] = choice
		}
	}
	chat["messages"] = messages
	return chat, nil
}

func chatToMessagesRequest(chat map[string]any) map[string]any {
	out := map[string]any{"model": chat["model"], "max_tokens": 4096}
	for _, key := range []string{"temperature", "top_p", "stream", "stop_sequences", "metadata"} {
		if v, ok := chat[key]; ok {
			out[key] = v
		}
	}
	if max, ok := chat["max_tokens"]; ok {
		out["max_tokens"] = max
	}
	if max, ok := chat["max_completion_tokens"]; ok {
		out["max_tokens"] = max
	}
	messages := []any{}
	systems := []any{}
	for _, raw := range asSlice(chat["messages"]) {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			systems = append(systems, message["content"])
			continue
		}
		if role == "tool" {
			messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": message["tool_call_id"], "content": message["content"]}}})
			continue
		}
		content := chatContentToAnthropic(message["content"])
		if calls := asSlice(message["tool_calls"]); calls != nil {
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				content = append(content, map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": jsonValue(fn["arguments"])})
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	if len(systems) > 0 {
		out["system"] = systems
	}
	out["messages"] = messages
	if tools := asSlice(chat["tools"]); tools != nil {
		converted := []any{}
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			converted = append(converted, map[string]any{"name": fn["name"], "description": fn["description"], "input_schema": fn["parameters"]})
		}
		out["tools"] = converted
	}
	if choice, ok := chat["tool_choice"]; ok {
		switch v := choice.(type) {
		case string:
			if v == "required" {
				out["tool_choice"] = map[string]any{"type": "any"}
			} else {
				out["tool_choice"] = map[string]any{"type": v}
			}
		case map[string]any:
			fn, _ := v["function"].(map[string]any)
			out["tool_choice"] = map[string]any{"type": "tool", "name": fn["name"]}
		}
	}
	return out
}

func chatToResponsesRequest(chat map[string]any) (map[string]any, error) {
	out := map[string]any{"model": chat["model"]}

	var input []any

	if messages := asSlice(chat["messages"]); messages != nil {
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				return nil, unsupportedFeature{"message items"}
			}
			role, _ := msg["role"].(string)

			switch role {
			case "system", "developer":
				item, err := messageToResponsesItem(msg, role)
				if err != nil {
					return nil, err
				}
				input = append(input, item)

			case "user":
				item, err := userMessageToResponsesItem(msg)
				if err != nil {
					return nil, err
				}
				input = append(input, item)

			case "assistant":
				if calls := asSlice(msg["tool_calls"]); calls != nil && len(calls) > 0 {
					// Assistant tool calls remain separate typed Responses items.
					if hasChatMessageContent(msg["content"]) {
						item, err := assistantMessageToResponsesItem(msg)
						if err != nil {
							return nil, err
						}
						input = append(input, item)
					}
					for _, callRaw := range calls {
						call, ok := callRaw.(map[string]any)
						fn, fnOK := call["function"].(map[string]any)
						if !ok || !fnOK || call["id"] == nil || fn["name"] == nil || fn["arguments"] == nil {
							return nil, unsupportedFeature{"tool calls"}
						}
						id, _ := call["id"].(string)
						input = append(input, map[string]any{
							"type":      "function_call",
							"call_id":   id,
							"name":      fn["name"],
							"arguments": fn["arguments"],
						})
					}
				} else {
					item, err := assistantMessageToResponsesItem(msg)
					if err != nil {
						return nil, err
					}
					input = append(input, item)
				}

			case "tool":
				id, idOK := msg["tool_call_id"].(string)
				content, contentOK := msg["content"].(string)
				if !idOK || id == "" || !contentOK {
					return nil, unsupportedFeature{"tool messages"}
				}
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": id,
					"output":  content,
				})

			default:
				return nil, unsupportedFeature{"message role " + role}
			}
		}
	}

	out["input"] = input

	// Handle tool_choice before the main key loop. The Responses relay
	// accepts only the string "auto" for tool_choice; every other Chat
	// value must be either represented safely or rejected rather than
	// silently weakened. "none" means "do not call tools" — the only
	// safe relay representation is to omit tools entirely. "required"
	// and named function choices have no equivalent and are rejected.
	var suppressTools bool
	if v, ok := chat["tool_choice"]; ok {
		choice, err := convertToolChoice(v)
		if err != nil {
			return nil, err
		}
		if choice == suppressToolChoice {
			suppressTools = true
		} else if choice != "" {
			out["tool_choice"] = choice
		}
	}
	for _, key := range []string{"temperature", "top_p", "stream", "metadata"} {
		if v, ok := chat[key]; ok {
			out[key] = v
		}
	}
	if max, ok := chat["max_completion_tokens"]; ok {
		out["max_output_tokens"] = max
	} else if max, ok := chat["max_tokens"]; ok {
		out["max_output_tokens"] = max
	}
	if !suppressTools {
		if tools := asSlice(chat["tools"]); tools != nil {
			converted := []any{}
			for _, raw := range tools {
				tool, ok := raw.(map[string]any)
				if !ok || tool["type"] != "function" {
					return nil, unsupportedFeature{"non-function tools"}
				}
				fn, fnOK := tool["function"].(map[string]any)
				if !fnOK {
					return nil, unsupportedFeature{"non-function tools"}
				}
				converted = append(converted, map[string]any{"type": "function", "name": fn["name"], "description": fn["description"], "parameters": fn["parameters"], "strict": fn["strict"]})
			}
			out["tools"] = converted
		}
	}
	return out, nil
}

func translateResponse(w http.ResponseWriter, r io.Reader, incoming, target providers.Protocol, route resolvedRoute, usage *usageCapture) error {
	reader := bufio.NewReader(r)
	prefix, err := reader.Peek(1)
	if err != nil {
		return err
	}
	if prefix[0] == '{' || prefix[0] == '[' {
		body, err := io.ReadAll(io.LimitReader(reader, 64<<20))
		if err != nil {
			return err
		}
		extractUsage(body, usage)
		translated, err := translateNonstreamResponse(body, incoming, target, route.RequestedModel)
		if err != nil {
			return err
		}
		_, err = w.Write(translated)
		return err
	}
	return translateSSE(w, reader, incoming, target, route.RequestedModel, usage)
}

func translateNonstreamResponse(body []byte, incoming, target providers.Protocol, model string) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, err
	}
	chat := responseToChat(source, target, model)
	switch incoming {
	case providers.ProtocolChat:
		return json.Marshal(chat)
	case providers.ProtocolMessages:
		return json.Marshal(chatResponseToMessages(chat, model))
	case providers.ProtocolResponses:
		return json.Marshal(chatResponseToResponses(chat, model))
	}
	return nil, errors.New("unknown client protocol")
}

func responseToChat(source map[string]any, target providers.Protocol, model string) map[string]any {
	if target == providers.ProtocolChat {
		source["model"] = model
		return source
	}
	message := map[string]any{"role": "assistant", "content": ""}
	finish := "stop"
	usage := source["usage"]
	if target == providers.ProtocolMessages {
		content := []any{}
		for _, raw := range asSlice(source["content"]) {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "text", "thinking":
				if text, _ := block["text"].(string); text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
			case "tool_use":
				calls, _ := message["tool_calls"].([]any)
				calls = append(calls, map[string]any{"id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": jsonString(block["input"])}})
				message["tool_calls"] = calls
			}
		}
		message["content"] = content
		if source["stop_reason"] == "tool_use" {
			finish = "tool_calls"
		}
		if u, ok := usage.(map[string]any); ok {
			usage = map[string]any{"prompt_tokens": u["input_tokens"], "completion_tokens": u["output_tokens"], "total_tokens": number(u["input_tokens"]) + number(u["output_tokens"])}
		}
	} else {
		content := []any{}
		calls := []any{}
		for _, raw := range asSlice(source["output"]) {
			item, _ := raw.(map[string]any)
			switch item["type"] {
			case "message":
				for _, partRaw := range asSlice(item["content"]) {
					part, _ := partRaw.(map[string]any)
					if part["type"] == "output_text" {
						content = append(content, map[string]any{"type": "text", "text": part["text"]})
					}
				}
			case "function_call":
				calls = append(calls, map[string]any{"id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}})
			}
		}
		message["content"] = content
		if len(calls) > 0 {
			message["tool_calls"] = calls
			finish = "tool_calls"
		}
		if u, ok := usage.(map[string]any); ok {
			usage = map[string]any{"prompt_tokens": u["input_tokens"], "completion_tokens": u["output_tokens"], "total_tokens": u["total_tokens"]}
		}
	}
	return map[string]any{"id": source["id"], "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage}
}

func chatResponseToMessages(chat map[string]any, model string) map[string]any {
	choices := asSlice(chat["choices"])
	choice := map[string]any{}
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	message, _ := choice["message"].(map[string]any)
	content := chatContentToAnthropic(message["content"])
	for _, raw := range asSlice(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		content = append(content, map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": jsonValue(fn["arguments"])})
	}
	stop := "end_turn"
	if choice["finish_reason"] == "tool_calls" {
		stop = "tool_use"
	}
	usage := map[string]any{}
	if u, ok := chat["usage"].(map[string]any); ok {
		usage = map[string]any{"input_tokens": u["prompt_tokens"], "output_tokens": u["completion_tokens"]}
	}
	return map[string]any{"id": chat["id"], "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": usage}
}

func chatResponseToResponses(chat map[string]any, model string) map[string]any {
	choices := asSlice(chat["choices"])
	choice := map[string]any{}
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	message, _ := choice["message"].(map[string]any)
	output := []any{}
	parts := []any{}
	for _, part := range chatContentToAnthropic(message["content"]) {
		block, _ := part.(map[string]any)
		if block["type"] == "text" {
			parts = append(parts, map[string]any{"type": "output_text", "text": block["text"], "annotations": []any{}})
		}
	}
	output = append(output, map[string]any{"id": "msg_" + fmt.Sprint(chat["id"]), "type": "message", "role": "assistant", "status": "completed", "content": parts})
	for _, raw := range asSlice(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		output = append(output, map[string]any{"type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"], "status": "completed"})
	}
	usage := map[string]any{}
	if u, ok := chat["usage"].(map[string]any); ok {
		usage = map[string]any{"input_tokens": u["prompt_tokens"], "output_tokens": u["completion_tokens"], "total_tokens": u["total_tokens"]}
	}
	return map[string]any{"id": chat["id"], "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": output, "usage": usage}
}

func translateSSE(w http.ResponseWriter, reader *bufio.Reader, incoming, target providers.Protocol, model string, usage *usageCapture) error {
	flusher, _ := w.(http.Flusher)
	state := &streamState{id: "tiller_" + fmt.Sprint(time.Now().UnixNano()), model: model}
	for {
		event, data, err := readSSEEvent(reader)
		if string(data) == "[DONE]" {
			writeStreamDone(w, incoming, state)
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
		if len(data) > 0 && string(data) != "[DONE]" {
			var payload map[string]any
			if json.Unmarshal(data, &payload) == nil {
				captureStreamUsage(payload, target, usage)
				deltas, done := canonicalDeltas(event, payload, target, state)
				for _, delta := range deltas {
					writeTranslatedEvent(w, incoming, state, delta)
					if flusher != nil {
						flusher.Flush()
					}
				}
				if done {
					writeStreamDone(w, incoming, state)
					if flusher != nil {
						flusher.Flush()
					}
					return nil
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				writeStreamDone(w, incoming, state)
				return nil
			}
			return err
		}
	}
}

type streamState struct {
	id, model                     string
	started, contentStarted, done bool
	outputIndex                   int
	accumulated                   strings.Builder
}
type canonicalDelta struct {
	Kind, Text, CallID, Name, Arguments, Finish string
	Usage                                       any
}

func canonicalDeltas(event string, payload map[string]any, target providers.Protocol, state *streamState) ([]canonicalDelta, bool) {
	out := []canonicalDelta{}
	if target == providers.ProtocolChat {
		if id, ok := payload["id"].(string); ok {
			state.id = id
		}
		for _, raw := range asSlice(payload["choices"]) {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok && text != "" {
				out = append(out, canonicalDelta{Kind: "text", Text: text})
			}
			for _, callRaw := range asSlice(delta["tool_calls"]) {
				call, _ := callRaw.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				out = append(out, canonicalDelta{Kind: "tool", CallID: fmt.Sprint(call["id"]), Name: fmt.Sprint(fn["name"]), Arguments: fmt.Sprint(fn["arguments"])})
			}
			if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
				out = append(out, canonicalDelta{Kind: "finish", Finish: finish})
			}
		}
		if usage := payload["usage"]; usage != nil {
			out = append(out, canonicalDelta{Kind: "usage", Usage: usage})
		}
		return out, false
	}
	if target == providers.ProtocolMessages {
		switch event {
		case "message_start":
			if message, ok := payload["message"].(map[string]any); ok {
				if id, ok := message["id"].(string); ok {
					state.id = id
				}
			}
		case "content_block_start":
			if block, ok := payload["content_block"].(map[string]any); ok && block["type"] == "tool_use" {
				out = append(out, canonicalDelta{Kind: "tool", CallID: fmt.Sprint(block["id"]), Name: fmt.Sprint(block["name"])})
			}
		case "content_block_delta":
			if delta, ok := payload["delta"].(map[string]any); ok {
				if text, ok := delta["text"].(string); ok {
					out = append(out, canonicalDelta{Kind: "text", Text: text})
				}
				if partial, ok := delta["partial_json"].(string); ok {
					out = append(out, canonicalDelta{Kind: "tool", Arguments: partial})
				}
			}
		case "message_delta":
			if delta, ok := payload["delta"].(map[string]any); ok {
				out = append(out, canonicalDelta{Kind: "finish", Finish: fmt.Sprint(delta["stop_reason"])})
			}
		case "message_stop":
			return out, true
		}
		return out, false
	}
	// Responses events.
	switch event {
	case "response.created":
		if response, ok := payload["response"].(map[string]any); ok {
			if id, ok := response["id"].(string); ok {
				state.id = id
			}
		}
	case "response.output_text.delta":
		out = append(out, canonicalDelta{Kind: "text", Text: fmt.Sprint(payload["delta"])})
	case "response.function_call_arguments.delta":
		out = append(out, canonicalDelta{Kind: "tool", CallID: fmt.Sprint(payload["call_id"]), Arguments: fmt.Sprint(payload["delta"])})
	case "response.output_item.added":
		if item, ok := payload["item"].(map[string]any); ok && item["type"] == "function_call" {
			out = append(out, canonicalDelta{Kind: "tool", CallID: fmt.Sprint(item["call_id"]), Name: fmt.Sprint(item["name"])})
		}
	case "response.completed":
		return out, true
	}
	return out, false
}

func writeTranslatedEvent(w io.Writer, incoming providers.Protocol, state *streamState, delta canonicalDelta) {
	if incoming == providers.ProtocolChat {
		payload := map[string]any{"id": state.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": state.model}
		choice := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}
		d := choice["delta"].(map[string]any)
		if !state.started {
			d["role"] = "assistant"
			state.started = true
		}
		switch delta.Kind {
		case "text":
			d["content"] = delta.Text
		case "tool":
			d["tool_calls"] = []any{map[string]any{"index": 0, "id": emptyNil(delta.CallID), "type": "function", "function": map[string]any{"name": emptyNil(delta.Name), "arguments": delta.Arguments}}}
		case "finish":
			choice["finish_reason"] = normalizeFinish(delta.Finish)
		}
		payload["choices"] = []any{choice}
		if delta.Kind == "usage" {
			payload["choices"] = []any{}
			payload["usage"] = delta.Usage
		}
		writeSSE(w, "", payload)
		return
	}
	if incoming == providers.ProtocolMessages {
		if !state.started {
			writeSSE(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": state.id, "type": "message", "role": "assistant", "model": state.model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
			state.started = true
		}
		switch delta.Kind {
		case "text":
			if !state.contentStarted {
				writeSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
				state.contentStarted = true
			}
			writeSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": delta.Text}})
		case "finish":
			if state.contentStarted {
				writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
			}
			writeSSE(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": normalizeAnthropicFinish(delta.Finish), "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
		}
		return
	}
	if !state.started {
		response := map[string]any{"id": state.id, "object": "response", "created_at": time.Now().Unix(), "status": "in_progress", "model": state.model, "output": []any{}}
		writeSSE(w, "response.created", map[string]any{"type": "response.created", "response": response})
		writeSSE(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_" + state.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
		writeSSE(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		state.started = true
	}
	if delta.Kind == "text" {
		state.accumulated.WriteString(delta.Text)
		writeSSE(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": delta.Text})
	}
}

func writeStreamDone(w io.Writer, incoming providers.Protocol, state *streamState) {
	if state.done {
		return
	}
	state.done = true
	if incoming == providers.ProtocolChat {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	if incoming == providers.ProtocolMessages {
		if !state.started {
			writeTranslatedEvent(w, incoming, state, canonicalDelta{Kind: "finish", Finish: "stop"})
		}
		writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
		return
	}
	text := state.accumulated.String()
	writeSSE(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "text": text})
	writeSSE(w, "response.content_part.done", map[string]any{"type": "response.content_part.done", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}})
	response := map[string]any{"id": state.id, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": state.model, "output": []any{map[string]any{"id": "msg_" + state.id, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}}
	writeSSE(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
}

func readSSEEvent(r *bufio.Reader) (string, []byte, error) {
	var event string
	var data []byte
	for {
		line, err := r.ReadString('\n')
		trim := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trim, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(trim, "event:"))
		} else if strings.HasPrefix(trim, "data:") {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimSpace(strings.TrimPrefix(trim, "data:"))...)
		}
		if trim == "" && len(data) > 0 {
			return event, data, nil
		}
		if err != nil {
			return event, data, err
		}
	}
}
func writeSSE(w io.Writer, event string, payload any) {
	body, _ := json.Marshal(payload)
	if event != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
}

func asSlice(value any) []any {
	if value == nil {
		return nil
	}
	if values, ok := value.([]any); ok {
		return values
	}
	return nil
}

func userMessageToResponsesItem(msg map[string]any) (map[string]any, error) {
	return messageToResponsesItem(msg, "user")
}

func messageToResponsesItem(msg map[string]any, role string) (map[string]any, error) {
	parts, err := chatContentToResponsesParts(msg["content"])
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": parts,
	}, nil
}

func hasChatMessageContent(value any) bool {
	switch content := value.(type) {
	case string:
		return content != ""
	case []any:
		return len(content) > 0
	default:
		return value != nil
	}
}

func assistantMessageToResponsesItem(msg map[string]any) (map[string]any, error) {
	parts, err := chatContentToResponsesParts(msg["content"])
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": parts,
	}, nil
}

func chatContentToResponsesParts(value any) ([]any, error) {
	if str, ok := value.(string); ok {
		return []any{map[string]any{"type": "input_text", "text": str}}, nil
	}
	parts := []any{}
	list := asSlice(value)
	if list == nil {
		return nil, unsupportedFeature{"message content"}
	}
	for _, raw := range list {
		block, ok := raw.(map[string]any)
		if !ok {
			return nil, unsupportedFeature{"message content blocks"}
		}
		var part map[string]any
		switch block["type"] {
		case "text":
			text, ok := block["text"].(string)
			if !ok {
				return nil, unsupportedFeature{"text content blocks"}
			}
			part = map[string]any{"type": "input_text", "text": text}
		case "image_url":
			url, ok := block["image_url"].(map[string]any)
			value, valueOK := url["url"].(string)
			if !ok || !valueOK || value == "" {
				return nil, unsupportedFeature{"image content blocks"}
			}
			part = map[string]any{"type": "input_image", "image_url": value}
		default:
			return nil, unsupportedFeature{"content block " + fmt.Sprint(block["type"])}
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// suppressToolChoice is a sentinel returned by convertToolChoice to signal
// that the caller should omit both tool_choice and tools from the relay
// request (the Chat "none" semantics: do not call tools).
const suppressToolChoice = "__suppress_tools__"

func convertToolChoice(value any) (any, error) {
	// The Responses wire shape used by chat-completions translation only
	// supports tool_choice as the string "auto". "none" means "do not
	// call tools" — the only safe relay representation is to omit tools
	// entirely. "required" and named function choices have no equivalent
	// and are rejected as unsupported rather than silently weakened.
	v, ok := value.(string)
	if !ok {
		return nil, unsupportedFeature{"tool_choice"}
	}
	switch v {
	case "auto":
		return v, nil
	case "none":
		return suppressToolChoice, nil
	default:
		return nil, unsupportedFeature{"tool_choice " + v}
	}
}

func chatContentToAnthropic(value any) []any {
	if text, ok := value.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}
	}
	out := []any{}
	for _, raw := range asSlice(value) {
		block, _ := raw.(map[string]any)
		switch block["type"] {
		case "text", "input_text":
			out = append(out, map[string]any{"type": "text", "text": first(block["text"], block["input_text"])})
		case "image_url", "input_image":
			out = append(out, openAIImageToAnthropic(block))
		}
	}
	return out
}
func responsesContentToChat(value any) any {
	out := []any{}
	for _, raw := range asSlice(value) {
		block, _ := raw.(map[string]any)
		switch block["type"] {
		case "input_text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": block["text"]})
		case "input_image":
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": block["image_url"]}})
		}
	}
	return out
}
func openAIImageToAnthropic(block map[string]any) map[string]any {
	raw := block["image_url"]
	if nested, ok := raw.(map[string]any); ok {
		raw = nested["url"]
	}
	urlText, _ := raw.(string)
	if strings.HasPrefix(urlText, "data:") {
		parts := strings.SplitN(urlText, ",", 2)
		meta := strings.TrimPrefix(strings.Split(parts[0], ";")[0], "data:")
		data := ""
		if len(parts) == 2 {
			data = parts[1]
		}
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": meta, "data": data}}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": urlText}}
}
func anthropicImageToOpenAI(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	urlText := fmt.Sprint(source["url"])
	if source["type"] == "base64" {
		urlText = "data:" + fmt.Sprint(source["media_type"]) + ";base64," + fmt.Sprint(source["data"])
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": urlText}}
}
func jsonString(value any) string { body, _ := json.Marshal(value); return string(body) }
func jsonValue(value any) any {
	if text, ok := value.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return decoded
		}
	}
	return value
}
func number(value any) float64 {
	if n, ok := value.(float64); ok {
		return n
	}
	return 0
}
func first(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return ""
}
func emptyNil(value string) any {
	if value == "" || value == "<nil>" {
		return nil
	}
	return value
}
func normalizeFinish(value string) string {
	switch value {
	case "end_turn", "stop_sequence", "stop":
		return "stop"
	case "max_tokens", "length":
		return "length"
	case "tool_use", "tool_calls":
		return "tool_calls"
	}
	return value
}
func normalizeAnthropicFinish(value string) string {
	switch value {
	case "stop", "end_turn":
		return "end_turn"
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "tool_use":
		return "tool_use"
	}
	return value
}
