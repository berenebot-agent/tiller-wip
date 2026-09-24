// Package mailer delivers the small set of transactional messages used by
// hosted identity flows. Provider credentials are held only in memory here;
// persistence and encryption belong to the platform-settings boundary.
package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotConfigured = errors.New("mailer: not configured")
	ErrInvalidConfig = errors.New("mailer: invalid configuration")
)

// Config is the runtime mail configuration. Secret fields are intentionally not
// exposed by the platform API; callers should use PublicConfig for status.
type Config struct {
	Provider     string
	From         string
	ResendAPIKey string
	BrevoAPIKey  string
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPMode     string
}

// PublicConfig is the non-secret status exposed to platform operators.
type PublicConfig struct {
	Provider         string `json:"provider"`
	From             string `json:"from"`
	SMTPHost         string `json:"smtp_host,omitempty"`
	SMTPPort         int    `json:"smtp_port,omitempty"`
	SMTPUsername     string `json:"smtp_username,omitempty"`
	SMTPMode         string `json:"smtp_mode,omitempty"`
	Configured       bool   `json:"configured"`
	SecretConfigured bool   `json:"secret_configured"`
}

// Message is a plain-text transactional message. Body content is never logged.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Mailer is the provider-neutral delivery boundary.
type Mailer interface {
	Send(context.Context, Message) error
}

// Manager hot-swaps the active provider without restarting the process. The
// current mailer is copied under a mutex and called after the lock is released,
// so SMTP/HTTP I/O never blocks configuration updates.
type Manager struct {
	mu     sync.RWMutex
	config Config
	active Mailer
}

// NewManager returns a manager with an optional initial configuration. An empty
// config is valid: hosted mode may boot before an operator configures mail.
func NewManager(initial Config) (*Manager, error) {
	m := &Manager{}
	if initial.Provider == "" {
		return m, nil
	}
	if err := m.Update(initial); err != nil {
		return nil, err
	}
	return m, nil
}

// Update validates and atomically replaces the active provider.
func (m *Manager) Update(cfg Config) error {
	active, err := build(cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.config, m.active = cfg, active
	m.mu.Unlock()
	return nil
}

// Clear disables delivery without deleting the caller's persisted settings.
func (m *Manager) Clear() {
	m.mu.Lock()
	m.config, m.active = Config{}, nil
	m.mu.Unlock()
}

func (m *Manager) Send(ctx context.Context, message Message) error {
	m.mu.RLock()
	active := m.active
	m.mu.RUnlock()
	if active == nil {
		return ErrNotConfigured
	}
	return active.Send(ctx, message)
}

func (m *Manager) Status() PublicConfig {
	m.mu.RLock()
	cfg := m.config
	m.mu.RUnlock()
	return publicConfig(cfg)
}

func publicConfig(cfg Config) PublicConfig {
	return PublicConfig{
		Provider: cfg.Provider, From: cfg.From, SMTPHost: cfg.SMTPHost,
		SMTPPort: cfg.SMTPPort, SMTPUsername: cfg.SMTPUsername, SMTPMode: cfg.SMTPMode,
		Configured:       cfg.Provider != "" && cfg.From != "",
		SecretConfigured: cfg.ResendAPIKey != "" || cfg.BrevoAPIKey != "" || cfg.SMTPPassword != "" || cfg.SMTPUsername != "",
	}
}

func build(cfg Config) (Mailer, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	switch cfg.Provider {
	case "resend":
		return &resendMailer{from: cfg.From, apiKey: cfg.ResendAPIKey, client: &http.Client{Timeout: 15 * time.Second}}, nil
	case "brevo":
		return &brevoMailer{from: cfg.From, apiKey: cfg.BrevoAPIKey, client: &http.Client{Timeout: 15 * time.Second}}, nil
	case "smtp":
		return &smtpMailer{from: cfg.From, host: cfg.SMTPHost, port: cfg.SMTPPort, username: cfg.SMTPUsername, password: cfg.SMTPPassword, mode: cfg.SMTPMode, timeout: 15 * time.Second}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported provider", ErrInvalidConfig)
	}
}

// Validate enforces the hosted mail transport policy.
func Validate(cfg Config) error {
	if cfg.Provider == "" {
		return nil
	}
	if cfg.From == "" || strings.ContainsAny(cfg.From, "\r\n") {
		return fmt.Errorf("%w: from address is required and must not contain newlines", ErrInvalidConfig)
	}
	if _, err := mail.ParseAddress(cfg.From); err != nil {
		return fmt.Errorf("%w: invalid from address", ErrInvalidConfig)
	}
	switch cfg.Provider {
	case "resend":
		if cfg.ResendAPIKey == "" {
			return fmt.Errorf("%w: resend API key is required", ErrInvalidConfig)
		}
		return nil
	case "brevo":
		if cfg.BrevoAPIKey == "" {
			return fmt.Errorf("%w: brevo API key is required", ErrInvalidConfig)
		}
		return nil
	case "smtp":
		if cfg.SMTPHost == "" || strings.ContainsAny(cfg.SMTPHost, "\r\n") {
			return fmt.Errorf("%w: SMTP host is required", ErrInvalidConfig)
		}
		if cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
			return fmt.Errorf("%w: SMTP port is invalid", ErrInvalidConfig)
		}
		if cfg.SMTPMode != "starttls" && cfg.SMTPMode != "implicit" {
			return fmt.Errorf("%w: SMTP mode must be starttls or implicit", ErrInvalidConfig)
		}
		if (cfg.SMTPUsername == "") != (cfg.SMTPPassword == "") {
			return fmt.Errorf("%w: SMTP username and password must be supplied together", ErrInvalidConfig)
		}
		return nil
	default:
		return fmt.Errorf("%w: provider must be resend, brevo, or smtp", ErrInvalidConfig)
	}
}

type resendMailer struct {
	from   string
	apiKey string
	client *http.Client
}

func (m *resendMailer) Send(ctx context.Context, message Message) error {
	if strings.ContainsAny(message.To+message.Subject, "\r\n") {
		return errors.New("mailer: invalid message headers")
	}
	payload, err := json.Marshal(map[string]any{"from": m.from, "to": []string{message.To}, "subject": message.Subject, "text": message.Text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend delivery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		return fmt.Errorf("resend delivery returned HTTP %d", resp.StatusCode)
	}
	return nil
}

type brevoMailer struct {
	from   string
	apiKey string
	client *http.Client
}

func (m *brevoMailer) Send(ctx context.Context, message Message) error {
	if strings.ContainsAny(message.To+message.Subject, "\r\n") {
		return errors.New("mailer: invalid message headers")
	}
	payload, err := json.Marshal(map[string]any{
		"sender":      map[string]string{"email": m.from},
		"to":          []map[string]string{{"email": message.To}},
		"subject":     message.Subject,
		"textContent": message.Text,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.brevo.com/v3/smtp/email", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("api-key", m.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("brevo delivery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		return fmt.Errorf("brevo delivery returned HTTP %d", resp.StatusCode)
	}
	return nil
}

type smtpMailer struct {
	from, host, username, password, mode string
	port                                 int
	timeout                              time.Duration
}

func (m *smtpMailer) Send(ctx context.Context, message Message) error {
	from, err := mail.ParseAddress(m.from)
	if err != nil {
		return errors.New("mailer: invalid from address")
	}
	to, err := mail.ParseAddress(message.To)
	if err != nil || strings.ContainsAny(message.Subject, "\r\n") {
		return errors.New("mailer: invalid recipient or subject")
	}
	address := net.JoinHostPort(m.host, strconv.Itoa(m.port))
	dialer := &net.Dialer{Timeout: m.timeout}
	var conn net.Conn
	if m.mode == "implicit" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return fmt.Errorf("SMTP connection: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(m.timeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	_ = conn.SetDeadline(deadline)
	client, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return fmt.Errorf("SMTP handshake: %w", err)
	}
	defer client.Close()
	if m.mode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server does not support STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("SMTP STARTTLS: %w", err)
		}
	}
	if m.username != "" {
		if err := client.Auth(smtp.PlainAuth("", m.username, m.password, m.host)); err != nil {
			return fmt.Errorf("SMTP authentication: %w", err)
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return fmt.Errorf("SMTP sender: %w", err)
	}
	if err := client.Rcpt(to.Address); err != nil {
		return fmt.Errorf("SMTP recipient: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP data: %w", err)
	}
	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n", from.String(), to.String(), message.Subject, message.Text)
	if _, err := io.WriteString(writer, body); err != nil {
		_ = writer.Close()
		return fmt.Errorf("SMTP write: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("SMTP commit: %w", err)
	}
	return client.Quit()
}
