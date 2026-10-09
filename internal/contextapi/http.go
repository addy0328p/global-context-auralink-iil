package contextapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
)

const demoPrincipal = "local-demo"

type API struct {
	config Config
	store  Store
}

type Variables struct {
	MemoryMD           string        `json:"memory_md"`
	MemoryVersion      int64         `json:"memory_version"`
	ActiveRole         string        `json:"active_role"`
	ActiveThreadRef    string        `json:"active_thread_ref"`
	MissingFields      []string      `json:"missing_fields"`
	NextQuestion       string        `json:"next_question"`
	CurrentConstraints []memory.Fact `json:"current_constraints"`
	PreferredLanguage  string        `json:"preferred_language"`
}
type Processing struct {
	Status  string `json:"status"`
	Pending bool   `json:"pending_relevant_update"`
}
type Envelope struct {
	Scope                memory.Scope      `json:"scope"`
	Variables            Variables         `json:"variables"`
	Hash                 string            `json:"content_sha256"`
	IncludedRevision     int64             `json:"included_revision"`
	AcceptedRevision     int64             `json:"latest_accepted_revision"`
	Processing           Processing        `json:"processing"`
	PublishedAt          time.Time         `json:"published_at"`
	ServedAt             time.Time         `json:"served_at"`
	Historical           bool              `json:"historical"`
	Synthetic            bool              `json:"synthetic"`
	ExtractionMode       string            `json:"extraction_mode"`
	MissingFieldsMeaning string            `json:"missing_fields_meaning"`
	SourceCoverage       []memory.Coverage `json:"source_coverage"`
	InitialVersion       int64             `json:"initial_memory_version,omitempty"`
	VersionChanged       bool              `json:"memory_version_changed"`
}

func Register(mux *http.ServeMux, c Config, s Store) error {
	if err := c.Validate(); err != nil {
		return err
	}
	a := &API{config: c, store: s}
	mux.HandleFunc("GET /v1/demo/subjects", a.demo(a.subjects))
	mux.HandleFunc("GET /v1/users/{glid}/memory", a.demo(a.userMemory))
	mux.HandleFunc("GET /v1/users/{glid}/threads", a.demo(a.threads))
	mux.HandleFunc("POST /v1/sessions", a.demo(a.createSession))
	mux.HandleFunc("GET /v1/sessions/{id}/checkpoint", a.demo(a.checkpoint))
	// Internal adapter entry points, not an assertion of Sarvam's on-start wire format.
	mux.HandleFunc("POST /v1/sarvam/on-start", a.tool(a.agentContext))
	mux.HandleFunc("POST /v1/sarvam/tools/{tool}", a.tool(a.agentContext))
	return nil
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]string{"error": code})
}
func authorized(r *http.Request, token string) bool {
	return equalSecret(r.Header.Get("Authorization"), "Bearer "+token)
}
func (a *API) demo(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, a.config.DemoToken) {
			fail(w, 401, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}
func (a *API) tool(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, a.config.ToolToken) {
			fail(w, 401, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}
func (a *API) allowed(s memory.Scope) bool {
	for _, v := range a.config.Subjects {
		if v == s {
			return true
		}
	}
	return false
}
func (a *API) subject(glid, role string) (memory.Scope, bool) {
	for _, s := range a.config.Subjects {
		if s.GLID == glid && s.Role == role {
			return s, true
		}
	}
	return memory.Scope{}, false
}
func (a *API) subjects(w http.ResponseWriter, r *http.Request) {
	write(w, 200, map[string]any{"mode": "local_demo_allowlist", "subjects": a.config.Subjects})
}
func (a *API) readSubject(w http.ResponseWriter, r *http.Request) (memory.Snapshot, int64, bool) {
	s, ok := a.subject(r.PathValue("glid"), r.URL.Query().Get("role"))
	if !ok {
		fail(w, 404, "subject_unavailable")
		return memory.Snapshot{}, 0, false
	}
	for name := range r.URL.Query() {
		if name != "role" && name != "offset" {
			fail(w, 400, "unsupported_query_parameter")
			return memory.Snapshot{}, 0, false
		}
	}
	v, accepted, err := a.store.Published(r.Context(), s)
	if err != nil {
		fail(w, 503, "published_memory_unavailable")
		return memory.Snapshot{}, 0, false
	}
	return v, accepted, true
}
func variables(c memory.Context) Variables {
	return Variables{c.MemoryMD, c.MemoryVersion, c.ActiveRole, c.ActiveThreadRef, c.MissingFields, c.NextQuestion, c.CurrentConstraints, c.PreferredLanguage}
}

func contextBounded(c memory.Context) bool {
	b, err := json.Marshal(struct {
		Variables Variables
		Coverage  []memory.Coverage
	}{variables(c), c.SourceCoverage})
	return err == nil && len(c.CurrentConstraints) <= 32 && len(b) <= 32768
}
func envelope(s memory.Snapshot, accepted int64, c memory.Context) Envelope {
	status := "published"
	if accepted > s.IncludedRevision {
		status = "pending_refresh"
	}
	return Envelope{Scope: s.Scope, Variables: variables(c), Hash: s.Hash, IncludedRevision: s.IncludedRevision, AcceptedRevision: accepted, Processing: Processing{status, accepted > s.IncludedRevision}, PublishedAt: s.PublishedAt, ServedAt: time.Now().UTC(), Historical: c.Historical, Synthetic: c.Synthetic, ExtractionMode: c.ExtractionMode, MissingFieldsMeaning: c.MissingFieldsMeaning, SourceCoverage: c.SourceCoverage}
}
func (a *API) userMemory(w http.ResponseWriter, r *http.Request) {
	s, n, ok := a.readSubject(w, r)
	if !ok {
		return
	}
	c, err := memory.WithThread(s.Context, "")
	if err != nil {
		fail(w, 503, "context_unavailable")
		return
	}
	if !contextBounded(c) {
		fail(w, 503, "published_context_exceeds_limit")
		return
	}
	write(w, 200, envelope(s, n, c))
}
func (a *API) threads(w http.ResponseWriter, r *http.Request) {
	s, n, ok := a.readSubject(w, r)
	if !ok {
		return
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		var err error
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 || offset > 10000 {
			fail(w, 400, "invalid_offset")
			return
		}
	}
	// Only brief requirement choices, never the entire companion JSON or raw events.
	type choice struct {
		Ref    string `json:"thread_ref"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	items := []choice{}
	end := min(offset+20, len(s.Context.Requirements))
	for i := offset; i < end; i++ {
		v := s.Context.Requirements[i]
		items = append(items, choice{v.ThreadRef, v.Title, v.Status})
	}
	next := 0
	if end < len(s.Context.Requirements) {
		next = end
	}
	write(w, 200, map[string]any{"scope": s.Scope, "memory_version": s.Version, "threads": items, "total": len(s.Context.Requirements), "next_offset": next, "processing": envelope(s, n, s.Context).Processing})
}
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, 415, "json_content_type_required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		fail(w, 400, "invalid_request_body")
		return false
	}
	if !errors.Is(d.Decode(&struct{}{}), io.EOF) {
		fail(w, 400, "single_json_object_required")
		return false
	}
	return true
}

type createRequest struct {
	GLID    string `json:"glid"`
	Role    string `json:"role"`
	Channel string `json:"channel"`
	Thread  string `json:"active_thread_ref"`
	Resume  string `json:"resumed_from_id"`
}

func channelValid(s string) bool { return s == "voice" || s == "chat" || s == "app" || s == "whatsapp" }
func (a *API) boundSession(r *http.Request, id string) (Session, bool) {
	if !hexToken.MatchString(id) {
		return Session{}, false
	}
	v, err := a.store.Session(r.Context(), id)
	return v, err == nil && v.Principal == demoPrincipal && a.allowed(v.Scope) && v.ExpiresAt.After(time.Now())
}
func (a *API) createSession(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !channelValid(req.Channel) || len(req.Thread) > 256 {
		fail(w, 400, "invalid_session_selection")
		return
	}
	var scope memory.Scope
	if req.Resume != "" {
		if req.GLID != "" || req.Role != "" || req.Thread != "" {
			fail(w, 400, "resume_cannot_override_subject_or_thread")
			return
		}
		previous, ok := a.boundSession(r, req.Resume)
		if !ok {
			fail(w, 404, "session_unavailable")
			return
		}
		scope = previous.Scope
		req.Thread = previous.Thread
	} else {
		var ok bool
		scope, ok = a.subject(req.GLID, req.Role)
		if !ok {
			fail(w, 404, "subject_unavailable")
			return
		}
	}
	s, n, err := a.store.Published(r.Context(), scope)
	if err != nil {
		fail(w, 503, "published_memory_unavailable")
		return
	}
	if n > s.IncludedRevision {
		fail(w, 503, "memory_refresh_pending")
		return
	}
	c, err := memory.WithThread(s.Context, req.Thread)
	if err != nil {
		fail(w, 409, "thread_not_in_published_context")
		return
	}
	if !contextBounded(c) {
		fail(w, 503, "selected_thread_context_exceeds_limit")
		return
	}
	session, capability, err := a.store.Create(r.Context(), Session{Scope: scope, Principal: demoPrincipal, Channel: req.Channel, Thread: req.Thread, InitialVersion: s.Version, ResumedFrom: req.Resume})
	if err != nil {
		fail(w, 503, "session_creation_unavailable")
		return
	}
	write(w, 201, map[string]any{"session": session, "session_tool_token": capability, "context": envelope(s, n, c)})
}
func (a *API) sessionContext(w http.ResponseWriter, r *http.Request, v Session, agent bool) {
	s, n, err := a.store.Published(r.Context(), v.Scope)
	if err != nil {
		fail(w, 503, "published_memory_unavailable")
		return
	}
	if agent && n > s.IncludedRevision {
		write(w, 503, map[string]any{"error": "memory_refresh_pending", "included_revision": s.IncludedRevision, "latest_accepted_revision": n})
		return
	}
	c, err := memory.WithThread(s.Context, v.Thread)
	if err != nil {
		fail(w, 409, "selected_thread_requires_reselection")
		return
	}
	// Reject pathological conflicting histories rather than sending an unbounded
	// tool prompt or silently dropping restrictions and conflicting values.
	if !contextBounded(c) {
		fail(w, 503, "selected_thread_context_exceeds_limit")
		return
	}
	e := envelope(s, n, c)
	e.InitialVersion = v.InitialVersion
	e.VersionChanged = s.Version != v.InitialVersion
	if r.PathValue("tool") == "get_missing_fields" {
		write(w, 200, map[string]any{"memory_version": e.Variables.MemoryVersion, "active_role": c.ActiveRole, "active_thread_ref": c.ActiveThreadRef, "missing_fields": c.MissingFields, "next_question": c.NextQuestion, "missing_fields_meaning": c.MissingFieldsMeaning, "processing": e.Processing})
		return
	}
	write(w, 200, e)
}
func (a *API) checkpoint(w http.ResponseWriter, r *http.Request) {
	v, ok := a.boundSession(r, r.PathValue("id"))
	if !ok {
		fail(w, 404, "session_unavailable")
		return
	}
	a.sessionContext(w, r, v, false)
}
func (a *API) agentContext(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("tool")
	if name != "" && name != "get_thread_context" && name != "get_missing_fields" {
		if name == "record_user_update" || name == "record_callback_request" {
			fail(w, 501, "write_tool_not_implemented")
		} else {
			fail(w, 404, "tool_unavailable")
		}
		return
	}
	var req struct {
		ID string `json:"session_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	v, ok := a.boundSession(r, req.ID)
	token := r.Header.Get("X-Auralink-Session-Token")
	if !ok || !hexToken.MatchString(token) || !equalSecret(digest(token), v.CapabilityHash) {
		fail(w, 404, "session_unavailable")
		return
	}
	a.sessionContext(w, r, v, true)
}
