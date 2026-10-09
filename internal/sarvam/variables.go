package sarvam

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
)

// StartVariables preserves Aditya's ten exact names while converting typed
// backend slots to text at the provider boundary. Canonical Markdown is copied
// byte-for-byte; a short opening never asserts unverified stock, price or dates.
func StartVariables(sessionID string, c memory.Context) (map[string]string, error) {
	if !sessionIDPattern.MatchString(sessionID) || c.MemoryVersion < 1 || c.MemoryMD == "" || (c.ActiveRole != "buyer" && c.ActiveRole != "seller") {
		return nil, errors.New("invalid session context for Sarvam bootstrap")
	}
	constraints := c.CurrentConstraints
	if constraints == nil {
		constraints = []memory.Fact{}
	}
	b, err := json.Marshal(constraints)
	if err != nil {
		return nil, errors.New("could not serialize context constraints")
	}
	opening := "How can I help with your purchasing requirement today?"
	if c.ActiveRole == "seller" {
		opening = "Which customer enquiry would you like to discuss today?"
	}
	if c.ActiveThreadRef != "" {
		opening = "I have saved context for this requirement. Let us continue from those details."
		if c.ActiveRole == "seller" {
			opening = "I have saved context for this customer enquiry. Let us continue from those details."
		}
	}
	if c.Historical {
		opening = "I have historical context available. Let us confirm what is still relevant."
	}
	if c.Synthetic {
		opening = "We are continuing a synthetic demonstration using the saved context."
	}
	if strings.EqualFold(c.PreferredLanguage, "Hindi") || c.PreferredLanguage == "hi" || c.PreferredLanguage == "hi-IN" {
		opening = "Aaj main aapki kharidari ki zaroorat mein kaise madad kar sakti hoon?"
		if c.ActiveRole == "seller" {
			opening = "Aap kis buyer enquiry par baat karna chahenge?"
		}
		if c.ActiveThreadRef != "" {
			opening = "Is enquiry ka saved context mere paas hai. Hum wahin se aage baat kar sakte hain."
		}
		if c.Historical {
			opening = "Mere paas purana context hai. Pehle confirm kar lete hain ki ab kya relevant hai."
		}
		if c.Synthetic {
			opening = "Hum saved context ke saath ek synthetic demo continue kar rahe hain."
		}
	}
	v := map[string]string{"session_id": sessionID, "active_role": c.ActiveRole, "active_thread_ref": c.ActiveThreadRef, "memory_version": strconv.FormatInt(c.MemoryVersion, 10), "memory_md": c.MemoryMD, "missing_fields": strings.Join(c.MissingFields, ", "), "next_question": c.NextQuestion, "current_constraints": string(b), "preferred_language": c.PreferredLanguage, "opening_line": opening}
	encoded, err := json.Marshal(v)
	if err != nil || len(encoded) > 32768 {
		return nil, errors.New("Sarvam start variables exceed context limit")
	}
	return v, nil
}

type InteractionConfig struct {
	OrgID              string            `json:"org_id"`
	WorkspaceID        string            `json:"workspace_id"`
	AppID              string            `json:"app_id"`
	Version            int               `json:"version"`
	UserIdentifier     string            `json:"user_identifier"`
	UserIdentifierType string            `json:"user_identifier_type"`
	InteractionType    string            `json:"interaction_type"`
	InputSampleRate    int               `json:"input_sample_rate"`
	OutputSampleRate   int               `json:"output_sample_rate"`
	AgentVariables     map[string]string `json:"agent_variables"`
}

func (c Config) Bootstrap(sessionID string, context memory.Context) (InteractionConfig, error) {
	if err := c.Validate(); err != nil {
		return InteractionConfig{}, err
	}
	agent, err := c.Agent(context.ActiveRole)
	if err != nil {
		return InteractionConfig{}, err
	}
	variables, err := StartVariables(sessionID, context)
	if err != nil {
		return InteractionConfig{}, err
	}
	return InteractionConfig{OrgID: c.OrgID, WorkspaceID: c.WorkspaceID, AppID: agent.AppID, Version: agent.Version, UserIdentifier: sessionID, UserIdentifierType: "custom", InteractionType: "call", InputSampleRate: 16000, OutputSampleRate: 16000, AgentVariables: variables}, nil
}
