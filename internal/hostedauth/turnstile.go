package hostedauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

var ErrTurnstileRejected = errors.New("hostedauth: Turnstile response rejected")

type TurnstileVerifier struct {
	client *http.Client
}

func NewTurnstileVerifier(client *http.Client) *TurnstileVerifier {
	return &TurnstileVerifier{client: client}
}

func (v *TurnstileVerifier) Verify(ctx context.Context, secret, token, hostname, action string) error {
	if v == nil || v.client == nil || secret == "" || token == "" || len(token) > 2048 {
		return ErrTurnstileRejected
	}
	form := url.Values{"secret": {secret}, "response": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileVerifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("hostedauth: Turnstile verification unavailable")
	}
	var result struct {
		Success  bool     `json:"success"`
		Hostname string   `json:"hostname"`
		Action   string   `json:"action"`
		Errors   []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return err
	}
	if !result.Success || !strings.EqualFold(result.Hostname, hostname) || result.Action != action {
		return ErrTurnstileRejected
	}
	return nil
}
