package oauth_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/oauth"
)

func TestRefreshRejectsAnAnswerWithoutBothTokens(t *testing.T) {
	tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-2","token_type":"Bearer","expires_in":3600}`))
	})

	_, err := oauth.Refresh(context.Background(), tokens.Client(), tokens.endpoints(),
		"oaiapp_issued", "refresh-1", fixedNow)

	if err == nil {
		t.Fatal("Refresh accepted an answer with no refresh_token")
	}
}

func TestRefreshSendsTheRotatingGrantWithoutScope(t *testing.T) {
	var form url.Values
	tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-2","id_token":"x.y.z","token_type":"Bearer","expires_in":3600}`))
	})

	got, err := oauth.Refresh(context.Background(), tokens.Client(), tokens.endpoints(),
		"oaiapp_issued", "refresh-1", fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	want := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {"oaiapp_issued"},
		"refresh_token": {"refresh-1"},
		"resource":      {testEndpoints.Resource},
	}
	if form.Encode() != want.Encode() {
		t.Errorf("form = %v, want exactly %v", form, want)
	}
	if got.AccessToken != "access-2" || got.RefreshToken != "refresh-2" || !got.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("result = %+v, want access-2, refresh-2, expiry one hour after now", got)
	}
}
