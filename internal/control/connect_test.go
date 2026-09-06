package control

import (
	"strings"
	"testing"
)

func TestClients_OrderAndCompleteness(t *testing.T) {
	cs := Clients()
	if len(cs) < 10 {
		t.Fatalf("expected at least 10 clients, got %d", len(cs))
	}
	// First entry must be claude-code (it's the headline use case).
	if cs[0].ID != "claude-code" {
		t.Errorf("expected first client to be claude-code, got %q", cs[0].ID)
	}
	seen := map[string]bool{}
	for _, c := range cs {
		if c.ID == "" {
			t.Errorf("client has empty ID: %+v", c)
		}
		if c.Description == "" {
			t.Errorf("client %q has empty description", c.ID)
		}
		if c.Protocol == "" {
			t.Errorf("client %q has empty protocol", c.ID)
		}
		if seen[c.ID] {
			t.Errorf("duplicate client ID: %q", c.ID)
		}
		seen[c.ID] = true
	}
}

func TestConnectSnippet_RendersAllClients(t *testing.T) {
	for _, c := range Clients() {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			out, err := ConnectSnippet(ConnectInput{
				Client:  c.ID,
				BaseURL: "http://localhost:8080",
				Token:   "sk-orc-TEST",
				Model:   "test-model",
			})
			if err != nil {
				t.Fatalf("ConnectSnippet(%q): %v", c.ID, err)
			}
			if out.Snippet == "" {
				t.Errorf("empty snippet for %q", c.ID)
			}
			// Sanity: the rendered snippet must include the substituted
			// base URL and token. If it doesn't, the template is missing a
			// placeholder.
			if !strings.Contains(out.Snippet, "http://localhost:8080") {
				t.Errorf("snippet for %q does not contain base URL", c.ID)
			}
			if !strings.Contains(out.Snippet, "sk-orc-TEST") {
				t.Errorf("snippet for %q does not contain token", c.ID)
			}
		})
	}
}

func TestConnectSnippet_UnknownClient(t *testing.T) {
	_, err := ConnectSnippet(ConnectInput{
		Client:  "not-a-real-client",
		BaseURL: "http://localhost:8080",
		Token:   "sk-orc-TEST",
	})
	if err == nil {
		t.Fatal("expected error for unknown client, got nil")
	}
	if !strings.Contains(err.Error(), "unknown client") {
		t.Errorf("expected 'unknown client' in error, got %v", err)
	}
}

func TestConnectSnippet_DefaultModel(t *testing.T) {
	out, err := ConnectSnippet(ConnectInput{
		Client:  "claude-code",
		BaseURL: "http://localhost:8080",
		Token:   "sk-orc-TEST",
		// Model intentionally empty
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "auto" {
		t.Errorf("expected default model 'auto', got %q", out.Model)
	}
	if !strings.Contains(out.Snippet, "auto") {
		t.Errorf("snippet does not contain default model 'auto': %s", out.Snippet)
	}
}

func TestConnectSnippet_TrimsTrailingSlashFromBaseURL(t *testing.T) {
	out, err := ConnectSnippet(ConnectInput{
		Client:  "openai-sdk",
		BaseURL: "http://localhost:8080/", // trailing slash
		Token:   "sk-orc-TEST",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should appear as http://localhost:8080/v1, not http://localhost:8080//v1
	if strings.Contains(out.Snippet, "//v1") {
		t.Errorf("snippet contains '//v1' (trailing slash not trimmed): %s", out.Snippet)
	}
}

func TestConnectSnippet_MissingBaseURL(t *testing.T) {
	_, err := ConnectSnippet(ConnectInput{
		Client: "curl",
		Token:  "sk-orc-TEST",
	})
	if err == nil {
		t.Fatal("expected error for missing BaseURL")
	}
}

func TestConnectSnippet_MissingToken(t *testing.T) {
	_, err := ConnectSnippet(ConnectInput{
		Client:  "curl",
		BaseURL: "http://localhost:8080",
	})
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}
