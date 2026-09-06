package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStripModelField_RemovesTopLevelModel(t *testing.T) {
	in := []byte(`{"model":"anthropic.claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	out, err := stripModelField(in)
	if err != nil {
		t.Fatalf("stripModelField: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, has := got["model"]; has {
		t.Errorf("model field still present: %s", out)
	}
	if _, has := got["messages"]; !has {
		t.Errorf("messages field stripped accidentally: %s", out)
	}
}

func TestStripModelField_NoopWhenModelAbsent(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, err := stripModelField(in)
	if err != nil {
		t.Fatalf("stripModelField: %v", err)
	}
	if !strings.Contains(string(out), `"messages"`) {
		t.Errorf("messages field dropped: %s", out)
	}
}

func TestStripModelField_RejectsNonJSON(t *testing.T) {
	if _, err := stripModelField([]byte("not json")); err == nil {
		t.Error("expected error on garbage input, got nil")
	}
}

func TestEnsureAnthropicVersion_AddsWhenAbsent(t *testing.T) {
	in := []byte(`{"messages":[]}`)
	out := ensureAnthropicVersion(in)
	if !strings.Contains(string(out), `"anthropic_version":"bedrock-2023-05-31"`) {
		t.Errorf("anthropic_version not added: %s", out)
	}
}

func TestEnsureAnthropicVersion_PreservesExisting(t *testing.T) {
	in := []byte(`{"anthropic_version":"custom","messages":[]}`)
	out := ensureAnthropicVersion(in)
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["anthropic_version"] != "custom" {
		t.Errorf("anthropic_version overwritten: %s", out)
	}
}

func TestEnsureAnthropicVersion_NoopOnBadJSON(t *testing.T) {
	out := ensureAnthropicVersion([]byte("not json"))
	if string(out) != "not json" {
		t.Errorf("non-JSON should pass through, got: %s", out)
	}
}

func TestVertexEndpoint_BuildsCorrectURL(t *testing.T) {
	got := vertexEndpoint("us-east5", "my-project", "gemini-1.5-pro")
	want := "https://us-east5-aiplatform.googleapis.com/v1/projects/my-project/locations/us-east5/publishers/google/models/gemini-1.5-pro:generateContent"
	if got != want {
		t.Errorf("URL mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestVertexEndpoint_DefaultLocation(t *testing.T) {
	got := vertexEndpoint("", "my-project", "gemini-1.5-pro")
	if !strings.Contains(got, "us-central1") {
		t.Errorf("empty location should default to us-central1, got: %s", got)
	}
}
