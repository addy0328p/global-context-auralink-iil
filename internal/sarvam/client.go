package sarvam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProviderError deliberately omits upstream bodies, headers, URLs and transport
// errors: these may contain credentials or signed connection capabilities.
type ProviderError struct {
	Status int
	Code   string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("Sarvam connection check: %s (HTTP %d)", e.Code, e.Status)
}

type Client struct {
	config Config
	http   *http.Client
}

func NewClient(config Config) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Client{config: config, http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// SignedConnection is a short-lived capability. Never write it to logs or
// persistent reports. Fetching it does not start an audio/WebSocket session.
type SignedConnection struct {
	URL         string `json:"url"`
	ReferenceID string `json:"reference_id"`
}

func (c *Client) ConnectionURL(ctx context.Context, role string) (SignedConnection, error) {
	agent, err := c.config.Agent(role)
	if err != nil {
		return SignedConnection{}, err
	}
	// Exact path, query and X-API-Key header from the official SDK's
	// constructUrl/getSignedUrl. No caller-supplied upstream host or path.
	path := runtimeBaseURL + "orgs/" + c.config.OrgID + "/workspaces/" + c.config.WorkspaceID + "/apps/" + agent.AppID + "/url"
	query := url.Values{"interaction_type": {"call"}, "version": {strconv.Itoa(agent.Version)}}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, path+"?"+query.Encode(), nil)
	if err != nil {
		return SignedConnection{}, errors.New("could not prepare Sarvam connection request")
	}
	r.Header.Set("X-API-Key", c.config.key)
	r.Header.Set("Accept", "application/json")
	response, err := c.http.Do(r)
	if err != nil {
		return SignedConnection{}, &ProviderError{Code: "transport_unavailable"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		code := "upstream_rejected"
		switch response.StatusCode {
		case 401:
			code = "api_key_rejected"
		case 403:
			code = "workspace_access_denied"
		case 404:
			code = "agent_or_scope_not_found"
		case 429:
			code = "rate_limited"
		case 301, 302, 303, 307, 308:
			code = "redirect_rejected"
		}
		return SignedConnection{}, &ProviderError{Status: response.StatusCode, Code: code}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 32769))
	if err != nil || len(data) > 32768 {
		return SignedConnection{}, &ProviderError{Status: 200, Code: "invalid_response_size"}
	}
	var v SignedConnection
	if json.Unmarshal(data, &v) != nil || len(v.ReferenceID) == 0 || len(v.ReferenceID) > 256 || strings.ContainsAny(v.ReferenceID, "\r\n") {
		return SignedConnection{}, &ProviderError{Status: 200, Code: "invalid_connection_response"}
	}
	parsed, err := url.Parse(v.URL)
	if err != nil || parsed.Scheme != "wss" || parsed.User != nil || parsed.Fragment != "" || parsed.Port() != "" || !strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".sarvam.ai") {
		return SignedConnection{}, &ProviderError{Status: 200, Code: "invalid_connection_url"}
	}
	return v, nil
}
