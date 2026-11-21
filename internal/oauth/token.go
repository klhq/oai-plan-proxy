package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxTokenResponse bounds how much of a token endpoint response is read.
const maxTokenResponse = 1 << 20

// TokenError is an error answer from the token endpoint.
type TokenError struct {
	Status int
	// Code is the machine-readable OAuth error, such as invalid_grant.
	Code        string
	Description string
}

func (e *TokenError) Error() string {
	msg := fmt.Sprintf("token endpoint answered %d", e.Status)
	if e.Code != "" {
		msg += ": " + e.Code
	}
	if e.Description != "" {
		msg += " (" + e.Description + ")"
	}
	return msg
}

// tokenResponse is the token endpoint's JSON answer.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// Refreshed is the result of a refresh grant.
type Refreshed struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
}

// Refresh trades a refresh token for new tokens. The refresh token rotates:
// the caller must persist Refreshed.RefreshToken before using the access
// token, because the old refresh token is now spent.
func Refresh(ctx context.Context, client *http.Client, ep Endpoints, clientID, refreshToken string, now time.Time) (Refreshed, error) {
	// No scope parameter: the docs say omitting it keeps the original grant.
	tok, err := postToken(ctx, client, ep.TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"resource":      {ep.Resource},
	})
	if err != nil {
		return Refreshed{}, err
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return Refreshed{}, errors.New("refresh answer lacks an access_token or refresh_token")
	}
	return Refreshed{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		IDToken:      tok.IDToken,
		ExpiresAt:    now.Add(time.Duration(tok.ExpiresIn) * time.Second),
	}, nil
}

// postToken sends a form-encoded grant to the token endpoint.
func postToken(ctx context.Context, client *http.Client, tokenURL string, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code        string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		return tokenResponse{}, &TokenError{Status: resp.StatusCode, Code: e.Code, Description: e.Description}
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return tokenResponse{}, fmt.Errorf("parse token response: %w", err)
	}
	return tok, nil
}

// idClaims are the id_token claims the proxy reads.
type idClaims struct {
	Issuer   string   `json:"iss"`
	Audience audience `json:"aud"`
	Subject  string   `json:"sub"`
	Email    string   `json:"email"`
	Nonce    string   `json:"nonce"`
	Expiry   int64    `json:"exp"`
}

// audience accepts the aud claim as a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("aud claim is neither a string nor a list: %w", err)
	}
	*a = many
	return nil
}

// parseIDToken decodes the id_token payload without checking its signature.
// The token came straight from the token endpoint over TLS, which OpenID
// Connect Core 3.1.3.7 accepts in place of signature validation.
func parseIDToken(raw string) (idClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idClaims{}, fmt.Errorf("id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idClaims{}, fmt.Errorf("decode id_token payload: %w", err)
	}
	var c idClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return idClaims{}, fmt.Errorf("parse id_token claims: %w", err)
	}
	return c, nil
}
