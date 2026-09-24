package identity

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestUserSessionRenewalCASConflictReloadsWinner(t *testing.T) {
	first, db := newTestStore(t)
	ctx := context.Background()
	signup, err := first.CreateSignup(ctx, "renew@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	user, err := first.ConsumeVerification(ctx, signup.VerificationToken)
	if err != nil {
		t.Fatal(err)
	}
	session, err := first.CreateUserSession(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(db, first.passwordHasher, first.tokenHasher, first.credentialHasher, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	selector, _, _ := parseOpaqueToken(session.Token)
	if _, err := db.Exec(`UPDATE user_sessions SET expires_at=? WHERE id=?`, time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339Nano), selector); err != nil {
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
		_, ok := first.GetUserSession(ctx, session.Token)
		firstDone <- ok
	}()
	<-entered
	secondDone := make(chan bool, 1)
	go func() {
		_, ok := second.GetUserSession(ctx, session.Token)
		secondDone <- ok
	}()
	if !<-secondDone {
		t.Fatal("winning user renewal rejected session")
	}
	close(release)
	if !<-firstDone {
		t.Fatal("CAS-conflict user renewal rejected session")
	}
}

func TestPlatformSessionRenewalCASConflictReloadsWinner(t *testing.T) {
	first, db := newTestStore(t)
	ctx := context.Background()
	session, err := first.CreatePlatformSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(db, first.passwordHasher, first.tokenHasher, first.credentialHasher, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	selector, _, _ := parseOpaqueToken(session.Token)
	if _, err := db.Exec(`UPDATE platform_admin_sessions SET expires_at=? WHERE id=?`, time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339Nano), selector); err != nil {
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
		_, ok := first.GetPlatformSession(ctx, session.Token)
		firstDone <- ok
	}()
	<-entered
	secondDone := make(chan bool, 1)
	go func() {
		_, ok := second.GetPlatformSession(ctx, session.Token)
		secondDone <- ok
	}()
	if !<-secondDone {
		t.Fatal("winning platform renewal rejected session")
	}
	close(release)
	if !<-firstDone {
		t.Fatal("CAS-conflict platform renewal rejected session")
	}
}
