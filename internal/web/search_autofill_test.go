package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Search and filter boxes must opt out of browser autofill. Otherwise password
// managers and address autofill treat them as login fields and offer the
// signed-in username.
var inputTagPattern = regexp.MustCompile(`<input\b[^>]*>`)

func TestSearchAndTextInputsDisableAutofill(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"assets/index.html", "assets/app.js"} {
		body, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range inputTagPattern.FindAllString(string(body), -1) {
			if !strings.Contains(tag, `type="search"`) && !strings.Contains(tag, `type="text"`) {
				continue
			}
			if strings.Contains(tag, `name="title"`) {
				continue // legal document title, not a search or filter box
			}
			if !strings.Contains(tag, `autocomplete="off"`) {
				t.Errorf("%s: input lacks autocomplete=\"off\": %s", name, tag)
			}
		}
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
