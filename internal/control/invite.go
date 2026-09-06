package control

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

// InviteInput is the request to issue a user-scope token and render a
// shareable config card for the named recipient.
//
// The recipient name doubles as the token's Name and UserID field today
// (matching what `opod token create <name>` does). When OIDC arrives,
// UserID will come from the issuing admin's session instead.
type InviteInput struct {
	Name             string   // recipient name; becomes the token's Name + UserID
	BaseURL          string   // Opod base URL embedded in the share card
	QuotaDailyTokens int64    // 0 = unlimited
	Clients          []string // client IDs to include in the card; nil = all in display order
	Model            string   // model to suggest in each snippet; "" → "auto"
}

// InviteResult contains everything the caller needs to display or
// transmit the invite: the new token (shown plaintext exactly once), the
// store record, and a rendered share card per requested client.
type InviteResult struct {
	Token    string       // plaintext API key — sk-orc-…
	Record   store.APIKey // persisted store record
	BaseURL  string       // normalized (no trailing slash)
	Snippets map[string]*ConnectOutput
	// ClientsOrder is the client IDs in the order the caller asked for
	// them (so the rendered share card is deterministic and ordered).
	ClientsOrder []string
}

// Invite creates a user-scope API key labelled with the recipient's name
// (with optional daily token quota) and renders client snippets for
// every supported tool. The token is returned in plaintext exactly once;
// the caller is responsible for showing or transmitting it.
//
// Networks of trust: per the v0.4 security model, this assumes the
// caller is already authenticated (CLI: running on the leader host; HTTP
// admin endpoint will be guarded by RequireScope("admin")).
func Invite(ctx context.Context, st store.Store, in InviteInput) (*InviteResult, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("invite: name is required")
	}
	in.BaseURL = strings.TrimRight(in.BaseURL, "/")
	if in.BaseURL == "" {
		return nil, fmt.Errorf("invite: base URL is required")
	}
	if in.Model == "" {
		in.Model = "auto"
	}
	if len(in.Clients) == 0 {
		in.Clients = ClientIDs()
	}

	// Validate clients early so we don't create a token if one of the IDs
	// is bogus. Keeps the operation atomic from the caller's view.
	for _, cid := range in.Clients {
		if !isKnownClient(cid) {
			return nil, fmt.Errorf("invite: unknown client %q (see `opod connect --list`)", cid)
		}
	}

	plain, rec, err := auth.Generate(name, "user", name)
	if err != nil {
		return nil, fmt.Errorf("invite: generate key: %w", err)
	}
	rec.QuotaDailyTokens = in.QuotaDailyTokens
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		return nil, fmt.Errorf("invite: persist key: %w", err)
	}

	snippets := make(map[string]*ConnectOutput, len(in.Clients))
	for _, cid := range in.Clients {
		out, err := ConnectSnippet(ConnectInput{
			Client:  cid,
			BaseURL: in.BaseURL,
			Token:   plain,
			Model:   in.Model,
		})
		if err != nil {
			// Rolling back the token here would be surprising — the admin
			// already has the user record, and the snippet failure is
			// almost certainly a programmer bug (missing template). Return
			// what we have plus the error so the caller can decide.
			return nil, fmt.Errorf("invite: render %s snippet: %w", cid, err)
		}
		snippets[cid] = out
	}

	return &InviteResult{
		Token:        plain,
		Record:       rec,
		BaseURL:      in.BaseURL,
		Snippets:     snippets,
		ClientsOrder: append([]string{}, in.Clients...),
	}, nil
}

func isKnownClient(id string) bool {
	for _, c := range clients {
		if c.ID == id {
			return true
		}
	}
	return false
}

// MarkdownCard renders an invite result as a paste-into-Slack markdown
// document. The shape: heading, key facts (URL + token + quota +
// revoke-command), then one fenced code block per client snippet.
func (r *InviteResult) MarkdownCard() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "## Opod access for **%s**\n\n", r.Record.Name)
	fmt.Fprintf(&b, "- **Base URL:** %s\n", r.BaseURL)
	fmt.Fprintf(&b, "- **API token:** `%s`\n", r.Token)
	if r.Record.QuotaDailyTokens > 0 {
		fmt.Fprintf(&b, "- **Daily quota:** %s tokens\n", formatThousands(r.Record.QuotaDailyTokens))
	} else {
		fmt.Fprintf(&b, "- **Daily quota:** unlimited\n")
	}
	fmt.Fprintf(&b, "- **Created:** %s\n", r.Record.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Revoke later with:** `opod token revoke %s`\n\n", r.Record.ID)
	fmt.Fprintln(&b, "### Wire up your tools")
	fmt.Fprintln(&b)
	for _, cid := range r.ClientsOrder {
		snip, ok := r.Snippets[cid]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "**%s** — %s\n\n", snip.Client.ID, snip.Client.Description)
		fmt.Fprintln(&b, "```")
		// Snippets already end in a newline; trim trailing whitespace so
		// the fence sits flush.
		fmt.Fprint(&b, strings.TrimRight(snip.Snippet, "\n"))
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "```")
		fmt.Fprintln(&b)
	}
	return b.String()
}

func formatThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	// Insert commas right-to-left.
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
