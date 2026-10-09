package memory

import (
	"strings"
	"testing"
)

func TestWithThreadPreservesCanonicalAndConflicts(t *testing.T) {
	c := Context{MemoryMD: "canonical", MemoryVersion: 4, ActiveThreadRef: "old", CurrentConstraints: []Fact{{Field: "do_not_call", Value: "true"}, {Field: "product", Value: "old"}}, Requirements: []Work{{ThreadRef: "a", Known: []Fact{{Field: "product", Value: "steel", Status: "conflict"}, {Field: "product", Value: "copper", Status: "conflict"}, {Field: "quantity", Value: "200", Status: "source_recorded"}}}}}
	v, err := WithThread(c, "a")
	if err != nil {
		t.Fatal(err)
	}
	if v.MemoryMD != c.MemoryMD || v.MemoryVersion != 4 || len(v.CurrentConstraints) != 4 || !strings.Contains(v.NextQuestion, "conflicting") {
		t.Fatal("canonical bytes, restriction or conflict lost")
	}
	if len(v.MissingFields) != 3 || v.MissingFields[0] != "product" {
		t.Fatal("conflicting values marked confirmed")
	}
	if c.CurrentConstraints[1].Value != "old" || c.ActiveThreadRef != "old" {
		t.Fatal("mutated stored context")
	}
	v, err = WithThread(c, "")
	if err != nil || len(v.CurrentConstraints) != 1 || v.ActiveThreadRef != "" {
		t.Fatal("unselected thread retained other requirement")
	}
	if _, err = WithThread(c, "other"); err == nil {
		t.Fatal("accepted foreign requirement")
	}
}
