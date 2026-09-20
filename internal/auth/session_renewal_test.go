package auth

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

type renewalTestHasher struct{}

func (renewalTestHasher) Hash(secret string) (string, error) { return secret, nil }
func (renewalTestHasher) Verify(secret, encoded string) bool { return secret == encoded }
func (renewalTestHasher) NeedsRehash(string) bool            { return false }

func TestSessionRenewalCASConflictReloadsWinner(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, err := NewSessionStoreWithHasher(db.SQL, "admin", "pw", time.Hour, renewalTestHasher{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := first.Create()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSessionStoreWithHasher(db.SQL, "admin", "pw", time.Hour, renewalTestHasher{})
	if err != nil {
		t.Fatal(err)
	}
	selector, _, _ := ParseSessionToken(session.Token)
	oldExpiry := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL.Exec(`UPDATE admin_sessions SET expires_at=? WHERE id=?`, oldExpiry, selector); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	first.renewHook = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	firstDone := make(chan bool, 1)
	go func() {
		_, ok := first.Get(session.Token)
		firstDone <- ok
	}()
	<-entered
	secondDone := make(chan bool, 1)
	go func() {
		_, ok := second.Get(session.Token)
		secondDone <- ok
	}()
	if !<-secondDone {
		t.Fatal("winning renewal rejected session")
	}
	close(release)
	if !<-firstDone {
		t.Fatal("CAS-conflict renewal rejected session")
	}
}
