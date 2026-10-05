package mailoutbox

import (
	"html/template"
	"net/url"
	"strings"
)

// Rendered is the fully composed content of a transactional message. Text is
// always populated; HTML is the richer alternative. Values are escaped here so
// the rest of the package only deals with final strings.
type Rendered struct {
	Subject string
	Text    string
	HTML    string
}

// render produces the text and HTML bodies for a message type. The raw token is
// already decrypted; baseURL is the configured public origin.
func render(msgType, baseURL, token string, params map[string]string) (Rendered, bool) {
	switch msgType {
	case TypeVerifyEmail:
		link := strings.TrimRight(baseURL, "/") + "/verify-email?token=" + url.QueryEscape(token)
		return Rendered{
			Subject: "Verify your Tiller account",
			Text:    "Verify your Tiller account:\n\n" + link + "\n\nThis link expires in 24 hours.",
			HTML: layout(layoutData{
				preheader: "Confirm your email to activate your Tiller account.",
				heading:   "Verify your email",
				paragraphs: []string{
					"Confirm this address to activate your Tiller Router account.",
				},
				buttonLabel: "Verify email",
				buttonURL:   link,
				footnote:    "This link expires in 24 hours. If you didn't create a Tiller account, you can ignore this email.",
			}),
		}, true
	case TypePasswordReset:
		link := strings.TrimRight(baseURL, "/") + "/reset-password?token=" + url.QueryEscape(token)
		return Rendered{
			Subject: "Reset your Tiller password",
			Text:    "Reset your Tiller password:\n\n" + link + "\n\nThis link expires in 1 hour.",
			HTML: layout(layoutData{
				preheader: "Choose a new password for your Tiller account.",
				heading:   "Reset your password",
				paragraphs: []string{
					"We received a request to reset the password for your Tiller Router account.",
				},
				buttonLabel: "Reset password",
				buttonURL:   link,
				footnote:    "This link expires in 1 hour. If you didn't request a reset, you can safely ignore this email.",
			}),
		}, true
	case TypeEmailChangeConfirm:
		link := strings.TrimRight(baseURL, "/") + "/confirm-email-change?token=" + url.QueryEscape(token)
		return Rendered{
			Subject: "Confirm your new Tiller email address",
			Text:    "Confirm your new Tiller email address:\n\n" + link + "\n\nThis link expires in 24 hours. If you did not request this change, you can ignore this message; your current address keeps working until you confirm.",
			HTML: layout(layoutData{
				preheader: "Confirm your new email address for Tiller.",
				heading:   "Confirm your new email",
				paragraphs: []string{
					"Confirm this address to make it the email on your Tiller Router account.",
				},
				buttonLabel: "Confirm new email",
				buttonURL:   link,
				footnote:    "This link expires in 24 hours. If you didn't request this change, you can ignore this email — your current address keeps working until you confirm.",
			}),
		}, true
	case TypeEmailChangeWarning:
		link := strings.TrimRight(baseURL, "/") + "/forgot-password"
		newEmail := params["new_email"]
		return Rendered{
			Subject: "Your Tiller email address is being changed",
			Text: "A request was made to change the email address on your Tiller account to " + newEmail + ".\n\n" +
				"If this was not you, reset your password immediately to cancel the change: " + link + "\n\n" +
				"The change does not take effect until the new address is confirmed.",
			HTML: layout(layoutData{
				preheader: "A change was requested for your Tiller email address.",
				heading:   "Your email is being changed",
				paragraphs: []string{
					"A request was made to change the email address on your Tiller Router account to:",
				},
				notice:      newEmail,
				buttonLabel: "Reset your password",
				buttonURL:   link,
				footnote:    "If this was you, no action is needed — the change takes effect once the new address is confirmed. If this wasn't you, reset your password to cancel it.",
			}),
		}, true
	default:
		return Rendered{}, false
	}
}

// layoutData is the content of one email body. The shared shell (header, card,
// button, footer) is built by layout so every message looks consistent.
type layoutData struct {
	preheader   string
	heading     string
	paragraphs  []string
	notice      string
	buttonLabel string
	buttonURL   string
	footnote    string
}

// layout renders the shared brand shell. It is table-based with inline CSS only
// (mail clients strip <style>), and every value is escaped.
func layout(d layoutData) string {
	e := template.HTMLEscapeString
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light only">
<title>` + e(d.heading) + `</title>
</head>
<body style="margin:0;padding:0;background-color:#f2f5f7;">
<span style="display:none!important;visibility:hidden;opacity:0;color:transparent;height:0;width:0;overflow:hidden;">` + e(d.preheader) + `</span>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#f2f5f7;">
<tr><td align="center" style="padding:32px 16px;">
<table role="presentation" width="560" cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:560px;background-color:#ffffff;border:1px solid #d5dee7;border-radius:6px;overflow:hidden;">
<tr><td style="background-color:#173454;padding:20px 32px;">
<span style="font-family:'Segoe UI',Tahoma,Arial,sans-serif;font-size:18px;letter-spacing:.08em;color:#ffffff;font-weight:600;">Tiller <span style="color:#9ed2f3;font-weight:700;">ROUTER</span></span>
</td></tr>
<tr><td style="padding:36px 32px 8px 32px;">
<h1 style="margin:0 0 18px 0;font-family:Georgia,'Times New Roman',serif;font-size:24px;line-height:1.25;color:#173454;font-weight:600;">` + e(d.heading) + `</h1>
`)
	for _, p := range d.paragraphs {
		b.WriteString(`<p style="margin:0 0 16px 0;font-family:'Segoe UI',Tahoma,Arial,sans-serif;font-size:15px;line-height:1.6;color:#283747;">` + e(p) + `</p>
`)
	}
	if d.notice != "" {
		b.WriteString(`<p style="margin:0 0 22px 0;padding:12px 16px;background-color:#e7f2fa;border-left:3px solid #2778b8;border-radius:3px;font-family:'SFMono-Regular',Consolas,monospace;font-size:14px;color:#173454;word-break:break-all;">` + e(d.notice) + `</p>
`)
	}
	if d.buttonLabel != "" && d.buttonURL != "" {
		b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:6px 0 20px 0;"><tr><td style="border-radius:4px;background-color:#2778b8;">
<a href="` + e(d.buttonURL) + `" style="display:inline-block;padding:12px 26px;font-family:'Segoe UI',Tahoma,Arial,sans-serif;font-size:15px;font-weight:600;color:#ffffff;text-decoration:none;border-radius:4px;">` + e(d.buttonLabel) + `</a>
</td></tr></table>
<p style="margin:0 0 12px 0;font-family:'Segoe UI',Tahoma,Arial,sans-serif;font-size:13px;line-height:1.6;color:#66788b;">Or open this link in your browser:</p>
<p style="margin:0 0 20px 0;font-family:'SFMono-Regular',Consolas,monospace;font-size:12px;line-height:1.6;word-break:break-all;"><a href="` + e(d.buttonURL) + `" style="color:#2778b8;text-decoration:underline;">` + e(d.buttonURL) + `</a></p>
`)
	}
	if d.footnote != "" {
		b.WriteString(`<p style="margin:0 0 28px 0;font-family:'Segoe UI',Tahoma,Arial,sans-serif;font-size:13px;line-height:1.6;color:#66788b;">` + e(d.footnote) + `</p>
`)
	}
	b.WriteString(`</td></tr>
<tr><td style="padding:18px 32px 26px 32px;border-top:1px solid #e2e8ed;">
<p style="margin:0;font-family:'Segoe UI',Tahoma,Arial,sans-serif;font-size:12px;line-height:1.6;color:#66788b;">Tiller Router — your self-hosted AI model router.<br>You're receiving this because of activity on your account.</p>
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`)
	return b.String()
}
