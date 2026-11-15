package web

import (
	"io/fs"
	"strings"
	"testing"
)

// TestMailProviderBranchesDisableInactiveControls guards the fix for a form
// that could never be submitted: a hidden-but-enabled SMTP port (type=number,
// min=1) kept the whole form invalid, so the browser refused to submit and no
// request was ever sent. Every provider branch must therefore be disabled when
// inactive, not merely hidden.
func TestMailProviderBranchesDisableInactiveControls(t *testing.T) {
	t.Parallel()
	body, err := fs.ReadFile(files, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(body)

	if !strings.Contains(js, "el.querySelectorAll('input, select, textarea').forEach(control => {") {
		t.Fatal("applyMailProviderVisibility no longer disables inactive controls")
	}
	if !strings.Contains(js, "control.disabled = !active;") {
		t.Fatal("applyMailProviderVisibility must set control.disabled for inactive branches")
	}

	// The bounded numeric control that triggered the bug must stay inside the
	// SMTP branch, where it is disabled whenever SMTP is not the provider.
	html, err := fs.ReadFile(files, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	idx := strings.Index(string(html), `name="mail_smtp_port"`)
	if idx < 0 {
		t.Fatal("mail_smtp_port input not found")
	}
	start := strings.LastIndex(string(html)[:idx], `<label data-mail-when="`)
	if start < 0 || !strings.HasPrefix(string(html)[start:], `<label data-mail-when="smtp"`) {
		t.Fatal("mail_smtp_port must live in the smtp data-mail-when branch so it is disabled when inactive")
	}
}
