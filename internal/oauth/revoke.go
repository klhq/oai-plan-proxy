package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Revoke revokes a refresh token at the revocation endpoint named by the
// discovery document. The endpoint answers an empty 200 on success.
func Revoke(ctx context.Context, client *http.Client, ep Endpoints, clientID, refreshToken string) error {
	endpoint, err := revocationEndpoint(ctx, client, ep.DiscoveryURL)
	if err != nil {
		return err
	}
	form := url.Values{
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {clientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build revocation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("revocation request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return fmt.Errorf("revocation endpoint answered %d %s", resp.StatusCode, e.Code)
	}
	return nil
}

// revocationEndpoint reads revocation_endpoint from the discovery document.
func revocationEndpoint(ctx context.Context, client *http.Client, discoveryURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", fmt.Errorf("build discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovery request: %w", err)
	}
	defer resp.Body.Close()
	var doc struct {
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponse)).Decode(&doc); err != nil {
		return "", fmt.Errorf("parse discovery document: %w", err)
	}
	return doc.RevocationEndpoint, nil
}
