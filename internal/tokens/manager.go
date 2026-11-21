// Package tokens keeps the stored sign-in usable: it hands out access tokens
// and refreshes them ahead of expiry.
package tokens

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/oauth"
	"github.com/klhq/oai-plan-proxy/internal/store"
)

// ErrSignInDead means the authorization server rejected the refresh token
// for good. Only a new `oai-plan-proxy login` recovers.
var ErrSignInDead = errors.New("sign-in is no longer valid; run `oai-plan-proxy login`")

// RefreshMargin is how long before expiry an access token is refreshed.
const RefreshMargin = 60 * time.Second

// RetryInterval is how long Run waits after a failed refresh.
const RetryInterval = 30 * time.Second

// Options configures a Manager.
type Options struct {
	Store     *store.Store
	Client    *http.Client
	Endpoints oauth.Endpoints
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// After waits like time.After. Nil means time.After.
	After func(time.Duration) <-chan time.Time
}

// Manager hands out access tokens from the stored sign-in.
type Manager struct {
	opts Options
	// refreshMu serializes refreshes in this process. A rotated refresh token
	// is single-use, so two concurrent refreshes would kill the sign-in.
	refreshMu sync.Mutex
}

// New returns a Manager for the sign-in in opts.Store.
func New(opts Options) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.After == nil {
		opts.After = time.After
	}
	return &Manager{opts: opts}
}

// AccessToken returns a usable access token, refreshing it first when it is
// within RefreshMargin of expiry.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	c, err := m.opts.Store.Load()
	if err != nil {
		return "", err
	}
	if c.RefreshToken == "" {
		return "", ErrSignInDead
	}
	if m.fresh(c) {
		return c.AccessToken, nil
	}
	return m.refreshLocked(ctx, func(c store.Credentials) bool { return !m.fresh(c) })
}

// Run refreshes the access token ahead of expiry until ctx ends, so the
// rotating refresh token stays alive while no requests arrive.
//
// After a failed attempt it waits RetryInterval. That covers a token
// endpoint outage and a missing or dead sign-in, which a later
// `oai-plan-proxy login` can repair while serve keeps running.
func (m *Manager) Run(ctx context.Context) {
	wait := m.untilRefresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.opts.After(wait):
		}
		if _, err := m.AccessToken(ctx); err != nil {
			wait = RetryInterval
			continue
		}
		wait = m.untilRefresh()
	}
}

// untilRefresh is how long to wait before the stored token needs refreshing.
func (m *Manager) untilRefresh() time.Duration {
	c, err := m.opts.Store.Load()
	if err != nil {
		return 0
	}
	return max(c.ExpiresAt.Sub(m.opts.Now())-RefreshMargin, 0)
}

// RefreshRejected handles an upstream 401 for the access token rejected. It
// refreshes when rejected is still the stored token and otherwise returns
// the newer stored token.
func (m *Manager) RefreshRejected(ctx context.Context, rejected string) (string, error) {
	return m.refreshLocked(ctx, func(c store.Credentials) bool { return c.AccessToken == rejected })
}

// refreshLocked takes the process and file locks, reloads the credentials,
// and refreshes only if needed still says so. Another caller or process may
// have refreshed while this one waited.
func (m *Manager) refreshLocked(ctx context.Context, needed func(store.Credentials) bool) (string, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	// The file lock extends the guard to other processes on this data
	// directory.
	unlock, err := m.opts.Store.Lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	c, err := m.opts.Store.Load()
	if err != nil {
		return "", err
	}
	if c.RefreshToken == "" {
		return "", ErrSignInDead
	}
	if !needed(c) {
		return c.AccessToken, nil
	}
	return m.refresh(ctx, c)
}

// permanentRefreshErrors are the refresh errors after which the refresh
// token can never work again.
var permanentRefreshErrors = map[string]bool{
	"invalid_grant":             true,
	"invalid_refresh_token":     true,
	"token_expired":             true,
	"refresh_token_expired":     true,
	"refresh_token_reused":      true,
	"refresh_token_invalidated": true,
}

func (m *Manager) fresh(c store.Credentials) bool {
	return m.opts.Now().Add(RefreshMargin).Before(c.ExpiresAt)
}

// refresh rotates the tokens in c and returns the new access token. It saves
// the rotated refresh token before returning, so a crash cannot leave the
// spent one on disk.
func (m *Manager) refresh(ctx context.Context, c store.Credentials) (string, error) {
	r, err := oauth.Refresh(ctx, m.opts.Client, m.opts.Endpoints, c.ClientID, c.RefreshToken, m.opts.Now())
	var tokenErr *oauth.TokenError
	if errors.As(err, &tokenErr) && permanentRefreshErrors[tokenErr.Code] {
		// Keep the client ID so the next login is a returning sign-in.
		dead := store.Credentials{ClientID: c.ClientID, Email: c.Email}
		if saveErr := m.opts.Store.Save(dead); saveErr != nil {
			return "", fmt.Errorf("%w (%s); also failed to record it: %v", ErrSignInDead, tokenErr.Code, saveErr)
		}
		return "", fmt.Errorf("%w (%s)", ErrSignInDead, tokenErr.Code)
	}
	if err != nil {
		return "", fmt.Errorf("refresh access token: %w", err)
	}
	c.AccessToken = r.AccessToken
	c.RefreshToken = r.RefreshToken
	c.ExpiresAt = r.ExpiresAt
	if r.IDToken != "" {
		c.IDToken = r.IDToken
	}
	if err := m.opts.Store.Save(c); err != nil {
		return "", fmt.Errorf("save refreshed tokens: %w", err)
	}
	return c.AccessToken, nil
}
