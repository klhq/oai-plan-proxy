package tokens_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/oauth"
	"github.com/klhq/oai-plan-proxy/internal/store"
	"github.com/klhq/oai-plan-proxy/internal/tokens"
)

var start = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// clock is a settable time source shared by the manager and the test.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// authServer is a fake token endpoint that rotates refresh tokens.
type authServer struct {
	*httptest.Server
	requests atomic.Int32
	// respond, when set, overrides the default rotating answer.
	respond func(w http.ResponseWriter, r *http.Request)
}

func newAuthServer(t *testing.T) *authServer {
	t.Helper()
	a := &authServer{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := a.requests.Add(1)
		if a.respond != nil {
			a.respond(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":"refresh-%d","token_type":"Bearer","expires_in":3600}`, n+1, n+1)
	}))
	t.Cleanup(a.Close)
	return a
}

type fixture struct {
	store *store.Store
	auth  *authServer
	clock *clock
	mgr   *tokens.Manager
}

// newFixture stores a sign-in whose access token expires at expiresAt.
func newFixture(t *testing.T, expiresAt time.Time) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(store.Credentials{
		ClientID: "oaiapp_issued", AccessToken: "access-1", RefreshToken: "refresh-1",
		ExpiresAt: expiresAt, Email: "user@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: st, auth: newAuthServer(t), clock: &clock{now: start}}
	f.mgr = f.newManager()
	return f
}

// newManager builds another manager on the same data directory, as a second
// process would.
func (f *fixture) newManager() *tokens.Manager {
	return tokens.New(tokens.Options{
		Store:  f.store,
		Client: f.auth.Client(),
		Endpoints: oauth.Endpoints{
			TokenURL: f.auth.URL + "/token",
			Resource: "https://api.example.test/v1",
		},
		Now: f.clock.Now,
	})
}

func TestATokenWithinTheRefreshMarginIsRefreshedAndPersisted(t *testing.T) {
	f := newFixture(t, start.Add(59*time.Second))

	tok, err := f.mgr.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if tok != "access-2" {
		t.Fatalf("token = %q, want the refreshed access-2", tok)
	}
	saved, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "refresh-2" || saved.AccessToken != "access-2" {
		t.Fatalf("stored tokens = %q/%q, want the rotated access-2/refresh-2", saved.AccessToken, saved.RefreshToken)
	}
	if saved.ClientID != "oaiapp_issued" || saved.Email != "user@example.com" {
		t.Fatalf("refresh lost the client ID or email: %+v", saved)
	}
}

func TestConcurrentCallersWithAnExpiredTokenShareOneRefresh(t *testing.T) {
	f := newFixture(t, start.Add(-time.Minute))
	// Slow the token endpoint so the callers overlap.
	f.auth.respond = func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-2","token_type":"Bearer","expires_in":3600}`))
	}
	const callers = 50
	got := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup

	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = f.mgr.AccessToken(context.Background())
		}()
	}
	wg.Wait()

	if n := f.auth.requests.Load(); n != 1 {
		t.Fatalf("refresh requests = %d, want exactly 1", n)
	}
	for i := range callers {
		if errs[i] != nil || got[i] != "access-2" {
			t.Fatalf("caller %d got %q, %v; want access-2", i, got[i], errs[i])
		}
	}
}

func TestTwoProcessesOnOneDataDirectoryShareOneRefresh(t *testing.T) {
	f := newFixture(t, start.Add(-time.Minute))
	f.auth.respond = func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-2","token_type":"Bearer","expires_in":3600}`))
	}
	other := f.newManager()
	var wg sync.WaitGroup

	for _, m := range []*tokens.Manager{f.mgr, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.AccessToken(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if n := f.auth.requests.Load(); n != 1 {
		t.Fatalf("refresh requests = %d, want exactly 1", n)
	}
}

func TestARefreshThatCannotBeSavedIsNotUsed(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, start.Add(-time.Minute))
	// Move the fixture onto a directory we can make read-only. Take the lock
	// once so the lock file exists before the directory is locked down.
	creds, _ := f.store.Load()
	if err := st.Save(creds); err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	f.store = st
	f.mgr = f.newManager()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	tok, err := f.mgr.AccessToken(context.Background())

	if err == nil || tok != "" {
		t.Fatalf("AccessToken = %q, %v; want no token and an error when the rotation cannot be saved", tok, err)
	}
}

func TestAPermanentlyRejectedRefreshMarksTheSignInDead(t *testing.T) {
	// The six codes the errors-and-recovery page lists for an unusable
	// refresh token.
	for _, code := range []string{
		"invalid_grant", "invalid_refresh_token", "token_expired",
		"refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated",
	} {
		t.Run(code, func(t *testing.T) {
			f := newFixture(t, start.Add(-time.Minute))
			f.auth.respond = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q}`, code)
			}

			_, err := f.mgr.AccessToken(context.Background())
			_, again := f.mgr.AccessToken(context.Background())

			if !errors.Is(err, tokens.ErrSignInDead) || !errors.Is(again, tokens.ErrSignInDead) {
				t.Fatalf("errors = %v / %v, want ErrSignInDead both times", err, again)
			}
			if n := f.auth.requests.Load(); n != 1 {
				t.Fatalf("refresh requests = %d, want 1: a dead sign-in must not be retried", n)
			}
			saved, loadErr := f.store.Load()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if saved.AccessToken != "" || saved.RefreshToken != "" || saved.ClientID != "oaiapp_issued" {
				t.Fatalf("stored %+v, want tokens cleared and the client ID kept", saved)
			}
			if f.store.State() != auth.StateDead {
				t.Fatalf("state = %q, want dead", f.store.State())
			}
		})
	}
}

func TestATransientRefreshFailureKeepsTheSignIn(t *testing.T) {
	cases := map[string]func(*fixture){
		"server error": func(f *fixture) {
			f.auth.respond = func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		},
		"network error": func(f *fixture) { f.auth.Close() },
	}
	for name, breakAuth := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, start.Add(-time.Minute))
			breakAuth(f)

			_, err := f.mgr.AccessToken(context.Background())

			if err == nil || errors.Is(err, tokens.ErrSignInDead) {
				t.Fatalf("err = %v, want a transient error", err)
			}
			saved, loadErr := f.store.Load()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if saved.RefreshToken != "refresh-1" || f.store.State() != auth.StateValid {
				t.Fatalf("stored refresh token %q, state %q; want refresh-1 and valid", saved.RefreshToken, f.store.State())
			}
		})
	}
}

func TestARejectedTokenIsRefreshedOnceEvenBeforeExpiry(t *testing.T) {
	f := newFixture(t, start.Add(time.Hour))

	first, err := f.mgr.RefreshRejected(context.Background(), "access-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.mgr.RefreshRejected(context.Background(), "access-1")
	if err != nil {
		t.Fatal(err)
	}

	if first != "access-2" || second != "access-2" {
		t.Fatalf("tokens = %q, %q; want access-2 both times", first, second)
	}
	if n := f.auth.requests.Load(); n != 1 {
		t.Fatalf("refresh requests = %d, want 1: the second rejection names a token already replaced", n)
	}
}

// timer lets a test see each wait Run asks for and decide when it ends.
type timer struct {
	waits chan time.Duration
	fire  chan time.Time
}

func newTimer() *timer {
	return &timer{waits: make(chan time.Duration), fire: make(chan time.Time)}
}

func (tm *timer) After(d time.Duration) <-chan time.Time {
	tm.waits <- d
	return tm.fire
}

func (tm *timer) nextWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-tm.waits:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("Run never scheduled a wait")
		return 0
	}
}

func TestRunRefreshesAheadOfExpiryWithoutTraffic(t *testing.T) {
	f := newFixture(t, start.Add(time.Hour))
	tm := newTimer()
	f.mgr = tokens.New(tokens.Options{
		Store: f.store, Client: f.auth.Client(),
		Endpoints: oauth.Endpoints{TokenURL: f.auth.URL + "/token", Resource: "https://api.example.test/v1"},
		Now:       f.clock.Now, After: tm.After,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.mgr.Run(ctx)

	if d := tm.nextWait(t); d != time.Hour-tokens.RefreshMargin {
		t.Fatalf("first wait = %v, want %v", d, time.Hour-tokens.RefreshMargin)
	}
	f.clock.Set(start.Add(time.Hour - tokens.RefreshMargin))
	tm.fire <- f.clock.Now()
	next := tm.nextWait(t)

	if n := f.auth.requests.Load(); n != 1 {
		t.Fatalf("refresh requests = %d, want 1", n)
	}
	saved, _ := f.store.Load()
	if saved.RefreshToken != "refresh-2" {
		t.Fatalf("stored refresh token = %q, want refresh-2", saved.RefreshToken)
	}
	if next != time.Hour-tokens.RefreshMargin {
		t.Fatalf("wait after refresh = %v, want %v until the new token nears expiry", next, time.Hour-tokens.RefreshMargin)
	}
}

func TestRunBacksOffAfterAFailedRefresh(t *testing.T) {
	f := newFixture(t, start.Add(-time.Minute))
	f.auth.respond = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	tm := newTimer()
	f.mgr = tokens.New(tokens.Options{
		Store: f.store, Client: f.auth.Client(),
		Endpoints: oauth.Endpoints{TokenURL: f.auth.URL + "/token", Resource: "https://api.example.test/v1"},
		Now:       f.clock.Now, After: tm.After,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.mgr.Run(ctx)

	tm.nextWait(t)
	tm.fire <- f.clock.Now()
	retry := tm.nextWait(t)

	if retry != tokens.RetryInterval {
		t.Fatalf("wait after a failed refresh = %v, want RetryInterval (%v)", retry, tokens.RetryInterval)
	}
}

func TestAFreshAccessTokenIsUsedWithoutRefreshing(t *testing.T) {
	f := newFixture(t, start.Add(time.Hour))

	tok, err := f.mgr.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if tok != "access-1" {
		t.Fatalf("token = %q, want access-1", tok)
	}
	if n := f.auth.requests.Load(); n != 0 {
		t.Fatalf("refresh requests = %d, want 0", n)
	}
}
