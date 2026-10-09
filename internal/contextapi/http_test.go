package contextapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
)

type fakeStore struct {
	snapshot   memory.Snapshot
	accepted   int64
	reads      int
	sessions   map[string]Session
	capability string
}

func (f *fakeStore) Published(_ context.Context, s memory.Scope) (memory.Snapshot, int64, error) {
	f.reads++
	if s != f.snapshot.Scope {
		return memory.Snapshot{}, 0, ErrNotFound
	}
	return f.snapshot, f.accepted, nil
}
func (f *fakeStore) Create(_ context.Context, v Session) (Session, string, error) {
	id, _ := randomToken()
	capability, _ := randomToken()
	v.ID = id
	v.CapabilityHash = digest(capability)
	v.ExpiresAt = time.Now().Add(time.Hour)
	f.sessions[id] = v
	return v, capability, nil
}
func (f *fakeStore) Session(_ context.Context, id string) (Session, error) {
	v, ok := f.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return v, nil
}
func fixtureAPI(t *testing.T) (http.Handler, *fakeStore, Config) {
	t.Helper()
	scope := memory.Scope{Tenant: "local_demo", Namespace: "synthetic_demo", GLID: "900000001", Role: "buyer"}
	c := Config{DemoToken: strings.Repeat("a", 64), ToolToken: strings.Repeat("b", 64), Subjects: []memory.Scope{scope}}
	f := &fakeStore{accepted: 1, capability: strings.Repeat("c", 64), sessions: map[string]Session{}, snapshot: memory.Snapshot{Scope: scope, Version: 1, IncludedRevision: 1, MD: "# Canonical fixture\n", Hash: digest("# Canonical fixture\n"), Context: memory.Context{MemoryMD: "# Canonical fixture\n", MemoryVersion: 1, ActiveRole: "buyer", Synthetic: true, CurrentConstraints: []memory.Fact{{Field: "do_not_call", Value: "true"}}, Requirements: []memory.Work{{ThreadRef: "req:a", Known: []memory.Fact{{Field: "product", Value: "steel", Status: "source_recorded"}}}, {ThreadRef: "req:b", Known: []memory.Fact{{Field: "product", Value: "copper", Status: "source_recorded"}}}}}}}
	mux := http.NewServeMux()
	if err := Register(mux, c, f); err != nil {
		t.Fatal(err)
	}
	return mux, f, c
}
func request(h http.Handler, method, path, body, token, capability string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if capability != "" {
		r.Header.Set("X-Auralink-Session-Token", capability)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type created struct {
	Session Session  `json:"session"`
	Token   string   `json:"session_tool_token"`
	Context Envelope `json:"context"`
}

func create(t *testing.T, h http.Handler, c Config, body string) created {
	t.Helper()
	w := request(h, "POST", "/v1/sessions", body, c.DemoToken, "")
	if w.Code != 201 {
		t.Fatalf("create status %d", w.Code)
	}
	var result created
	if json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("invalid created session")
	}
	return result
}

func TestAuthorizationAndBodyBoundaries(t *testing.T) {
	h, f, c := fixtureAPI(t)
	for _, tc := range []struct {
		method, path, body, token string
		code                      int
	}{
		{"GET", "/v1/demo/subjects", "", "", 401},
		{"GET", "/v1/demo/subjects", "", c.ToolToken, 401},
		{"GET", "/v1/users/999/memory?role=buyer", "", c.DemoToken, 404},
		{"GET", "/v1/users/900000001/memory?role=seller", "", c.DemoToken, 404},
		{"GET", "/v1/users/900000001/memory?role=buyer&namespace=historical_dataset", "", c.DemoToken, 400},
		{"POST", "/v1/sessions", `{"glid":"900000001","role":"buyer","channel":"voice","namespace":"x"}`, c.DemoToken, 400},
		{"POST", "/v1/sessions", `{"glid":"900000001","role":"buyer","channel":"import"}`, c.DemoToken, 400},
		{"POST", "/v1/sessions", `{} {}`, c.DemoToken, 400},
		{"POST", "/v1/sessions", `{"glid":"` + strings.Repeat("x", 17000) + `"}`, c.DemoToken, 400},
	} {
		w := request(h, tc.method, tc.path, tc.body, tc.token, "")
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d want %d", tc.method, tc.path, w.Code, tc.code)
		}
	}
	if f.reads != 0 {
		t.Fatal("unauthorized or malformed selections touched published memory")
	}
}

func TestCrossChannelSessionsAndCapabilities(t *testing.T) {
	h, f, c := fixtureAPI(t)
	voice := create(t, h, c, `{"glid":"900000001","role":"buyer","channel":"voice","active_thread_ref":"req:a"}`)
	chat := create(t, h, c, `{"channel":"chat","resumed_from_id":"`+voice.Session.ID+`"}`)
	if chat.Session.Thread != "req:a" || chat.Session.Scope != voice.Session.Scope || chat.Context.Hash != voice.Context.Hash || chat.Context.Variables.MemoryMD != f.snapshot.MD {
		t.Fatal("cross-channel resume changed canonical identity, thread or bytes")
	}
	if chat.Context.Variables.ActiveRole != "buyer" || chat.Context.Variables.ActiveThreadRef != "req:a" {
		t.Fatal("shared agent variables changed")
	}
	for _, fact := range chat.Context.Variables.CurrentConstraints {
		if fact.Value == "copper" {
			t.Fatal("other thread leaked into current constraints")
		}
	}
	if len(chat.Context.Variables.CurrentConstraints) != 2 {
		t.Fatal("lost contact restriction or requirement")
	}
	w := request(h, "POST", "/v1/sessions", `{"channel":"chat","resumed_from_id":"`+voice.Session.ID+`","active_thread_ref":"req:b"}`, c.DemoToken, "")
	if w.Code != 400 {
		t.Fatal("resume allowed thread substitution")
	}
	body := `{"session_id":"` + voice.Session.ID + `"}`
	w = request(h, "POST", "/v1/sarvam/on-start", `{"session_id":"`+chat.Session.ID+`"}`, c.ToolToken, voice.Token)
	if w.Code != 404 {
		t.Fatal("tool capability from another session was accepted")
	}
	for _, tc := range []struct {
		token, capability string
		code              int
	}{{c.ToolToken, "", 404}, {c.DemoToken, voice.Token, 401}, {c.ToolToken, strings.Repeat("d", 64), 404}, {c.ToolToken, voice.Token, 200}} {
		w = request(h, "POST", "/v1/sarvam/tools/get_thread_context", body, tc.token, tc.capability)
		if w.Code != tc.code {
			t.Fatalf("capability check got %d want %d", w.Code, tc.code)
		}
	}
	w = request(h, "POST", "/v1/sarvam/on-start", body, c.ToolToken, voice.Token)
	if w.Code != 200 {
		t.Fatal("internal on-start failed")
	}
	var result Envelope
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result.Hash != f.snapshot.Hash || result.Variables.MemoryMD != f.snapshot.MD {
		t.Fatal("agent and executive use different memory")
	}
	w = request(h, "POST", "/v1/sarvam/tools/get_missing_fields", body, c.ToolToken, voice.Token)
	if w.Code != 200 || strings.Contains(w.Body.String(), "memory_md") {
		t.Fatal("missing-field tool returned unbounded context")
	}
	w = request(h, "POST", "/v1/sarvam/tools/get_thread_context", `{"session_id":"`+voice.Session.ID+`","glid":"999"}`, c.ToolToken, voice.Token)
	if w.Code != 400 {
		t.Fatal("tool accepted scope override")
	}
	v := f.sessions[voice.Session.ID]
	v.Principal = "someone-else"
	f.sessions[v.ID] = v
	w = request(h, "GET", "/v1/sessions/"+v.ID+"/checkpoint", "", c.DemoToken, "")
	if w.Code != 404 {
		t.Fatal("principal isolation failed")
	}
	v.Principal = demoPrincipal
	v.ExpiresAt = time.Now().Add(-time.Minute)
	f.sessions[v.ID] = v
	w = request(h, "POST", "/v1/sarvam/on-start", body, c.ToolToken, voice.Token)
	if w.Code != 404 {
		t.Fatal("expired session still usable")
	}
}

func TestPendingAndRefreshedMemory(t *testing.T) {
	h, f, c := fixtureAPI(t)
	v := create(t, h, c, `{"glid":"900000001","role":"buyer","channel":"voice","active_thread_ref":"req:a"}`)
	f.accepted = 2
	w := request(h, "GET", "/v1/sessions/"+v.Session.ID+"/checkpoint", "", c.DemoToken, "")
	var result Envelope
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || !result.Processing.Pending {
		t.Fatal("executive lost freshness warning")
	}
	w = request(h, "POST", "/v1/sarvam/on-start", `{"session_id":"`+v.Session.ID+`"}`, c.ToolToken, v.Token)
	if w.Code != 503 || strings.Contains(w.Body.String(), "memory_md") {
		t.Fatal("stale agent context served")
	}
	w = request(h, "POST", "/v1/sessions", `{"glid":"900000001","role":"buyer","channel":"chat"}`, c.DemoToken, "")
	if w.Code != 503 {
		t.Fatal("started session against pending memory")
	}
	f.snapshot.Version = 2
	f.snapshot.IncludedRevision = 2
	f.snapshot.Context.MemoryVersion = 2
	w = request(h, "GET", "/v1/sessions/"+v.Session.ID+"/checkpoint", "", c.DemoToken, "")
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.Processing.Pending || !result.VersionChanged || result.InitialVersion != 1 || result.Variables.MemoryVersion != 2 {
		t.Fatal("session failed to load newer published memory")
	}
	f.snapshot.Context.Requirements = nil
	w = request(h, "POST", "/v1/sarvam/on-start", `{"session_id":"`+v.Session.ID+`"}`, c.ToolToken, v.Token)
	if w.Code != 409 {
		t.Fatal("silently switched an unsupported active thread")
	}
}

func TestThreadListIsBounded(t *testing.T) {
	h, f, c := fixtureAPI(t)
	f.snapshot.Context.Requirements = make([]memory.Work, 45)
	w := request(h, "GET", "/v1/users/900000001/threads?role=buyer", "", c.DemoToken, "")
	var v struct {
		Threads []any `json:"threads"`
		Next    int   `json:"next_offset"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if w.Code != 200 || len(v.Threads) != 20 || v.Next != 20 {
		t.Fatal("requirement list not paginated")
	}
	w = request(h, "GET", "/v1/users/900000001/threads?role=buyer&offset=-1", "", c.DemoToken, "")
	if w.Code != 400 {
		t.Fatal("accepted negative offset")
	}
}
