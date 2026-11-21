package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/store"
)

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func sampleCredentials() store.Credentials {
	return store.Credentials{
		ClientID:     "oaiapp_test",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		IDToken:      "id-1",
		ExpiresAt:    time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC),
		Email:        "user@example.com",
	}
}

func TestLoadReportsNoCredentialsOnAnEmptyStore(t *testing.T) {
	s, _ := openStore(t)

	_, err := s.Load()

	if !errors.Is(err, store.ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
}

func TestSaveWritesPrivateCredentialsThatLoadBack(t *testing.T) {
	s, dir := openStore(t)
	want := sampleCredentials()

	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
	got.ExpiresAt = want.ExpiresAt
	if got != want {
		t.Fatalf("loaded %+v, want %+v", got, want)
	}
	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credentials mode = %o, want 600", perm)
	}
}

func TestSaveRejectsTheDynamicRegistrationPlaceholder(t *testing.T) {
	s, _ := openStore(t)
	c := sampleCredentials()
	c.ClientID = "dynamic_agent_client"

	err := s.Save(c)

	if err == nil {
		t.Fatal("Save accepted dynamic_agent_client as the client ID")
	}
	if _, loadErr := s.Load(); !errors.Is(loadErr, store.ErrNoCredentials) {
		t.Fatalf("Load after rejected save: %v, want ErrNoCredentials", loadErr)
	}
}

func TestLockMakesASecondHolderWaitForRelease(t *testing.T) {
	first, dir := openStore(t)
	second, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	unlock, err := first.Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan func(), 1)
	go func() {
		u, err := second.Lock(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		acquired <- u
	}()

	select {
	case <-acquired:
		t.Fatal("second holder got the lock while the first still held it")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case u := <-acquired:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("second holder never got the lock after release")
	}
}

func TestLockGivesUpWhenTheContextEnds(t *testing.T) {
	first, dir := openStore(t)
	second, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := first.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		u, err := second.Lock(ctx)
		if u != nil {
			u()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lock kept waiting after its context ended")
	}
}

func TestStateIsMissingWithoutCredentials(t *testing.T) {
	s, _ := openStore(t)

	if got := s.State(); got != auth.StateMissing {
		t.Fatalf("State() = %q, want missing", got)
	}
}

func TestStateIsValidWithARefreshToken(t *testing.T) {
	s, _ := openStore(t)
	if err := s.Save(sampleCredentials()); err != nil {
		t.Fatal(err)
	}

	if got := s.State(); got != auth.StateValid {
		t.Fatalf("State() = %q, want valid", got)
	}
}

func TestStateIsDeadWhenTokensWereClearedButTheClientIDRemains(t *testing.T) {
	s, _ := openStore(t)
	if err := s.Save(store.Credentials{ClientID: "oaiapp_test"}); err != nil {
		t.Fatal(err)
	}

	if got := s.State(); got != auth.StateDead {
		t.Fatalf("State() = %q, want dead", got)
	}
}

func TestOpenCreatesPrivateDataDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	if _, err := store.Open(dir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("data dir mode = %o, want 700", perm)
	}
}

func TestHostIDIsCreatedOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	first, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := first.HostID()
	if err != nil {
		t.Fatal(err)
	}

	second, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := second.HostID()
	if err != nil {
		t.Fatal(err)
	}

	if len(id) < 16 {
		t.Fatalf("host id %q is too short to be unique", id)
	}
	if again != id {
		t.Fatalf("host id changed from %q to %q", id, again)
	}
}

func TestOpenTightensAnExistingWideDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Open(dir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("data dir mode = %o, want 700", perm)
	}
}

func TestDeleteRemovesCredentialsButKeepsTheHostID(t *testing.T) {
	s, _ := openStore(t)
	hostID, err := s.HostID()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(sampleCredentials()); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := s.Load(); !errors.Is(err, store.ErrNoCredentials) {
		t.Fatalf("Load after Delete = %v, want ErrNoCredentials", err)
	}
	if got, _ := s.HostID(); got != hostID {
		t.Fatalf("host id = %q after Delete, want the original %q", got, hostID)
	}
}
