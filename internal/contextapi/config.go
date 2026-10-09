// Package contextapi exposes the application's private context contract. These
// routes are not a claimed Sarvam webhook format; a provider adapter comes later.
package contextapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
)

type Config struct {
	DemoToken string
	ToolToken string
	Subjects  []memory.Scope
}

var hexToken = regexp.MustCompile(`^[0-9a-f]{64}$`)
var digits = regexp.MustCompile(`^[0-9]{1,30}$`)

func ReadSubjects(path string) ([]memory.Scope, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("could not open private demo subject manifest")
	}
	defer f.Close()
	var manifest struct {
		Subjects []memory.Candidate `json:"subjects"`
	}
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	if d.Decode(&manifest) != nil {
		return nil, errors.New("invalid private demo subject manifest")
	}
	var scopes []memory.Scope
	for _, c := range manifest.Subjects {
		scopes = append(scopes, c.Scope)
	}
	if err := validateSubjects(scopes); err != nil {
		return nil, err
	}
	return scopes, nil
}

func validateSubjects(scopes []memory.Scope) error {
	if len(scopes) == 0 || len(scopes) > 20 {
		return errors.New("demo manifest must allow 1 to 20 subject roles")
	}
	seen := map[memory.Scope]bool{}
	identities := map[string]bool{}
	for _, s := range scopes {
		key := s.GLID + ":" + s.Role
		if s.Tenant != "local_demo" || (s.Namespace != "historical_dataset" && s.Namespace != "synthetic_demo") || !digits.MatchString(s.GLID) || (s.Role != "buyer" && s.Role != "seller") || seen[s] || identities[key] {
			return errors.New("invalid or repeated demo subject scope")
		}
		seen[s] = true
		identities[key] = true
	}
	return nil
}

// LoadConfig fails closed if only part of the private API configuration exists.
// With neither token set, the server retains health endpoints only.
func LoadConfig() (*Config, error) {
	a, b := os.Getenv("DEMO_API_BEARER_TOKEN"), os.Getenv("SARVAM_TOOL_BEARER_TOKEN")
	if a == "" && b == "" {
		return nil, nil
	}
	path := os.Getenv("DEMO_SUBJECT_MANIFEST")
	if path == "" {
		path = "artifacts/exports/historical_dataset/manifest.json"
	}
	subjects, err := ReadSubjects(path)
	if err != nil {
		return nil, err
	}
	c := &Config{DemoToken: a, ToolToken: b, Subjects: subjects}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !hexToken.MatchString(c.DemoToken) || !hexToken.MatchString(c.ToolToken) || equalSecret(c.DemoToken, c.ToolToken) {
		return errors.New("private API requires two distinct 64-character random hexadecimal bearer tokens; run go run ./cmd/apiconfig")
	}
	return validateSubjects(c.Subjects)
}

func equalSecret(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
