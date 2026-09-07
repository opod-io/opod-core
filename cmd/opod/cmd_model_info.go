package main

// `opod model search|info` — rendering over models.Search and the catalog.

import (
	"context"
	"fmt"
	"strings"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

func modelSearch(query string, sortReleased bool, since string, asJSON bool) {
	cfg := loadConfigOrExit()
	cat := loadCatalogOrExit(cfg)
	// Find which models are already installed so we can mark them ✓
	installed := map[string]bool{}
	if st, err := store.OpenSQLite(cfg.Storage.DSN); err == nil {
		defer st.Close()
		if ms, err := st.Models().List(context.Background()); err == nil {
			for _, m := range ms {
				installed[m.CatalogID] = true
			}
		}
	}

	rows := models.Search(cat, query, since, sortReleased)

	if asJSON {
		// Decorate each entry with an `installed` field so scripts don't
		// have to cross-reference a separate query.
		type out struct {
			models.Entry
			Installed bool `json:"installed"`
		}
		decorated := make([]out, len(rows))
		for i, e := range rows {
			decorated[i] = out{Entry: e, Installed: installed[e.ID]}
		}
		emitJSON(decorated)
		return
	}

	fmt.Printf("%s %s %s %s %s %s %s\n",
		bold(fmt.Sprintf("%-26s", "ID")),
		bold(fmt.Sprintf("%-32s", "NAME")),
		bold(fmt.Sprintf("%7s", "SIZE")),
		bold(fmt.Sprintf("%5s", "RAM")),
		bold(fmt.Sprintf("%-22s", "CAPABILITIES")),
		bold(fmt.Sprintf("%-10s", "RELEASED")),
		bold("INSTALLED"))
	for _, e := range rows {
		size := "?"
		if e.SizeBytes > 0 {
			size = fmt.Sprintf("%.1f GB", float64(e.SizeBytes)/1e9)
		}
		caps := strings.Join(e.Capabilities, ",")
		if len(caps) > 22 {
			caps = caps[:21] + "…"
		}
		mark := ""
		if installed[e.ID] {
			mark = green("✓")
		}
		released := e.Released
		if released == "" {
			released = "—"
		}
		fmt.Printf("%s %-32s %7s %4dG %-22s %s %s\n",
			padCyan(e.ID, 26),
			truncStr(e.DisplayName, 32), size, e.Hardware.MinRAMGB, caps,
			padDim(released, 10),
			mark)
	}
	fmt.Println()
	fmt.Println(dim("Tip: `opod model info <id>` for full details on one model. `opod model add <id>` to install."))
}

// padCyan returns the colored string followed by trailing spaces so the
// visual column width is `n`. Using fmt's "%-26s" directly with a
// colored string would count the ANSI escape bytes as visible width.
func padCyan(s string, n int) string {
	if len(s) >= n {
		return cyan(s)
	}
	return cyan(s) + strings.Repeat(" ", n-len(s))
}

func padDim(s string, n int) string {
	if len(s) >= n {
		return dim(s)
	}
	return dim(s) + strings.Repeat(" ", n-len(s))
}

// parseModelSearchArgs extracts the query plus `--sort=released` and
// `--since YYYY-MM-DD` flags. Flags and the free-form query may appear
// in any order.
func parseModelSearchArgs(args []string) (query string, sortReleased bool, since string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--sort=released" || a == "--sort-released":
			sortReleased = true
		case a == "--sort":
			if i+1 < len(args) && args[i+1] == "released" {
				sortReleased = true
				i++
			}
		case strings.HasPrefix(a, "--since="):
			since = strings.TrimPrefix(a, "--since=")
		case a == "--since":
			if i+1 < len(args) {
				since = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-"):
			die("unknown flag: %s (run `opod model --help`)", a)
		default:
			if query == "" {
				query = a
			}
		}
	}
	return
}

// modelInfo prints the full metadata for a single catalog model + whether
// it's installed locally + ready-to-paste usage snippets. Matches the kind
// of info in MODELS.md but condensed for terminal display.
func modelInfo(id string, asJSON bool) {
	cfg := loadConfigOrExit()
	cat := loadCatalogOrExit(cfg)
	entry := models.FindByID(cat, id)
	if entry == nil {
		die("no catalog entry for %q (try `opod model search`)", id)
	}
	if asJSON {
		emitJSON(entry)
		return
	}

	// Check installed state
	st, _ := store.OpenSQLite(cfg.Storage.DSN)
	var installedRow *store.Model
	if st != nil {
		defer st.Close()
		installedRow, _ = st.Models().Get(context.Background(), id)
	}

	bold, dim, reset := ansiCodes()

	fmt.Printf("%s%s%s — %s\n", bold, entry.ID, reset, entry.DisplayName)
	fmt.Println()

	// Status
	status := dim + "not installed" + reset
	if installedRow != nil {
		status = "installed · " + installedRow.Status + dim + " (" + installedRow.InstalledAt.Format("2006-01-02") + ")" + reset
	}
	fmt.Printf("  %sStatus%s         %s\n", bold, reset, status)

	// Source
	source := "—"
	switch entry.Source.Type {
	case "ollama":
		source = "ollama: " + entry.Source.OllamaName
	case "huggingface":
		source = "huggingface: " + entry.Source.Repo
		if entry.Source.File != "" {
			source += " (" + entry.Source.File + ")"
		}
	case "file":
		source = "file: " + entry.Source.Path
	}
	fmt.Printf("  %sSource%s         %s\n", bold, reset, source)

	// Size
	if entry.SizeBytes > 0 {
		fmt.Printf("  %sSize%s           %.1f GB\n", bold, reset, float64(entry.SizeBytes)/1e9)
	}
	if entry.Quant != "" {
		fmt.Printf("  %sQuant%s          %s\n", bold, reset, entry.Quant)
	}
	if entry.ContextWindow > 0 {
		fmt.Printf("  %sContext window%s %d tokens\n", bold, reset, entry.ContextWindow)
	}
	fmt.Printf("  %sMin RAM%s        %d GB\n", bold, reset, entry.Hardware.MinRAMGB)
	if entry.Hardware.MinVRAMGB > 0 {
		fmt.Printf("  %sMin VRAM%s       %d GB\n", bold, reset, entry.Hardware.MinVRAMGB)
	}
	if len(entry.Capabilities) > 0 {
		fmt.Printf("  %sCapabilities%s   %s\n", bold, reset, strings.Join(entry.Capabilities, ", "))
	}
	if len(entry.RecommendedEngines) > 0 {
		fmt.Printf("  %sEngines%s        %s\n", bold, reset, strings.Join(entry.RecommendedEngines, ", "))
	}
	if len(entry.Tags) > 0 {
		fmt.Printf("  %sTags%s           %s\n", bold, reset, strings.Join(entry.Tags, ", "))
	}
	if entry.Sharding.Required {
		fmt.Printf("  %sSharding%s       required (default %d shards, %s engine)\n",
			bold, reset, entry.Sharding.DefaultShards, entry.Sharding.Engine)
	}
	if entry.License != "" {
		licenseLine := entry.License
		if entry.LicenseURL != "" {
			licenseLine += " · " + dim + entry.LicenseURL + reset
		}
		fmt.Printf("  %sLicense%s        %s\n", bold, reset, licenseLine)
	}
	if entry.Released != "" {
		fmt.Printf("  %sReleased%s       %s\n", bold, reset, entry.Released)
	}
	if len(entry.Fallback) > 0 {
		fmt.Printf("  %sFallback%s       %s\n",
			bold, reset, strings.Join(entry.Fallback, " → "))
	}
	if len(entry.FallbackOnContextLength) > 0 {
		fmt.Printf("  %sFallback (context-length)%s  %s\n",
			bold, reset, strings.Join(entry.FallbackOnContextLength, " → "))
	}
	if len(entry.FallbackOnContentPolicy) > 0 {
		fmt.Printf("  %sFallback (content-policy)%s  %s\n",
			bold, reset, strings.Join(entry.FallbackOnContentPolicy, " → "))
	}

	// Pricing — vendor lookup first, then the catalog override if set.
	pp, pc := models.PriceFor(entry.ID, []models.Entry{*entry})
	if pp > 0 || pc > 0 {
		// Render as $/M tokens so the line reads like the vendor's
		// posted rate ("$3.00 / 1M prompt").
		fmt.Printf("  %sPricing%s        $%.2f / 1M prompt · $%.2f / 1M completion\n",
			bold, reset, pp*1000, pc*1000)
	} else {
		fmt.Printf("  %sPricing%s        %sfree (no cost tracking — open weights on your hardware)%s\n",
			bold, reset, dim, reset)
	}

	// Install + usage snippets — shape depends on what this model does.
	hasCap := func(c string) bool {
		for _, x := range entry.Capabilities {
			if x == c {
				return true
			}
		}
		return false
	}
	isEmbedding := hasCap("embedding")
	isVision := hasCap("vision")

	fmt.Println()
	fmt.Printf("%sInstall%s\n", bold, reset)
	if entry.Sharding.Required {
		fmt.Printf("  opod shard create %s %d\n", entry.ID, max2(entry.Sharding.DefaultShards))
	} else {
		fmt.Printf("  opod model add %s\n", entry.ID)
	}

	fmt.Println()
	if isEmbedding {
		fmt.Printf("%sUse via API (OpenAI embeddings shape)%s\n", bold, reset)
		fmt.Printf("  curl http://localhost:8080/v1/embeddings \\\n")
		fmt.Printf("    -H 'Authorization: Bearer sk-orc-...' \\\n")
		fmt.Printf("    -d '{\"model\":\"%s\",\"input\":\"hello world\"}'\n", entry.ID)
		fmt.Println()
		fmt.Printf("%sDrop-in for OpenAI text-embedding-* in any RAG library.%s\n", dim, reset)
	} else {
		fmt.Printf("%sUse via API (OpenAI shape)%s\n", bold, reset)
		fmt.Printf("  curl http://localhost:8080/v1/chat/completions \\\n")
		fmt.Printf("    -H 'Authorization: Bearer sk-orc-...' \\\n")
		fmt.Printf("    -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n", entry.ID)
		if isVision {
			fmt.Println()
			fmt.Printf("%sFor image input, send a content array with an `image_url` block:%s\n", dim, reset)
			fmt.Printf("  -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\n", entry.ID)
			fmt.Printf("       \"content\":[{\"type\":\"text\",\"text\":\"what is this?\"},\n")
			fmt.Printf("                   {\"type\":\"image_url\",\"image_url\":{\"url\":\"data:image/png;base64,...\"}}]}]}'\n")
		}
		fmt.Println()
		fmt.Printf("%sUse from an OpenAI-shape tool%s (Codex CLI, Aider, Cursor, the OpenAI SDK — `opod connect <tool>` prints the exact lines)\n", bold, reset)
		fmt.Printf("  export OPENAI_BASE_URL=http://localhost:8080/v1\n")
		fmt.Printf("  export OPENAI_API_KEY=sk-orc-...\n")
		fmt.Printf("  codex --model %s\n", entry.ID)
	}
	fmt.Println()
	fmt.Printf("%sFull walkthrough%s   https://github.com/opod-io/opod/blob/main/MODELS.md#%s\n",
		bold, reset, strings.ReplaceAll(entry.ID, ".", "-"))
}

func max2(n int) int {
	if n < 2 {
		return 2
	}
	return n
}
