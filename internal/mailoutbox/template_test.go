package mailoutbox

import (
	"strings"
	"testing"
)

func TestRenderBodiesCarryMatchingTextAndHTML(t *testing.T) {
	cases := []struct {
		msgType   string
		params    map[string]string
		wantLink  string
		wantLabel string
	}{
		{TypeVerifyEmail, nil, "https://router.example/verify-email?token=tok", "Verify email"},
		{TypePasswordReset, nil, "https://router.example/reset-password?token=tok", "Reset password"},
		{TypeEmailChangeConfirm, nil, "https://router.example/confirm-email-change?token=tok", "Confirm new email"},
		{TypeEmailChangeWarning, map[string]string{"new_email": "new@example.com"}, "https://router.example/forgot-password", "Reset your password"},
	}
	for _, tc := range cases {
		t.Run(tc.msgType, func(t *testing.T) {
			rendered, ok := render(tc.msgType, "https://router.example", "tok", tc.params)
			if !ok {
				t.Fatal("render reported unknown type")
			}
			if rendered.Subject == "" || rendered.Text == "" || rendered.HTML == "" {
				t.Fatalf("empty fields: %+v", rendered)
			}
			if !strings.Contains(rendered.Text, tc.wantLink) {
				t.Fatalf("text body missing link %q:\n%s", tc.wantLink, rendered.Text)
			}
			if !strings.Contains(rendered.HTML, tc.wantLink) {
				t.Fatalf("html body missing link %q", tc.wantLink)
			}
			if !strings.Contains(rendered.HTML, tc.wantLabel) {
				t.Fatalf("html body missing button label %q", tc.wantLabel)
			}
			if !strings.Contains(rendered.HTML, "<!DOCTYPE html>") {
				t.Fatal("html body missing doctype")
			}
		})
	}
}

func TestRenderUnknownType(t *testing.T) {
	if _, ok := render("nope", "https://router.example", "tok", nil); ok {
		t.Fatal("unknown type reported as rendered")
	}
}

func TestRenderEscapesHTML(t *testing.T) {
	rendered, ok := render(TypeEmailChangeWarning, "https://router.example", "", map[string]string{
		"new_email": `evil"<script>alert(1)</script>@example.com`,
	})
	if !ok {
		t.Fatal("render reported unknown type")
	}
	if strings.Contains(rendered.HTML, "<script>") {
		t.Fatalf("html body did not escape the new email:\n%s", rendered.HTML)
	}
	if !strings.Contains(rendered.HTML, "&lt;script&gt;") {
		t.Fatalf("html body missing escaped email:\n%s", rendered.HTML)
	}
}

func TestRenderEncodesTokenInLink(t *testing.T) {
	rendered, _ := render(TypePasswordReset, "https://router.example/", "a b/c+d", nil)
	if !strings.Contains(rendered.HTML, "token=a+b%2Fc%2Bd") {
		t.Fatalf("token was not URL-encoded in link:\n%s", rendered.HTML)
	}
}
