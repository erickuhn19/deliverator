package cmd

import (
	"reflect"
	"testing"
)

func TestJsonSchemaBasics(t *testing.T) {
	type demo struct {
		A    string   `json:"a"`
		B    int      `json:"b,omitempty"`
		C    bool     `json:"-"`
		D    []string `json:"d"`
		priv int
	}
	s := jsonSchema(reflect.TypeOf(demo{}), map[reflect.Type]bool{})
	if s["type"] != "object" {
		t.Fatalf("type = %v, want object", s["type"])
	}
	props := s["properties"].(map[string]any)
	if _, ok := props["a"]; !ok {
		t.Error("missing field a")
	}
	if props["a"].(map[string]any)["type"] != "string" {
		t.Error("a should be string")
	}
	if props["b"].(map[string]any)["type"] != "number" {
		t.Error("b should be number")
	}
	if props["d"].(map[string]any)["type"] != "array" {
		t.Error("d should be array")
	}
	if _, ok := props["c"]; ok {
		t.Error(`json:"-" field must be omitted`)
	}
	if _, ok := props["priv"]; ok {
		t.Error("unexported field must be omitted")
	}
	// required = non-omitempty exported fields (a, d) but not b.
	req := map[string]bool{}
	for _, r := range s["required"].([]string) {
		req[r] = true
	}
	if !req["a"] || !req["d"] || req["b"] {
		t.Errorf("required = %v; want a,d not b", s["required"])
	}
}

func TestJsonSchemaRecursionGuard(t *testing.T) {
	type node struct {
		Val  string `json:"val"`
		Kids []node `json:"kids"`
	}
	s := jsonSchema(reflect.TypeOf(node{}), map[reflect.Type]bool{})
	kids := s["properties"].(map[string]any)["kids"].(map[string]any)
	item := kids["items"].(map[string]any)
	if item["note"] != "recursive node" {
		t.Fatalf("recursive type must be cut with a note, got %+v", item)
	}
}

func TestDescribeCommand(t *testing.T) {
	if s, ok := describeCommand("positions"); !ok || s["type"] != "array" {
		t.Fatalf("positions should describe as an array, got %v ok=%v", s, ok)
	}
	if s, ok := describeCommand("PREVIEW"); !ok || s["type"] != "object" {
		t.Fatalf("preview (case-insensitive) should describe as an object, got %v ok=%v", s, ok)
	}
	if _, ok := describeCommand("not-a-command"); ok {
		t.Fatal("an unknown command must not resolve")
	}
}

// Every registered type must reflect cleanly into an object/array schema — guards
// against a future registry entry whose type the reflector can't shape.
func TestSchemaRegistryAllResolve(t *testing.T) {
	for _, name := range describableCommands() {
		s, ok := describeCommand(name)
		if !ok {
			t.Errorf("%s: not resolvable", name)
			continue
		}
		if s["type"] != "object" && s["type"] != "array" {
			t.Errorf("%s: top-level type = %v, want object|array", name, s["type"])
		}
	}
}
