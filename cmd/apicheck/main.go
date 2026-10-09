// apicheck verifies the local backend contract. Channel labels here exercise
// context continuity; this command does not make a Sarvam call or generate audio.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/contextapi"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
)

type created struct {
	Session contextapi.Session  `json:"session"`
	Token   string              `json:"session_tool_token"`
	Context contextapi.Envelope `json:"context"`
}
type client struct {
	base   string
	http   *http.Client
	config contextapi.Config
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "loopback backend URL")
	flag.Parse()
	if err := run(*base); err != nil {
		slog.Error("local API check failed", "reason", err.Error())
		os.Exit(1)
	}
}
func run(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return errors.New("invalid backend URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("API check requires an HTTP loopback URL without extra path, query or credentials")
	}
	_ = godotenv.Load()
	config, err := contextapi.LoadConfig()
	if err != nil {
		return err
	}
	if config == nil {
		return errors.New("run go run ./cmd/apiconfig first")
	}
	c := client{base: base, http: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, config: *config}
	var status struct {
		Status string `json:"status"`
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if err := c.call("GET", path, nil, "", "", 200, &status); err != nil {
			return err
		}
	}
	if err := c.call("GET", "/v1/demo/subjects", nil, "", "", 401, nil); err != nil {
		return err
	}
	var subjects struct {
		Subjects []memory.Scope `json:"subjects"`
	}
	if err := c.call("GET", "/v1/demo/subjects", nil, config.DemoToken, "", 200, &subjects); err != nil {
		return err
	}
	if len(subjects.Subjects) != len(config.Subjects) {
		return errors.New("server and local configuration have different allowlists")
	}
	for _, scope := range subjects.Subjects {
		var canonical contextapi.Envelope
		path := "/v1/users/" + scope.GLID + "/memory?role=" + scope.Role
		if err := c.call("GET", path, nil, config.DemoToken, "", 200, &canonical); err != nil {
			return err
		}
		if canonical.Processing.Pending {
			return errors.New("sample memory has a pending refresh")
		}
		var choices struct {
			Threads []struct {
				Ref string `json:"thread_ref"`
			} `json:"threads"`
		}
		if err := c.call("GET", "/v1/users/"+scope.GLID+"/threads?role="+scope.Role, nil, config.DemoToken, "", 200, &choices); err != nil {
			return err
		}
		thread := ""
		if len(choices.Threads) > 0 {
			thread = choices.Threads[0].Ref
		}
		var voice, chat created
		if err := c.call("POST", "/v1/sessions", map[string]string{"glid": scope.GLID, "role": scope.Role, "channel": "voice", "active_thread_ref": thread}, config.DemoToken, "", 201, &voice); err != nil {
			return err
		}
		if err := c.call("POST", "/v1/sessions", map[string]string{"channel": "chat", "resumed_from_id": voice.Session.ID}, config.DemoToken, "", 201, &chat); err != nil {
			return err
		}
		for _, v := range []created{voice, chat} {
			var e contextapi.Envelope
			if err := c.call("POST", "/v1/sarvam/tools/get_thread_context", map[string]string{"session_id": v.Session.ID}, config.ToolToken, v.Token, 200, &e); err != nil {
				return err
			}
			if e.Scope != scope || e.Hash != canonical.Hash || e.Variables.MemoryMD != canonical.Variables.MemoryMD || e.Variables.MemoryVersion != canonical.Variables.MemoryVersion || e.Variables.ActiveThreadRef != thread || e.Variables.ActiveRole != scope.Role {
				return errors.New("canonical context changed across session channels")
			}
		}
	}
	slog.Info("local backend continuity verified", "subject_roles", len(subjects.Subjects), "channel_pairs", len(subjects.Subjects), "provider_integration", false)
	return nil
}

func (c client) call(method, path string, body any, token, capability string, want int, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("could not encode check request")
		}
	}
	r, err := http.NewRequest(method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("could not prepare check request")
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if capability != "" {
		r.Header.Set("X-Auralink-Session-Token", capability)
	}
	response, err := c.http.Do(r)
	if err != nil {
		return errors.New("could not reach local backend; start go run ./cmd/server first")
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		return fmt.Errorf("local %s request returned HTTP %d; expected %d", method, response.StatusCode, want)
	}
	if out == nil {
		return nil
	}
	if json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(out) != nil {
		return errors.New("could not decode local backend response")
	}
	return nil
}
