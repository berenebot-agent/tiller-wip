package mailer

import (
	"context"
	"testing"
)

func TestValidateMailConfigs(t *testing.T) {
	valid := []Config{
		{Provider: "resend", From: "Tiller <no-reply@example.com>", ResendAPIKey: "re_test"},
		{Provider: "smtp", From: "no-reply@example.com", SMTPHost: "smtp.example.com", SMTPPort: 587, SMTPMode: "starttls"},
		{Provider: "ses", From: "no-reply@example.com", SMTPHost: "email-smtp.example.com", SMTPPort: 465, SMTPMode: "implicit", SMTPUsername: "u", SMTPPassword: "p"},
	}
	for _, cfg := range valid {
		if err := Validate(cfg); err != nil {
			t.Errorf("valid config %+v: %v", cfg, err)
		}
	}
	invalid := []Config{
		{Provider: "resend", From: "x@y.z"},
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
	if err := m.Send(context.Background(), Message{To: "x@y.z", Subject: "x", Text: "x"}); err != ErrNotConfigured {
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
