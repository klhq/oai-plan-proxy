// Package oauth runs the Sign in with ChatGPT authorization code flow.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// Scope is the scope list for ChatGPT plan usage.
const Scope = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"

// DynamicClientID is the client_id of a first sign-in. The callback returns
// the issued client ID that replaces it.
const DynamicClientID = "dynamic_agent_client"

// CallbackPath is the loopback callback path the authorization server
// redirects to.
const CallbackPath = "/auth/callback"

// Endpoints locates the authorization server and the API the tokens are for.
type Endpoints struct {
	AuthorizeURL string
	TokenURL     string
	Issuer       string
	Resource     string
}

// OpenAI holds the production endpoints from the Sign in with ChatGPT docs.
// The issuer value is inferred from the authorization host and still needs
// confirmation against a live id_token.
var OpenAI = Endpoints{
	AuthorizeURL: "https://auth.openai.com/api/accounts/authorize",
	TokenURL:     "https://auth.openai.com/api/accounts/oauth/token",
	Issuer:       "https://auth.openai.com",
	Resource:     "https://api.openai.com/v1",
}

// Params describes one sign-in attempt.
type Params struct {
	// ClientID is the issued client ID from an earlier sign-in. Leave it
	// empty for a first, dynamic registration.
	ClientID  string
	HostID    string
	AgentName string
	Port      int
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
}

// Flow is one sign-in attempt and its one-time secrets.
type Flow struct {
	// URL is the authorization URL the user opens in a browser.
	URL string

	ep          Endpoints
	clientID    string
	now         func() time.Time
	verifier    string
	state       string
	nonce       string
	redirectURI string
}

// NewFlow prepares a sign-in attempt with fresh PKCE, state and nonce values.
func NewFlow(ep Endpoints, p Params) (*Flow, error) {
	verifier, err := randomString()
	if err != nil {
		return nil, err
	}
	state, err := randomString()
	if err != nil {
		return nil, err
	}
	nonce, err := randomString()
	if err != nil {
		return nil, err
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	f := &Flow{
		ep:          ep,
		clientID:    p.ClientID,
		now:         now,
		verifier:    verifier,
		state:       state,
		nonce:       nonce,
		redirectURI: fmt.Sprintf("http://127.0.0.1:%d%s", p.Port, CallbackPath),
	}

	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"ext_agent_host_id":     {p.HostID},
		"redirect_uri":          {f.redirectURI},
		"scope":                 {Scope},
		"resource":              {ep.Resource},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	if p.ClientID == "" {
		q.Set("client_id", DynamicClientID)
		q.Set("agent_name_hint", p.AgentName)
	} else {
		q.Set("client_id", p.ClientID)
	}
	f.URL = ep.AuthorizeURL + "?" + q.Encode()
	return f, nil
}

// Result is a completed sign-in.
type Result struct {
	ClientID     string
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
	Subject      string
	Email        string
}

// Exchange validates the callback query and trades its code for tokens.
func (f *Flow) Exchange(ctx context.Context, callback url.Values, client *http.Client) (Result, error) {
	if subtle.ConstantTimeCompare([]byte(callback.Get("state")), []byte(f.state)) != 1 {
		return Result{}, errors.New("sign-in callback has an unexpected state; start the sign-in again")
	}
	if oauthErr := callback.Get("error"); oauthErr != "" {
		return Result{}, fmt.Errorf("sign-in was not completed: %s", oauthErr)
	}
	clientID := f.clientID
	if clientID == "" {
		clientID = callback.Get("client_id")
		if clientID == "" || clientID == DynamicClientID {
			return Result{}, errors.New("registration incomplete: the sign-in callback carried no issued client_id")
		}
	}

	tok, err := postToken(ctx, client, f.ep.TokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {callback.Get("code")},
		"code_verifier": {f.verifier},
		"redirect_uri":  {f.redirectURI},
		"resource":      {f.ep.Resource},
	})
	if err != nil {
		return Result{}, err
	}
	claims, err := parseIDToken(tok.IDToken)
	if err != nil {
		return Result{}, err
	}
	if err := f.checkClaims(claims, clientID); err != nil {
		return Result{}, err
	}
	return Result{
		ClientID:     clientID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		IDToken:      tok.IDToken,
		ExpiresAt:    f.now().Add(time.Duration(tok.ExpiresIn) * time.Second),
		Subject:      claims.Subject,
		Email:        claims.Email,
	}, nil
}

// checkClaims applies the id_token checks the sign-in docs require: issuer,
// audience against the issued client ID, expiry and nonce.
func (f *Flow) checkClaims(c idClaims, clientID string) error {
	if subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(f.nonce)) != 1 {
		return errors.New("id_token nonce does not match this sign-in attempt")
	}
	if c.Issuer != f.ep.Issuer {
		return fmt.Errorf("id_token issuer %q is not %q", c.Issuer, f.ep.Issuer)
	}
	if !slices.Contains(c.Audience, clientID) {
		return fmt.Errorf("id_token audience %v does not include the issued client ID", []string(c.Audience))
	}
	if !f.now().Before(time.Unix(c.Expiry, 0)) {
		return errors.New("id_token has expired")
	}
	return nil
}

// randomString returns 32 random bytes as unpadded base64url, which is 43
// characters and valid as a PKCE verifier.
func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
