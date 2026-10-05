package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestValidateMailConfigs(t *testing.T) {
	valid := []Config{
		{Provider: "resend", From: "Tiller <no-reply@example.com>", ResendAPIKey: "re_test"},
		{Provider: "brevo", From: "no-reply@example.com", BrevoAPIKey: "xkeysib-test"},
		{Provider: "smtp", From: "no-reply@example.com", SMTPHost: "smtp.example.com", SMTPPort: 587, SMTPMode: "starttls"},
	}
	for _, cfg := range valid {
		if err := Validate(cfg); err != nil {
			t.Errorf("valid config %+v: %v", cfg, err)
		}
	}
	invalid := []Config{
		{Provider: "resend", From: "x@y.z"},
		{Provider: "brevo", From: "x@y.z"},
		{Provider: "smtp", From: "x@y.z", SMTPHost: "h", SMTPPort: 587, SMTPMode: "plain"},
		{Provider: "smtp", From: "x@y.z", SMTPHost: "h", SMTPPort: 587, SMTPMode: "starttls", SMTPUsername: "u"},
	}
	for _, cfg := range invalid {
		if err := Validate(cfg); err == nil {
			t.Errorf("invalid config accepted: %+v", cfg)
		}
	}
}

func TestManagerStartsUnconfiguredAndHotSwaps(t *testing.T) {
	m, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(context.Background(), Message{To: "x@y.z", Subject: "x", Text: "x"}); err != ErrNotConfigured {
		t.Fatalf("unconfigured send = %v", err)
	}
	if err := m.Update(Config{Provider: "resend", From: "no-reply@example.com", ResendAPIKey: "re_test"}); err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	if !status.Configured || !status.SecretConfigured || status.Provider != "resend" {
		t.Fatalf("status = %+v", status)
	}
	m.Clear()
	if m.Status().Configured {
		t.Fatal("clear left mail configured")
	}
}

func TestBrevoSendReturnsProviderMessageID(t *testing.T) {
	const wantMessageID = "<20260927.12345@example.brevo.com>"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.brevo.com/v3/smtp/email" {
			t.Fatalf("request URL = %q", r.URL)
		}
		if got := r.Header.Get("api-key"); got != "test-api-key" {
			t.Fatalf("api-key header = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"messageId":"` + wantMessageID + `"}`)),
			Request:    r,
		}, nil
	})}
	mailer := &brevoMailer{from: "no-reply@example.com", apiKey: "test-api-key", client: client}
	result, err := mailer.Send(context.Background(), Message{To: "user@example.com", Subject: "Verify", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageID != wantMessageID {
		t.Fatalf("message ID = %q, want %q", result.MessageID, wantMessageID)
	}
}

func TestResendPayloadIncludesHTMLOnlyWhenPresent(t *testing.T) {
	plain := resendPayload("no-reply@example.com", Message{To: "u@example.com", Subject: "s", Text: "t"})
	if _, ok := plain["html"]; ok {
		t.Fatal("plain message carried an html field")
	}
	rich := resendPayload("no-reply@example.com", Message{To: "u@example.com", Subject: "s", Text: "t", HTML: "<p>hi</p>"})
	if rich["html"] != "<p>hi</p>" {
		t.Fatalf("html field = %v", rich["html"])
	}
	if rich["text"] != "t" {
		t.Fatalf("text field = %v", rich["text"])
	}
}

func TestBrevoPayloadIncludesHTMLContentOnlyWhenPresent(t *testing.T) {
	plain := brevoPayload("no-reply@example.com", Message{To: "u@example.com", Subject: "s", Text: "t"})
	if _, ok := plain["htmlContent"]; ok {
		t.Fatal("plain message carried an htmlContent field")
	}
	rich := brevoPayload("no-reply@example.com", Message{To: "u@example.com", Subject: "s", Text: "t", HTML: "<p>hi</p>"})
	if rich["htmlContent"] != "<p>hi</p>" {
		t.Fatalf("htmlContent field = %v", rich["htmlContent"])
	}
	if rich["textContent"] != "t" {
		t.Fatalf("textContent field = %v", rich["textContent"])
	}
}

func TestBrevoSendIncludesHTMLContentInRequest(t *testing.T) {
	var body map[string]any
	mailer := &brevoMailer{
		from:   "no-reply@example.com",
		apiKey: "test-api-key",
		client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(`{"messageId":"x"}`)),
				Request:    r,
			}, nil
		})},
	}
	if _, err := mailer.Send(context.Background(), Message{To: "u@example.com", Subject: "s", Text: "t", HTML: "<p>hi</p>"}); err != nil {
		t.Fatal(err)
	}
	if body["htmlContent"] != "<p>hi</p>" || body["textContent"] != "t" {
		t.Fatalf("request body = %+v", body)
	}
}

func TestBuildSMTPBodyPlainAndMultipart(t *testing.T) {
	plain := buildSMTPBody("no-reply@example.com", "u@example.com", Message{Subject: "s", Text: "hello"})
	if !strings.Contains(plain, "Content-Type: text/plain; charset=UTF-8") {
		t.Fatalf("plain body missing text/plain header:\n%s", plain)
	}
	if strings.Contains(plain, "multipart/alternative") {
		t.Fatalf("plain body unexpectedly multipart:\n%s", plain)
	}

	rich := buildSMTPBody("no-reply@example.com", "u@example.com", Message{Subject: "s", Text: "hello", HTML: "<p>hi</p>"})
	if !strings.Contains(rich, "Content-Type: multipart/alternative;") {
		t.Fatalf("rich body missing multipart header:\n%s", rich)
	}
	if !strings.Contains(rich, "Content-Type: text/plain; charset=UTF-8") {
		t.Fatalf("rich body missing text part:\n%s", rich)
	}
	if !strings.Contains(rich, "Content-Type: text/html; charset=UTF-8") {
		t.Fatalf("rich body missing html part:\n%s", rich)
	}
	if !strings.Contains(rich, "<p>hi</p>") {
		t.Fatalf("rich body missing html content:\n%s", rich)
	}
	if !strings.Contains(rich, "--\r\n") && !strings.Contains(rich, "--") {
		t.Fatalf("rich body missing closing boundary:\n%s", rich)
	}
}

func TestBrevoSendAcceptsMissingOrMalformedMessageID(t *testing.T) {
	for _, body := range []string{"", "not-json", `{"messageId":42}`} {
		t.Run(body, func(t *testing.T) {
			mailer := &brevoMailer{
				from:   "no-reply@example.com",
				apiKey: "test-api-key",
				client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusCreated,
						Header:     make(http.Header),
						Body:       io.NopCloser(bytes.NewBufferString(body)),
						Request:    r,
					}, nil
				})},
			}
			result, err := mailer.Send(context.Background(), Message{To: "user@example.com", Subject: "Verify", Text: "body"})
			if err != nil {
				t.Fatalf("accepted 2xx response returned error: %v", err)
			}
			if result.MessageID != "" {
				t.Fatalf("message ID = %q, want empty", result.MessageID)
			}
		})
	}
}
