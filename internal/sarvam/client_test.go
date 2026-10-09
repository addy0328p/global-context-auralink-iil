package sarvam

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureConfig() Config {
	return Config{OrgID: "01a110f1-7440-7aea-8bfd-dde645fe6bd7", WorkspaceID: "01a11f36-6b52-72db-89c2-299093f797a2", Buyer: Agent{"test-buyer", 1}, Seller: Agent{"test-seller", 2}, key: "synthetic-api-key-for-tests"}
}

func TestConnectionContractAndRoleSelection(t *testing.T) {
	c, err := NewClient(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Scheme != "https" || r.URL.Host != "apps.sarvam.ai" || r.Header.Get("X-API-Key") != fixtureConfig().key || r.URL.Query().Get("interaction_type") != "call" {
			t.Fatal("request differs from published SDK contract")
		}
		role := "buyer"
		version := "1"
		if calls == 2 {
			role = "seller"
			version = "2"
		}
		if !strings.HasSuffix(r.URL.Path, "/apps/test-"+role+"/url") || r.URL.Query().Get("version") != version {
			t.Fatal("agent role or committed version changed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"url":"wss://runtime.sarvam.ai/ws?signature=synthetic","reference_id":"synthetic-ref"}`)), Header: http.Header{}}, nil
	})
	for _, role := range []string{"buyer", "seller"} {
		v, err := c.ConnectionURL(context.Background(), role)
		if err != nil || v.ReferenceID != "synthetic-ref" {
			t.Fatal("valid connection response rejected")
		}
	}
	if _, err := c.ConnectionURL(context.Background(), "admin"); err == nil || calls != 2 {
		t.Fatal("unapproved agent role reached provider")
	}
}

func TestProviderFailuresNeverExposeSecrets(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, code string
	}{
		{401, `synthetic-api-key-for-tests signed-secret-token`, "api_key_rejected"},
		{403, `raw private upstream error`, "workspace_access_denied"},
		{404, `private config`, "agent_or_scope_not_found"},
		{429, `private quota`, "rate_limited"},
		{200, `{"url":"wss://attacker.example/ws?secret=signed-secret-token","reference_id":"ref"}`, "invalid_connection_url"},
		{200, `{"url":"https://runtime.sarvam.ai/ws","reference_id":"ref"}`, "invalid_connection_url"},
		{200, `{"url":"wss://runtime.sarvam.ai.attacker.example/ws","reference_id":"ref"}`, "invalid_connection_url"},
		{200, `{"url":"wss://user@runtime.sarvam.ai/ws","reference_id":"ref"}`, "invalid_connection_url"},
		{200, `{"url":"wss://runtime.sarvam.ai/ws"}`, "invalid_connection_response"},
		{200, strings.Repeat("x", 33000), "invalid_response_size"},
	} {
		c, _ := NewClient(fixtureConfig())
		c.http.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
		})
		_, err := c.ConnectionURL(context.Background(), "buyer")
		var upstream *ProviderError
		if !errors.As(err, &upstream) || upstream.Code != tc.code || upstream.Status != tc.status {
			t.Fatal("unexpected safe failure classification")
		}
		if strings.Contains(err.Error(), "synthetic-api-key-for-tests") || strings.Contains(err.Error(), "signed-secret-token") || strings.Contains(err.Error(), "private") {
			t.Fatal("provider error exposed credential, URL or raw body")
		}
	}
	c, _ := NewClient(fixtureConfig())
	calls := 0
	c.http.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": []string{"https://attacker.example/"}}}, nil
	})
	_, err := c.ConnectionURL(context.Background(), "seller")
	var upstream *ProviderError
	if !errors.As(err, &upstream) || upstream.Code != "redirect_rejected" || calls != 1 {
		t.Fatal("provider credential followed redirect")
	}
	c.http.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("raw transport error includes synthetic-api-key-for-tests")
	})
	_, err = c.ConnectionURL(context.Background(), "seller")
	if err == nil || strings.Contains(err.Error(), "synthetic-api-key-for-tests") {
		t.Fatal("transport error leaked secret")
	}
}

func TestInvalidConfigurationIsRejected(t *testing.T) {
	for _, mutate := range []func(*Config){func(c *Config) { c.key = "your_key_here" }, func(c *Config) { c.key = "secret\r\nInjected: value" }, func(c *Config) { c.Seller.AppID = "../other" }, func(c *Config) { c.Buyer.AppID = c.Seller.AppID }, func(c *Config) { c.Seller.Version = 0 }, func(c *Config) { c.OrgID = "invalid" }} {
		c := fixtureConfig()
		mutate(&c)
		if _, err := NewClient(c); err == nil {
			t.Fatal("invalid provider configuration accepted")
		}
	}
}
