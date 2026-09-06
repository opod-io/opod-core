package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opod-io/opod/internal/config"
)

// cmdConfig shows the effective runtime config (secrets redacted) and
// surfaces the file path + env-var overrides admins need to know.
//
//	opod config show       # default
//	opod config path       # print the config file path
//	opod config edit       # print the $EDITOR command to edit the config
func cmdConfig(args []string) {
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "config",
			summary: "view + locate the effective runtime config",
			usage:   "opod config <show [--json] | path | edit>",
			examples: []string{
				"opod config show              # effective config, secrets redacted",
				"opod config show --json       # same, JSON output",
				"opod config path              # print the config file path",
				"opod config edit              # print the editor command",
			},
			notes: []string{
				"Config is YAML at ~/.opod/config.yaml. Secrets (vendor keys, worker tokens) come from env vars.",
			},
		})
	}
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "show":
		configShow(args[1:])
	case "path":
		fmt.Println(loadedConfigPath())
	case "edit":
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		fmt.Printf("Run: %s %s\n", editor, loadedConfigPath())
		fmt.Println("(then restart opod for changes to take effect)")
	default:
		dieUnknownSubcommand("config", args[0], []string{"show", "path", "edit"})
	}
}

// loadedConfigPath returns the path config.Load("") actually read.
// The loader resolves the file from the DEFAULT data dir BEFORE applying
// yaml/env overrides, so cfg.DataDir post-load (e.g. with `data_dir:`
// set in the file, or OPOD_DATA_DIR in the env) can point at a
// directory whose config.yaml was never opened. Replicate the loader's
// resolution instead of deriving the path from the loaded config.
func loadedConfigPath() string {
	return filepath.Join(config.Default().DataDir, "config.yaml")
}

func configShow(args []string) {
	fs := flag.NewFlagSet("config show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print as JSON instead of pretty key=value lines")
	_ = fs.Parse(args)

	cfg := loadConfigOrExit()
	body, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/config", nil)
	if err != nil {
		// adminCall returns a nil body only on a transport failure (leader
		// unreachable); an HTTP error (auth, 5xx) returns the response body.
		// Only fall back to the local view when the leader is genuinely
		// unreachable — otherwise surface the server error instead of
		// silently masking it with a CLI-loaded dump.
		if body != nil {
			die("config show: %v", err)
		}
		fmt.Fprintln(os.Stderr, "(leader not reachable; showing config loaded by this CLI — secrets redacted)")
		printConfigLocal(cfg, *asJSON)
		return
	}
	if *asJSON {
		fmt.Println(string(body))
		return
	}
	var v map[string]any
	_ = json.Unmarshal(body, &v)
	printConfigPretty("", v, 0)
	fmt.Println()
	if hint, ok := v["edit_hint"].(string); ok && hint != "" {
		fmt.Println(hint)
	}
}

func printConfigPretty(prefix string, v any, depth int) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			if k == "edit_hint" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := t[k]
			label := k
			if prefix != "" {
				label = prefix + "." + k
			}
			switch child.(type) {
			case map[string]any:
				printConfigPretty(label, child, depth+1)
			default:
				fmt.Printf("  %-40s %v\n", label, child)
			}
		}
	default:
		fmt.Printf("%v\n", t)
	}
}

func printConfigLocal(cfg any, asJSON bool) {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if asJSON {
		fmt.Println(string(b))
		return
	}
	// minimal pretty for local fallback
	out := strings.ReplaceAll(string(b), "\"", "")
	fmt.Println(out)
}
