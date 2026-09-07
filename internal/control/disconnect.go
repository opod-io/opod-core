package control

import "fmt"

// DisconnectSnippet returns the reversal instructions for a client previously
// set up by ConnectSnippet — how to un-set the Opod-pointing config and
// (optionally) point the client back at the vendor with the user's own key.
//
// Snippets are intentionally static (no template substitution): there's no
// user-specific data to embed and the cleanup steps are the same for everyone.
// If a new client is added to the `clients` registry, add a matching case
// here too — DisconnectSnippet errors clearly on an unknown id so the gap
// is obvious.
func DisconnectSnippet(clientID string) (string, error) {
	for _, c := range clients {
		if c.ID == clientID {
			s, ok := disconnectSnippets[clientID]
			if !ok {
				return "", fmt.Errorf("disconnect: no reversal snippet for %q (the connect registry lists it, but disconnect.go doesn't — please update disconnectSnippets)", clientID)
			}
			return s, nil
		}
	}
	return "", fmt.Errorf("disconnect: unknown client %q (try `opod disconnect --list`)", clientID)
}

// disconnectSnippets is the per-client reversal text. Keep these in sync
// with the connect-side templates: any env var or settings field that
// connect sets should appear in the matching disconnect block.
var disconnectSnippets = map[string]string{

	"cursor": `# Cursor's Opod override lives in the GUI, not env vars.
# Reverse it like this:
#
#   1. Open Cursor → Settings (Cmd/Ctrl + ,)
#   2. Models → "Override OpenAI Base URL" → toggle OFF
#      (or just clear the URL field and the model name override)
#   3. Cursor immediately resumes using its built-in billing,
#      or your own OpenAI key if you set one under API Keys.
#
# Nothing to unset on the shell side — Cursor stores its
# config in its own settings store.
`,

	"aider": `# Aider takes its base URL from CLI flags or env vars.
# Reverse the Opod override:
unset OPENAI_API_BASE
unset OPENAI_API_KEY

# Then run aider with your own vendor key:
export OPENAI_API_KEY=sk-...           # for OpenAI
# OR
export ANTHROPIC_API_KEY=sk-ant-...    # for Claude

aider                                  # picks up the key automatically
`,

	"continue": `# Continue's config lives in ~/.continue/config.json (VS Code)
# or ~/.continue/config.json on JetBrains.
#
# Open the file and remove the model entry that points at Opod.
# It looks something like this and should be deleted:
#
#   {
#     "title": "Opod",
#     "provider": "openai",
#     "model": "...",
#     "apiBase": "http://your-opod-host:8080/v1",
#     "apiKey": "sk-orc-..."
#   }
#
# Continue picks up the change the next time you open the panel.
`,

	"zed": `# Zed reads its model config from settings.json
# (Cmd/Ctrl + , in the editor, or ~/.config/zed/settings.json).
#
# Find and remove the assistant block that points at Opod:
#
#   "assistant": {
#     "default_model": { ... pointing at Opod ... }
#   }
#
# Zed will fall back to the default OpenAI configuration —
# add your own OPENAI_API_KEY if you want to use vendor directly.
`,

	"cline": `# Cline / Roo Code stores config in the VS Code settings panel.
#
#   1. Open VS Code → Settings (Cmd/Ctrl + ,)
#   2. Search "cline" (or "roo")
#   3. Clear the "API Base URL" and "API Key" fields you set for Opod.
#   4. Pick a provider (Anthropic / OpenAI) and paste your own key.
#
# Cline picks up the change on the next message.
`,

	"open-webui": `# Open WebUI's Opod override lives inside the container (env vars at
# launch + UI settings). Reverse it:
#
#   1. If you launched it with the Docker command from ` + "`opod connect open-webui`" + `,
#      stop the container:
#        docker rm -f open-webui
#      Then re-launch without the OPENAI_API_BASE_URL / _API_KEY env vars
#      (or with the vendor URL / your own key):
#        docker run -d -p 3000:8080 \
#          -e OPENAI_API_BASE_URL=https://api.openai.com/v1 \
#          -e OPENAI_API_KEY=sk-... \
#          -v open-webui:/app/backend/data \
#          --name open-webui --restart always \
#          ghcr.io/open-webui/open-webui:main
#
#   2. If you set Opod in the UI instead: Settings → Admin Panel →
#      Connections → OpenAI API → clear the API URL + API Key fields
#      (or replace with your own provider).
#
# Open WebUI keeps user chats in the open-webui Docker volume regardless
# of which backend is configured.
`,

	"plandex": `# Plandex reads OPENAI_API_BASE + OPENAI_API_KEY from env.
unset OPENAI_API_BASE
unset OPENAI_API_KEY
unset PLANDEX_MODEL

# Restore your vendor key:
export OPENAI_API_KEY=sk-...

# Existing plans don't need to be re-created — Plandex picks up the
# new endpoint on the next ` + "`plandex tell`" + ` invocation.
`,

	"openhands": `# OpenHands runs in Docker. Reverse by stopping the container and
# relaunching without LLM_BASE_URL / LLM_API_KEY (or with vendor values):

docker rm -f openhands

# Restart pointing at your own provider:
docker run -it --rm --pull=always \
  -e LLM_MODEL=openai/gpt-4o \
  -e LLM_API_KEY=sk-... \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v ~/.openhands:/.openhands \
  -p 3000:3000 \
  --add-host host.docker.internal:host-gateway \
  --name openhands \
  docker.all-hands.dev/all-hands-ai/openhands:latest

# Existing sandboxed sessions live in ~/.openhands — they survive the
# restart and resume with the new provider.
`,

	"codex-cli": `# Codex CLI reads OPENAI_BASE_URL + OPENAI_API_KEY.
unset OPENAI_BASE_URL
export OPENAI_API_KEY=sk-...   # your real OpenAI key

# Or remove the override from ~/.codex/config.yaml — delete the
# "base_url:" line so Codex talks to api.openai.com.
`,

	"open-notebook": `# Open Notebook reads credentials from either docker-compose's .env
# or the in-UI Settings → API Keys panel. Reverse the one you used:
#
# Option A — .env: remove the OPENAI_API_BASE_URL line so the
# container picks up its default (api.openai.com); keep the API
# key value if it's still your OpenAI key, or swap to a vendor key:
#
#   # before
#   OPENAI_API_BASE_URL=http://your-opod-host:8080/v1
#   OPENAI_API_KEY=sk-orc-...
#
#   # after — drop the BASE_URL, keep / replace the key
#   OPENAI_API_KEY=sk-...
#
# Then bounce the container:
#   docker compose down && docker compose up -d
#
# Option B — UI: Settings → API Keys → find the "OpenAI Compatible"
# credential pointing at your Opod host → Delete (or edit, replace
# the base URL with https://api.openai.com/v1, replace the key).

# Existing notebooks and source embeddings live in the open-notebook
# Docker volume — they survive the swap. The next chat call uses
# the new provider.
`,

	"goose": `# Goose reads its provider from ~/.config/goose/config.yaml.
# Reverse the Opod override:
#
#   # before
#   GOOSE_PROVIDER: openai
#   GOOSE_MODEL: qwen-coder-14b
#   OPENAI_BASE_URL: http://your-opod-host:8080/v1
#   OPENAI_API_KEY: sk-orc-...
#
#   # after — delete the OPENAI_BASE_URL line so Goose talks to
#   # api.openai.com, and replace the OPENAI_API_KEY with your own:
#   GOOSE_PROVIDER: openai
#   GOOSE_MODEL: gpt-4o
#   OPENAI_API_KEY: sk-...

# Or re-run the interactive configurator:
goose configure
`,

	"opencode": `# OpenCode reads its endpoint from opencode.json (project root or
# ~/.config/opencode/opencode.json). Reverse the Opod override by
# deleting the per-provider "options.baseURL" + "options.apiKey" keys
# you added when you ran ` + "`opod connect opencode`" + `:
#
#   # before
#   "provider": {
#     "openai":    { "options": { "baseURL": "...", "apiKey": "sk-orc-..." } },
#     "anthropic": { "options": { "baseURL": "...", "apiKey": "sk-orc-..." } }
#   }
#
#   # after — drop the options blocks (or the whole "provider" key)
#   # OpenCode falls back to its built-in defaults: api.openai.com /
#   # api.anthropic.com with whatever API key the provider env var holds.

# Then set your vendor key(s) as needed:
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
`,

	"openclaw": `# OpenClaw reads its endpoint from config.yaml (or .env). Reverse by
# editing the file you wrote when you ran ` + "`opod connect openclaw`" + `:
#
#   # before
#   base_url: http://your-opod-host:8080/v1
#   api_key:  sk-orc-...
#   model:    qwen-coder-14b
#
#   # after — delete those lines, OpenClaw falls back to its defaults
#   # (OpenAI public API + $OPENAI_API_KEY) or whatever provider you set.

# If you used env vars instead:
unset OPENAI_BASE_URL
unset OPENAI_API_KEY
unset OPENCLAW_MODEL

export OPENAI_API_KEY=sk-...   # then OpenClaw talks to api.openai.com
`,

	"openai-sdk": `# If you set base_url in your Python/JS code, remove that argument:
#
#   # before
#   client = OpenAI(base_url="http://your-opod-host:8080/v1", api_key="sk-orc-...")
#   # after
#   client = OpenAI()                            # uses OPENAI_API_KEY env var
#
# If you used the env-var form, unset them:
unset OPENAI_BASE_URL
unset OPENAI_API_KEY

# Then set your own:
export OPENAI_API_KEY=sk-...
`,

	"curl": `# Just point curl at the vendor URL with your own key, no Opod cleanup needed:

# Anthropic:
curl https://api.anthropic.com/v1/messages \
  -H "x-api-key: sk-ant-..." \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}'

# OpenAI:
curl https://api.openai.com/v1/chat/completions \
  -H "Authorization: Bearer sk-..." \
  -H "content-type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
`,
}
