package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Free-text fields must opt out of browser autofill. Otherwise password
// managers and address autofill treat them as login fields and offer the
// signed-in username.
var inputTagPattern = regexp.MustCompile(`<input\b[^>]*>`)

// fieldTagPattern matches every text-entry control. Checkboxes, radios, hidden
// inputs, file pickers and sliders are never autofilled.
var (
	fieldTagPattern         = regexp.MustCompile(`<(?:input|textarea)\b[^>]*>`)
	exemptFromAutofillCheck = regexp.MustCompile(`type="(?:checkbox|radio|hidden|file|range)"`)
	// passwordAutofillOptOut matches a password field that has explicitly
	// declined every major password manager. A field like this must NOT carry
	// current-password/new-password: those hint tokens are exactly what makes a
	// manager offer, and leave, a saved password in the box.
	passwordAutofillOptOut = regexp.MustCompile(`autocomplete="off".*data-1p-ignore.*data-lpignore="true".*data-bwignore`)
)

func TestFreeTextFieldsDisableAutofill(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"assets/index.html", "assets/app.js"} {
		body, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range fieldTagPattern.FindAllString(string(body), -1) {
			if exemptFromAutofillCheck.MatchString(tag) {
				continue
			}
			if !strings.Contains(tag, "autocomplete=") {
				t.Errorf("%s: field has no autocomplete attribute: %s", name, tag)
			}
			if strings.Contains(tag, `autocomplete="current-password"`) || strings.Contains(tag, `autocomplete="new-password"`) {
				continue
			}
			if strings.Contains(tag, `type="password"`) && !passwordAutofillOptOut.MatchString(tag) {
				t.Errorf("%s: password field needs current-password or new-password: %s", name, tag)
			}
		}
	}
}

// API token dialogs must not carry a password field: browsers would treat the
// preceding Name input as a username and offer saved credentials.
func TestAPITokenFormsHaveNoPasswordField(t *testing.T) {
	t.Parallel()
	body, err := fs.ReadFile(files, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "token_password") {
		t.Error("app.js: API token forms must not include a password field")
	}
}

func TestPlatformAccountSearchDoesNotLookLikeLogin(t *testing.T) {
	t.Parallel()
	body, err := fs.ReadFile(files, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range inputTagPattern.FindAllString(string(body), -1) {
		if !strings.Contains(tag, `id="platform-account-search"`) {
			continue
		}
		for _, banned := range []string{"user", "email", "login"} {
			if strings.Contains(strings.ToLower(tag), banned) {
				t.Errorf("platform account search mentions %q and may trigger autofill: %s", banned, tag)
			}
		}
		return
	}
	t.Fatal("platform-account-search input not found")
}
