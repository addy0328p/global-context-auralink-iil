package contextapi

import (
	"net/http"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/sarvam"
)

// RegisterBootstrap prepares authorized start variables locally. It does not
// open a provider session or assert that the configured API key was accepted.
// No general SDK proxy or credential-bearing client configuration is exposed.
func RegisterBootstrap(mux *http.ServeMux, c Config, store Store, voice sarvam.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := voice.Validate(); err != nil {
		return err
	}
	a := &API{config: c, store: store}
	mux.HandleFunc("POST /v1/sessions/{id}/sarvam-bootstrap", a.demo(func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !decodeBody(w, r, &body) {
			return
		}
		v, ok := a.boundSession(r, r.PathValue("id"))
		if !ok {
			fail(w, 404, "session_unavailable")
			return
		}
		if v.Channel != "voice" {
			fail(w, 409, "voice_session_required")
			return
		}
		s, n, err := store.Published(r.Context(), v.Scope)
		if err != nil {
			fail(w, 503, "published_memory_unavailable")
			return
		}
		if n > s.IncludedRevision {
			fail(w, 503, "memory_refresh_pending")
			return
		}
		selected, err := memory.WithThread(s.Context, v.Thread)
		if err != nil {
			fail(w, 409, "selected_thread_requires_reselection")
			return
		}
		if !contextBounded(selected) {
			fail(w, 503, "selected_thread_context_exceeds_limit")
			return
		}
		config, err := voice.Bootstrap(v.ID, selected)
		if err != nil {
			fail(w, 503, "sarvam_bootstrap_unavailable")
			return
		}
		write(w, 200, map[string]any{"status": "prepared", "provider_mode": "configuration_only", "provider_connection_verified": false, "interaction_config": config, "content_sha256": s.Hash, "included_revision": s.IncludedRevision, "processing": envelope(s, n, selected).Processing, "sdk_contract_version": sarvam.SDKContractVersion})
	}))
	return nil
}
