package contextapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/sarvam"
)

func TestBootstrapKeepsCredentialsAndScopeOnServer(t *testing.T) {
	h, f, c := fixtureAPI(t)
	for name, value := range map[string]string{"SARVAM_ORG_ID": "01a110f1-7440-7aea-8bfd-dde645fe6bd7", "SARVAM_WORKSPACE_ID": "01a11f36-6b52-72db-89c2-299093f797a2", "SARVAM_BUYER_APP_ID": "fixture-buyer", "SARVAM_SELLER_APP_ID": "fixture-seller", "SARVAM_BUYER_AGENT_VERSION": "1", "SARVAM_SELLER_AGENT_VERSION": "2", "SARVAM_VOICE_API_KEY": "bootstrap-only-fake-key"} {
		t.Setenv(name, value)
	}
	voiceConfig, err := sarvam.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterBootstrap(h.(*http.ServeMux), c, f, voiceConfig); err != nil {
		t.Fatal(err)
	}
	v := create(t, h, c, `{"glid":"900000001","role":"buyer","channel":"voice","active_thread_ref":"req:a"}`)
	path := "/v1/sessions/" + v.Session.ID + "/sarvam-bootstrap"
	w := request(h, "POST", path, `{}`, c.DemoToken, "")
	var got struct {
		Config   sarvam.InteractionConfig `json:"interaction_config"`
		Hash     string                   `json:"content_sha256"`
		Verified bool                     `json:"provider_connection_verified"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Config.AppID != "fixture-buyer" || got.Config.Version != 1 || got.Hash != f.snapshot.Hash || got.Config.AgentVariables["memory_md"] != f.snapshot.MD || got.Config.AgentVariables["active_thread_ref"] != "req:a" || got.Verified {
		t.Fatal("local bootstrap changed session context or claimed live verification")
	}
	if strings.Contains(w.Body.String(), "bootstrap-only-fake-key") {
		t.Fatal("provider credential exposed in HTTP response")
	}
	for _, tc := range []struct {
		body, token string
		status      int
	}{{`{}`, c.ToolToken, 401}, {`{"app_id":"other-agent"}`, c.DemoToken, 400}, {`{"agent_variables":{"memory_md":"injected"}}`, c.DemoToken, 400}} {
		w = request(h, "POST", path, tc.body, tc.token, "")
		if w.Code != tc.status {
			t.Fatal("bootstrap accepted unauthorized configuration")
		}
	}
	f.accepted = 2
	w = request(h, "POST", path, `{}`, c.DemoToken, "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "memory_md") {
		t.Fatal("bootstrap exposed stale memory")
	}
	f.accepted = 1
	chat := create(t, h, c, `{"channel":"chat","resumed_from_id":"`+v.Session.ID+`"}`)
	w = request(h, "POST", "/v1/sessions/"+chat.Session.ID+"/sarvam-bootstrap", `{}`, c.DemoToken, "")
	if w.Code != 409 {
		t.Fatal("chat session requested voice bootstrap")
	}
}
