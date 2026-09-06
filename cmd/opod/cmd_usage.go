package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/config"
)

// cmdUsage prints the recent usage records or time-bucketed aggregates.
//
//	opod usage [--limit=N] [--user=X]
//	opod usage --by user,model --bucket day --since 2026-06-01
func cmdUsage(args []string) {
	args, asJSON := extractJSONFlag(args)
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	limit := fs.Int("limit", 50, "maximum number of rows to show")
	user := fs.String("user", "", "filter to a specific user_id (client-side)")
	summary := fs.Bool("summary", false, "show aggregate stats (total, top models, p50/p95, error rate) instead of rows")
	by := fs.String("by", "", "time-bucketed breakdown: comma-separated subset of user,model,protocol,outcome")
	bucket := fs.String("bucket", "day", "time bucket for --by: hour|day|month|total")
	since := fs.String("since", "", "ISO date (YYYY-MM-DD) for --by — defaults to 30 days ago")
	until := fs.String("until", "", "ISO date (YYYY-MM-DD) for --by — defaults to now")
	help := helpSpec{
		name:    "usage",
		summary: "show recent inference usage records",
		usage:   "opod usage [--limit N] [--user X] [--summary] [--json] | opod usage --by user,model --bucket day --since YYYY-MM-DD",
		flags:   fs,
		examples: []string{
			"opod usage                                     # latest 50 records",
			"opod usage --limit 200                         # latest 200",
			"opod usage --user alice                        # filter by user",
			"opod usage --summary                           # aggregate view (top models, p50/p95, error rate)",
			"opod usage --by user --bucket day --since 2026-05-01      # per-user-per-day",
			"opod usage --by model --bucket month                       # monthly per-model",
			"opod usage --by user,model --bucket total --json           # totals, scriptable",
		},
	}
	// Bad flags: print usage to stderr and let ExitOnError exit 2.
	fs.Usage = func() { showUsageErr(help) }
	if wantsHelp(args) {
		showHelp(help)
	}
	_ = fs.Parse(args)

	cfg := loadConfigOrExit()

	if *by != "" {
		usageBreakdown(cfg, *by, *bucket, *since, *until, *limit, asJSON)
		return
	}

	if *summary {
		body, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/usage/summary", nil)
		if err != nil {
			die("%v: %s", err, string(body))
		}
		if asJSON {
			fmt.Println(string(body))
			return
		}
		renderUsageSummary(body)
		return
	}

	body, err := adminCall(context.Background(), cfg, "GET", "/admin/v1/usage/recent", nil)
	if err != nil {
		die("%v: %s", err, string(body))
	}
	var rows []map[string]any
	_ = json.Unmarshal(body, &rows)

	// Apply --user filter + --limit up-front so JSON and table modes
	// both honor them.
	filtered := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if *user != "" && fmt.Sprint(r["UserID"]) != *user {
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
		fmt.Println("(no usage records yet)")
		return
	}

	fmt.Printf("%s %s %s %s %s %s %s %s\n",
		bold(fmt.Sprintf("%-19s", "TIME")),
		bold(fmt.Sprintf("%-14s", "USER/KEY")),
		bold(fmt.Sprintf("%-22s", "MODEL")),
		bold(fmt.Sprintf("%-12s", "PROTOCOL")),
		bold(fmt.Sprintf("%5s", "PROMPT")),
		bold(fmt.Sprintf("%5s", "COMPL")),
		bold(fmt.Sprintf("%7s", "MS")),
		bold("OUTCOME"))
	for _, r := range filtered {
		ts := parseTime(r["TS"])
		outcome := fmt.Sprint(r["Outcome"])
		coloredOutcome := outcome
		switch strings.ToLower(outcome) {
		case "ok", "success", "completed":
			coloredOutcome = green(outcome)
		case "error", "failed", "timeout", "cancelled":
			coloredOutcome = red(outcome)
		}
		fmt.Printf("%s %-14s %-22s %-12s %5v %5v %7v %s\n",
			dim(ts.Format("2006-01-02 15:04:05")),
			truncStr(fmt.Sprint(firstNonEmpty(r["UserID"], r["APIKeyID"])), 14),
			truncStr(fmt.Sprint(r["Model"]), 22),
			fmt.Sprint(r["Protocol"]),
			r["PromptTokens"], r["CompletionTokens"], r["LatencyMS"],
			coloredOutcome)
	}
}

// renderUsageSummary pretty-prints the /admin/v1/usage/summary JSON for
// a terminal. Mirrors the dashboard's home view in plain text.
func renderUsageSummary(body []byte) {
	var s struct {
		Total       int     `json:"total"`
		TokensTotal int64   `json:"tokens_total"`
		CostTotal   float64 `json:"cost_usd_total"`
		CostToday   float64 `json:"cost_usd_today"`
		ErrorRate   float64 `json:"error_rate"`
		P50MS       int     `json:"p50_ms"`
		P95MS       int     `json:"p95_ms"`
		P99MS       int     `json:"p99_ms"`
		TopModels   []struct {
			Model   string  `json:"model"`
			Count   int     `json:"count"`
			CostUSD float64 `json:"cost_usd"`
		} `json:"top_models"`
		RPM60Min []int `json:"rpm_60min"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		die("decode summary: %v", err)
	}
	fmt.Printf("%s\n", bold("Usage summary (last 1000 requests)"))
	if s.Total == 0 {
		fmt.Println(dim("  (no requests recorded yet)"))
		return
	}
	fmt.Printf("  %s  %d   %s  %s\n",
		bold("Total requests"), s.Total,
		bold("Tokens served"), fmt.Sprintf("%d", s.TokensTotal))
	if s.CostTotal > 0 || s.CostToday > 0 {
		fmt.Printf("  %s   %s today · %s in last 1000\n",
			bold("$ spent"),
			green(fmt.Sprintf("$%.4f", s.CostToday)),
			dim(fmt.Sprintf("$%.4f", s.CostTotal)))
	}
	errColor := green
	if s.ErrorRate > 0.05 {
		errColor = red
	} else if s.ErrorRate > 0 {
		errColor = yellow
	}
	fmt.Printf("  %s  p50=%dms  p95=%dms  p99=%dms   %s  %s\n",
		bold("Latency"),
		s.P50MS, s.P95MS, s.P99MS,
		bold("Error rate"),
		errColor(fmt.Sprintf("%.1f%%", s.ErrorRate*100)))
	if len(s.TopModels) > 0 {
		fmt.Printf("  %s\n", bold("Top models"))
		for _, m := range s.TopModels {
			costStr := ""
			if m.CostUSD > 0 {
				costStr = " · " + dim(fmt.Sprintf("$%.4f", m.CostUSD))
			}
			fmt.Printf("    %s  %s%s\n", padCyan(m.Model, 24), dim(fmt.Sprintf("%d requests", m.Count)), costStr)
		}
	}
	if len(s.RPM60Min) == 60 {
		fmt.Printf("  %s  %s\n", bold("Last 60 min"), sparkline(s.RPM60Min))
	}
}

// sparkline renders a series of ints as a compact bar-chart row using
// the unicode block ramp.
func sparkline(vals []int) string {
	if len(vals) == 0 {
		return ""
	}
	peak := 0
	for _, v := range vals {
		if v > peak {
			peak = v
		}
	}
	if peak == 0 {
		return strings.Repeat("·", len(vals))
	}
	bars := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range vals {
		idx := int(float64(v) / float64(peak) * float64(len(bars)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(bars) {
			idx = len(bars) - 1
		}
		b.WriteRune(bars[idx])
	}
	return cyan(b.String())
}

func parseTime(v any) time.Time {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func truncStr(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Count and slice by rune so a multibyte character isn't split into
	// mojibake, and guard the n==1 edge (n-1 == 0).
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func firstNonEmpty(vals ...any) any {
	for _, v := range vals {
		s := fmt.Sprint(v)
		if s != "" && s != "<nil>" {
			return v
		}
	}
	return ""
}

// usageBreakdown fetches /admin/v1/usage/breakdown and renders the rows.
// `--by` accepts a comma-separated list of {user,model,protocol,outcome};
// `--bucket` ∈ {hour,day,month,total}; `--since`/`--until` are dates.
func usageBreakdown(cfg *config.Config, by, bucket, since, until string, limit int, asJSON bool) {
	q := fmt.Sprintf("/admin/v1/usage/breakdown?bucket=%s&group_by=%s", bucket, by)
	if since != "" {
		q += "&since=" + since
	}
	if until != "" {
		q += "&until=" + until
	}
	if limit > 0 {
		q += fmt.Sprintf("&limit=%d", limit)
	}
	body, err := adminCall(context.Background(), cfg, "GET", q, nil)
	if err != nil {
		die("%v: %s", err, string(body))
	}
	if asJSON {
		fmt.Println(string(body))
		return
	}
	var resp struct {
		Rows []struct {
			Bucket           string  `json:"bucket"`
			User             string  `json:"user,omitempty"`
			Model            string  `json:"model,omitempty"`
			Protocol         string  `json:"protocol,omitempty"`
			Outcome          string  `json:"outcome,omitempty"`
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			Requests         int64   `json:"requests"`
			CostUSD          float64 `json:"cost_usd"`
		} `json:"rows"`
		Totals struct {
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			Requests         int64   `json:"requests"`
			CostUSD          float64 `json:"cost_usd"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		die("decode breakdown: %v", err)
	}
	if len(resp.Rows) == 0 {
		fmt.Println("(no usage in the requested window)")
		return
	}
	groups := strings.Split(by, ",")
	// Only render the $ column when the totals are non-zero — keeps
	// open-weight setups from showing a sea of $0.0000.
	showCost := resp.Totals.CostUSD > 0
	header := []string{bold(fmt.Sprintf("%-19s", "BUCKET"))}
	for _, g := range groups {
		header = append(header, bold(fmt.Sprintf("%-22s", strings.ToUpper(strings.TrimSpace(g)))))
	}
	header = append(header,
		bold(fmt.Sprintf("%10s", "PROMPT")),
		bold(fmt.Sprintf("%10s", "COMPL")),
		bold(fmt.Sprintf("%10s", "REQS")))
	if showCost {
		header = append(header, bold(fmt.Sprintf("%10s", "$")))
	}
	fmt.Println(strings.Join(header, " "))
	for _, r := range resp.Rows {
		cols := []string{padDim(r.Bucket, 19)}
		for _, g := range groups {
			switch strings.TrimSpace(g) {
			case "user":
				cols = append(cols, padCyan(r.User, 22))
			case "model":
				cols = append(cols, padCyan(r.Model, 22))
			case "protocol":
				cols = append(cols, fmt.Sprintf("%-22s", r.Protocol))
			case "outcome":
				cols = append(cols, fmt.Sprintf("%-22s", r.Outcome))
			}
		}
		cols = append(cols,
			fmt.Sprintf("%10d", r.PromptTokens),
			fmt.Sprintf("%10d", r.CompletionTokens),
			fmt.Sprintf("%10d", r.Requests))
		if showCost {
			cols = append(cols, fmt.Sprintf("%10s", fmt.Sprintf("$%.4f", r.CostUSD)))
		}
		fmt.Println(strings.Join(cols, " "))
	}
	fmt.Println()
	costStr := ""
	if showCost {
		costStr = fmt.Sprintf("  cost=$%.4f", resp.Totals.CostUSD)
	}
	fmt.Printf("%s  prompt=%d  completion=%d  requests=%d%s\n",
		bold("Totals"), resp.Totals.PromptTokens, resp.Totals.CompletionTokens, resp.Totals.Requests, costStr)
}
