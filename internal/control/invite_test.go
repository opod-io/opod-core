package control

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/store"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestInvite_HappyPath(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	res, err := Invite(ctx, st, InviteInput{
		Name:             "hadi",
		BaseURL:          "http://localhost:8080",
		QuotaDailyTokens: 100000,
	})
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if !strings.HasPrefix(res.Token, "sk-orc-") {
		t.Errorf("expected token to start with sk-orc-, got %q", res.Token)
	}
	if res.Record.Name != "hadi" {
		t.Errorf("expected record.Name=hadi, got %q", res.Record.Name)
	}
	if res.Record.Scope != "user" {
		t.Errorf("expected scope=user, got %q", res.Record.Scope)
	}
	if res.Record.QuotaDailyTokens != 100000 {
		t.Errorf("expected quota=100000, got %d", res.Record.QuotaDailyTokens)
	}
	if len(res.Snippets) != len(Clients()) {
		t.Errorf("expected %d snippets (all clients), got %d", len(Clients()), len(res.Snippets))
	}
	// Round-trip: the token should now be in the store.
	keys, err := st.APIKeys().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 key persisted, got %d", len(keys))
	}
}

func TestInvite_ExplicitClientSubset(t *testing.T) {
	st := newTestStore(t)
	res, err := Invite(context.Background(), st, InviteInput{
		Name:    "alice",
		BaseURL: "http://opod.local:8080",
		Clients: []string{"claude-code", "cursor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snippets) != 2 {
		t.Errorf("expected 2 snippets, got %d", len(res.Snippets))
	}
	if _, ok := res.Snippets["claude-code"]; !ok {
		t.Errorf("missing claude-code snippet")
	}
	if _, ok := res.Snippets["aider"]; ok {
		t.Errorf("aider snippet should NOT be in subset result")
	}
	// Order preserved
	if res.ClientsOrder[0] != "claude-code" || res.ClientsOrder[1] != "cursor" {
		t.Errorf("ClientsOrder not preserved: %v", res.ClientsOrder)
	}
}

func TestInvite_UnknownClient(t *testing.T) {
	st := newTestStore(t)
	_, err := Invite(context.Background(), st, InviteInput{
		Name:    "bob",
		BaseURL: "http://x:8080",
		Clients: []string{"claude-code", "not-a-tool"},
	})
	if err == nil {
		t.Fatal("expected error for unknown client")
	}
	// Should NOT have created the token (validate-before-mutate).
	keys, _ := st.APIKeys().List(context.Background())
	if len(keys) != 0 {
		t.Errorf("expected no keys after validation failure, got %d", len(keys))
	}
}

func TestInvite_MissingFields(t *testing.T) {
	st := newTestStore(t)
	if _, err := Invite(context.Background(), st, InviteInput{
		BaseURL: "http://x:8080",
	}); err == nil {
		t.Error("expected error for missing name")
	}
	if _, err := Invite(context.Background(), st, InviteInput{
		Name: "alice",
	}); err == nil {
		t.Error("expected error for missing base URL")
	}
}

func TestMarkdownCard_ContainsExpectedSections(t *testing.T) {
	st := newTestStore(t)
	res, err := Invite(context.Background(), st, InviteInput{
		Name:             "hadi",
		BaseURL:          "http://opod.local:8080",
		QuotaDailyTokens: 100000,
		Clients:          []string{"claude-code", "curl"},
	})
	if err != nil {
		t.Fatal(err)
	}
	md := res.MarkdownCard()
	for _, want := range []string{
		"## Opod access for **hadi**",
		"**Base URL:** http://opod.local:8080",
		"**API token:**",
		"100,000 tokens",
		"opod token revoke",
		"**claude-code**",
		"**curl**",
		"```",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("MarkdownCard missing %q. Got:\n%s", want, md)
		}
	}
}

func TestMarkdownCard_UnlimitedQuota(t *testing.T) {
	st := newTestStore(t)
	res, err := Invite(context.Background(), st, InviteInput{
		Name:    "free",
		BaseURL: "http://x:8080",
		Clients: []string{"claude-code"},
		// QuotaDailyTokens: 0 → unlimited
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.MarkdownCard(), "unlimited") {
		t.Errorf("expected 'unlimited' in card for quota=0")
	}
}

func TestFormatThousands(t *testing.T) {
	tests := map[int64]string{
		0:        "0",
		999:      "999",
		1000:     "1,000",
		100000:   "100,000",
		1234567:  "1,234,567",
		12345678: "12,345,678",
	}
	for in, want := range tests {
		if got := formatThousands(in); got != want {
			t.Errorf("formatThousands(%d) = %q, want %q", in, got, want)
		}
	}
}
