package server

import "testing"

func TestEstimateTokensChatMessages(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello world"},{"role":"assistant","content":[{"type":"text","text":"hi there"}]}]}`)
	// "hello world" = 11 runes, "hi there" = 8 runes => 19 runes / 3 = 7 (ceil).
	got := EstimateTokens(body, "chat")
	if got != 7 {
		t.Fatalf("estimate = %d, want 7", got)
	}
}

func TestEstimateTokensIgnoresMetadata(t *testing.T) {
	// role/model/type/id must not inflate the estimate.
	body := []byte(`{"model":"a-very-long-model-identifier-indeed","messages":[{"role":"assistant","id":"msg_1234567890","content":"hi"}]}`)
	if got := EstimateTokens(body, "chat"); got != 1 {
		t.Fatalf("estimate = %d, want 1 (only content counted)", got)
	}
}

func TestEstimateTokensResponsesInput(t *testing.T) {
	body := []byte(`{"input":"abcdef","model":"m"}`)
	if got := EstimateTokens(body, "responses"); got != 2 {
		t.Fatalf("estimate = %d, want 2", got)
	}
}

func TestEstimateTokensAnthropicSystem(t *testing.T) {
	body := []byte(`{"system":"12345","messages":[{"role":"user","content":"678"}]}`)
	// 5 + 3 = 8 runes / 3 = 3 (ceil).
	if got := EstimateTokens(body, "messages"); got != 3 {
		t.Fatalf("estimate = %d, want 3", got)
	}
}

func TestEstimateTokensCJKOverEstimates(t *testing.T) {
	// 9 CJK runes at ratio 3 => ceil(3) = 3; conservative but non-zero.
	body := []byte(`{"messages":[{"role":"user","content":"你好世界你好世界你"}]}`)
	if got := EstimateTokens(body, "chat"); got == 0 {
		t.Fatal("expected a non-zero CJK estimate")
	}
}

func TestEstimateTokensEmptyAndMalformed(t *testing.T) {
	if got := EstimateTokens(nil, "chat"); got != 0 {
		t.Fatalf("empty estimate = %d, want 0", got)
	}
	if got := EstimateTokens([]byte("{not json"), "chat"); got != 0 {
		t.Fatalf("malformed estimate = %d, want 0", got)
	}
}

func TestEstimateTokensCustomRatio(t *testing.T) {
	body := []byte(`{"input":"abcdef"}`)
	if got := EstimateTokensWithRatio(body, "responses", 6); got != 1 {
		t.Fatalf("estimate with ratio 6 = %d, want 1", got)
	}
}
