package sarvam

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
)

func TestStartVariablesPreserveExactCanonicalBytes(t *testing.T) {
	c := memory.Context{MemoryMD: "# Exact canonical memory\nहिंदी\n", MemoryVersion: 7, ActiveRole: "seller", ActiveThreadRef: "req:one", Historical: true, MissingFields: []string{"quantity", "unit"}, NextQuestion: "Which quantity?", CurrentConstraints: []memory.Fact{{Field: "product", Value: "MCB", Status: "conflict"}}}
	config, err := fixtureConfig().Bootstrap(strings.Repeat("a", 64), c)
	if err != nil {
		t.Fatal(err)
	}
	v := config.AgentVariables
	if len(v) != 10 || v["memory_md"] != c.MemoryMD || v["memory_version"] != "7" || v["active_thread_ref"] != "req:one" || v["missing_fields"] != "quantity, unit" || v["preferred_language"] != "" {
		t.Fatal("provider variable mapping changed canonical bytes or invented a preference")
	}
	if config.AppID != "test-seller" || config.Version != 2 || config.UserIdentifier != v["session_id"] {
		t.Fatal("bootstrap lost role, version or session binding")
	}
	var facts []memory.Fact
	if json.Unmarshal([]byte(v["current_constraints"]), &facts) != nil || len(facts) != 1 || facts[0].Status != "conflict" {
		t.Fatal("structured constraints lost in string mapping")
	}
	if !strings.Contains(v["opening_line"], "historical") || strings.Contains(v["opening_line"], "180") {
		t.Fatal("opening misstated freshness or price")
	}
	b, _ := json.Marshal(config)
	if strings.Contains(string(b), fixtureConfig().key) || strings.Contains(string(b), "api_key") {
		t.Fatal("bootstrap exposed provider key")
	}
	c.ActiveRole = "buyer"
	c.Historical = false
	c.Synthetic = true
	config, err = fixtureConfig().Bootstrap(strings.Repeat("a", 64), c)
	if err != nil || config.AppID != "test-buyer" || config.Version != 1 || !strings.Contains(config.AgentVariables["opening_line"], "synthetic") {
		t.Fatal("buyer bootstrap used seller configuration or mislabeled fixture")
	}
	if _, err = StartVariables(strings.Repeat("x", 64), c); err == nil {
		t.Fatal("malformed session identity accepted")
	}
	c.MemoryMD = strings.Repeat("x", 33000)
	if _, err = StartVariables(strings.Repeat("a", 64), c); err == nil {
		t.Fatal("unbounded start variables accepted")
	}
}
