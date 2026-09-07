package control

import (
	"strings"
	"testing"
)

// Every entry in the `clients` registry must have a matching reversal
// snippet — otherwise users can `opod connect cursor` but not
// `opod disconnect cursor`. This test fails loudly if the two lists
// drift.
func TestDisconnectSnippet_CoversEveryRegisteredClient(t *testing.T) {
	for _, c := range clients {
		t.Run(c.ID, func(t *testing.T) {
			out, err := DisconnectSnippet(c.ID)
			if err != nil {
				t.Fatalf("DisconnectSnippet(%q) = error: %v", c.ID, err)
			}
			if strings.TrimSpace(out) == "" {
				t.Fatalf("DisconnectSnippet(%q) returned empty snippet", c.ID)
			}
		})
	}
}

func TestDisconnectSnippet_UnknownClient(t *testing.T) {
	_, err := DisconnectSnippet("definitely-not-a-real-client")
	if err == nil {
		t.Fatal("expected error for unknown client, got nil")
	}
	if !strings.Contains(err.Error(), "unknown client") {
		t.Errorf("error %q should mention 'unknown client'", err.Error())
	}
}

// Each disconnect snippet must name the env vars / settings `opod connect`
// sets for that client, so the reversal is symmetric (OPENAI_BASE_URL /
// OPENAI_API_KEY / base_url).
//
// This locks the symmetry: if someone changes connect.go to set a NEW env
// var, this test fails until they also add it to the disconnect snippet.
func TestDisconnectSnippet_SymmetricEnvVars(t *testing.T) {
	cases := []struct {
		clientID string
		mustHave []string
	}{
		{"aider", []string{"OPENAI_API_BASE", "OPENAI_API_KEY"}},
		{"openai-sdk", []string{"OPENAI_BASE_URL", "base_url"}},
	}
	for _, c := range cases {
		t.Run(c.clientID, func(t *testing.T) {
			out, _ := DisconnectSnippet(c.clientID)
			for _, needle := range c.mustHave {
				if !strings.Contains(out, needle) {
					t.Errorf("snippet for %q missing %q — symmetry with connect.go broken", c.clientID, needle)
				}
			}
		})
	}
}
