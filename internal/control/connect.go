package control

import (
	"bytes"
	"embed"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

//go:embed snippets/*.tmpl
var snippetFS embed.FS

// Client describes one supported tool that can be wired up to Opod.
type Client struct {
	ID          string // stable identifier, kebab-case
	Protocol    string // "OpenAI" | "Anthropic" | "Both" | "Raw HTTP"
	Description string // one-line "what is this"
}

// clients is the registry of supported tools. Order here is the order
// they appear in `opod connect --list` and in invite share cards.
var clients = []Client{
	{ID: "claude-code", Protocol: "Anthropic", Description: "Anthropic's official CLI coding agent"},
	{ID: "cursor", Protocol: "OpenAI", Description: "IDE with built-in AI"},
	{ID: "aider", Protocol: "OpenAI", Description: "Terminal-based AI pair programmer"},
	{ID: "continue", Protocol: "Both", Description: "VS Code / JetBrains AI assistant"},
	{ID: "zed", Protocol: "OpenAI", Description: "Fast multiplayer code editor"},
	{ID: "cline", Protocol: "Both", Description: "VS Code AI extension (Cline / Roo Code)"},
	{ID: "qwen-code", Protocol: "Anthropic", Description: "Open-source Claude Code fork"},
	{ID: "hermes", Protocol: "Anthropic", Description: "Nous Research's CLI agent (persistent memory + auto skills)"},
	{ID: "openclaw", Protocol: "OpenAI", Description: "Open-source CLI coding agent (OpenAI-compatible)"},
	{ID: "opencode", Protocol: "Both", Description: "Open-source terminal coding agent (per-provider baseURL override)"},
	{ID: "open-webui", Protocol: "OpenAI", Description: "Self-hosted ChatGPT-style web UI (~85k★ on GitHub)"},
	{ID: "open-notebook", Protocol: "OpenAI", Description: "Open-source NotebookLM alternative (MIT, ~27k★)"},
	{ID: "goose", Protocol: "OpenAI", Description: "Block's OSS terminal agent (Apache-2.0, OpenAI-compatible)"},
	{ID: "plandex", Protocol: "OpenAI", Description: "Terminal-native agentic planner (MIT, OpenAI-compatible)"},
	{ID: "openhands", Protocol: "Both", Description: "Autonomous coding agent (formerly OpenDevin; MIT)"},
	{ID: "codex-cli", Protocol: "OpenAI", Description: "OpenAI's official CLI (supports base-URL override)"},
	{ID: "openai-sdk", Protocol: "OpenAI", Description: "OpenAI Python/JS SDK"},
	{ID: "anthropic-sdk", Protocol: "Anthropic", Description: "Anthropic Python/JS SDK"},
	{ID: "curl", Protocol: "Raw HTTP", Description: "Direct HTTP calls for testing"},
}

// Clients returns the list of supported clients in display order. Callers
// should treat the slice as read-only.
func Clients() []Client {
	out := make([]Client, len(clients))
	copy(out, clients)
	return out
}

// ConnectInput is the request to render a per-client config snippet.
type ConnectInput struct {
	Client  string // one of the IDs returned by Clients()
	BaseURL string // e.g. "http://localhost:8080" (no trailing slash)
	Token   string // Opod API key, plaintext (sk-orc-…)
	Model   string // model id to suggest in the snippet; defaults to "auto"
	// Retries, when > 0, adds an `X-Opod-Num-Retries: N` header to the
	// rendered snippet so the gateway retries each candidate up to N
	// times with exponential backoff before walking the fallback chain.
	// Surfaced in the curl template; clients that don't expose
	// per-request headers ignore it.
	Retries int
}

// ConnectOutput is the rendered snippet plus metadata about the client.
type ConnectOutput struct {
	Client  Client
	Snippet string
	BaseURL string
	Token   string
	Model   string
}

// ConnectSnippet renders the embedded template for the named client with
// the caller's base URL + token substituted in. Returns an error if the
// client is unknown or the template can't be parsed.
func ConnectSnippet(in ConnectInput) (*ConnectOutput, error) {
	in.BaseURL = strings.TrimRight(in.BaseURL, "/")
	if in.BaseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}
	if in.Token == "" {
		return nil, fmt.Errorf("token is required")
	}
	if in.Model == "" {
		in.Model = "auto"
	}

	var matched *Client
	for i := range clients {
		if clients[i].ID == in.Client {
			matched = &clients[i]
			break
		}
	}
	if matched == nil {
		return nil, fmt.Errorf("unknown client %q (run `opod connect --list` for supported clients)", in.Client)
	}

	tmplBytes, err := snippetFS.ReadFile("snippets/" + matched.ID + ".tmpl")
	if err != nil {
		return nil, fmt.Errorf("snippet template missing for %q: %w", matched.ID, err)
	}

	tmpl, err := template.New(matched.ID).Parse(string(tmplBytes))
	if err != nil {
		return nil, fmt.Errorf("snippet template parse for %q: %w", matched.ID, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		BaseURL string
		Token   string
		Model   string
		Retries int
	}{in.BaseURL, in.Token, in.Model, in.Retries}); err != nil {
		return nil, fmt.Errorf("snippet template render for %q: %w", matched.ID, err)
	}

	return &ConnectOutput{
		Client:  *matched,
		Snippet: buf.String(),
		BaseURL: in.BaseURL,
		Token:   in.Token,
		Model:   in.Model,
	}, nil
}

// ClientIDs returns just the IDs in display order, useful for completion
// and list views.
func ClientIDs() []string {
	out := make([]string, len(clients))
	for i := range clients {
		out[i] = clients[i].ID
	}
	return out
}

// SortedClientIDs returns the IDs in alphabetical order, useful when the
// caller wants a deterministic listing independent of display order.
func SortedClientIDs() []string {
	out := ClientIDs()
	sort.Strings(out)
	return out
}
