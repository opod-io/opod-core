package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
)

// cmdAudit prints the recent audit log entries.
//
//	opod audit [--limit=N] [--actor=X]
//
// renderAuditSummary pretty-prints the /admin/v1/audit/summary JSON.
func renderAuditSummary(body []byte) {
	var s struct {
		Total     int `json:"total"`
		TopActors []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"top_actors"`
		TopActions []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"top_actions"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		die("decode summary: %v", err)
	}
	fmt.Printf("%s\n", bold("Audit summary (last 1000 entries)"))
	if s.Total == 0 {
		fmt.Println(dim("  (no entries recorded yet)"))
		return
	}
	fmt.Printf("  %s  %d\n", bold("Total"), s.Total)
	if len(s.TopActors) > 0 {
		fmt.Printf("  %s\n", bold("Top actors"))
		for _, a := range s.TopActors {
			fmt.Printf("    %s  %s\n", padCyan(a.Name, 24), dim(fmt.Sprintf("%d entries", a.Count)))
		}
	}
	if len(s.TopActions) > 0 {
		fmt.Printf("  %s\n", bold("Top actions"))
		for _, a := range s.TopActions {
			fmt.Printf("    %s  %s\n", padCyan(a.Name, 40), dim(fmt.Sprintf("%d entries", a.Count)))
		}
	}
}

func cmdAudit(args []string) {
	args, asJSON := extractJSONFlag(args)
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	limit := fs.Int("limit", 50, "maximum rows to show")
	actor := fs.String("actor", "", "filter by actor name (client-side)")
	summary := fs.Bool("summary", false, "show top actors + top actions instead of rows")
	help := helpSpec{
		name:    "audit",
		summary: "show recent admin audit log entries",
		usage:   "opod audit [--limit N] [--actor X] [--summary] [--json]",
		flags:   fs,
		examples: []string{
			"opod audit                       # latest 50 entries",
			"opod audit --limit 500",
			"opod audit --actor admin",
			"opod audit --summary             # top actors + top actions",
			"opod audit --json                # machine-readable rows",
		},
	}
	// Bad flags: print usage to stderr and let ExitOnError exit 2.
	fs.Usage = func() { showUsageErr(help) }
	if wantsHelp(args) {
		showHelp(help)
	}
	_ = fs.Parse(args)

	cfg := loadConfigOrExit()

	if *summary {
		body, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/audit/summary", nil)
		if err != nil {
			die("%v: %s", err, string(body))
		}
		if asJSON {
			fmt.Println(string(body))
			return
		}
		renderAuditSummary(body)
		return
	}

	body, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/audit/recent", nil)
	if err != nil {
		die("%v: %s", err, string(body))
	}
	var rows []map[string]any
	_ = json.Unmarshal(body, &rows)

	filtered := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if *actor != "" && fmt.Sprint(r["Actor"]) != *actor {
			continue
		}
		filtered = append(filtered, r)
		if len(filtered) >= *limit {
			break
		}
	}

	if asJSON {
		emitJSON(filtered)
		return
	}
	if len(filtered) == 0 {
		fmt.Println(dim("(no audit records yet)"))
		return
	}
	fmt.Printf("%s %s %s %s\n",
		bold(fmt.Sprintf("%-19s", "TIME")),
		bold(fmt.Sprintf("%-16s", "ACTOR")),
		bold(fmt.Sprintf("%-40s", "ACTION")),
		bold("TARGET"))
	for _, r := range filtered {
		ts := parseTime(r["TS"])
		fmt.Printf("%s %-16s %-40s %s\n",
			dim(ts.Format("2006-01-02 15:04:05")),
			truncStr(fmt.Sprint(r["Actor"]), 16),
			truncStr(fmt.Sprint(r["Action"]), 40),
			cyan(fmt.Sprint(r["Target"])))
	}
}
