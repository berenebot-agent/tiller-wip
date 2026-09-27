package mailer

import (
	"bytes"
	"context"
	"io"
	"net/http"
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
