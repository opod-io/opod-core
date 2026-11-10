package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/opod-io/opod-core/internal/config"
)

func cmdStatus(args []string) {
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "status",
			summary: "this host's leader and engine, and the leader's models and nodes",
			usage:   "opod status [--json]",
			examples: []string{
				"opod status",
				"opod status --json   # machine-readable",
			},
		})
	}
	rest, asJSON := extractJSONFlag(args)
	if len(rest) > 0 {
		dieUsageOf("status", "unknown argument %q (status takes only --json)", rest[0])
	}
	cfg := loadConfigOrExit()
	base := localLeaderURL(cfg)
	url := base + "/healthz"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type engineStatus struct {
		Name      string `json:"name"`
		Endpoint  string `json:"endpoint"`
		Reachable bool   `json:"reachable"`
		Error     string `json:"error,omitempty"`
	}
	type modelRow struct {
		CatalogID string `json:"id"`
		Status    string `json:"status"`
		Source    string `json:"source"`
	}
	type nodeRow struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	var out struct {
		Leader struct {
			URL       string `json:"url"`
			Reachable bool   `json:"reachable"`
			Healthy   bool   `json:"healthy"`
			Status    string `json:"status,omitempty"`
			Error     string `json:"error,omitempty"`
		} `json:"leader"`
		Engine engineStatus `json:"engine"`
		Models []modelRow   `json:"models"`
		Nodes  []nodeRow    `json:"nodes"`
		// ListError says why models and nodes are absent: they are the
		// leader's to list, through its admin API.
		ListError string `json:"list_error,omitempty"`
	}
	out.Leader.URL = url
	out.Models, out.Nodes = []modelRow{}, []nodeRow{}

	// The engine is checked whatever the leader says: a dead leader over a
	// live engine and a live leader over a dead engine are different fixes.
	eng := newEngineFromConfig(cfg)
	out.Engine.Name, out.Engine.Endpoint = eng.Name(), eng.Endpoint()
	if err := eng.Health(ctx); err != nil {
		out.Engine.Error = err.Error()
	} else {
		out.Engine.Reachable = true
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := localLeaderClient(cfg, 3*time.Second).Do(req)
	switch {
	case err != nil:
		out.Leader.Error = err.Error()
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		out.Leader.Reachable, out.Leader.Status = true, resp.Status
		// Only a 2xx is healthy: a 401 or a 503 is a leader that answered
		// and said no, which used to print a check mark and exit 0.
		out.Leader.Healthy = resp.StatusCode/100 == 2
		if !out.Leader.Healthy {
			out.Leader.Error = strings.TrimSpace(string(body))
		}
	}
	if out.Leader.Healthy {
		if raw, lerr := adminCall(ctx, cfg, "GET", "/admin/v1/models", nil); lerr != nil {
			out.ListError = lerr.Error()
		} else {
			var ms []struct{ CatalogID, Status, Source string }
			_ = json.Unmarshal(raw, &ms)
			for _, m := range ms {
				out.Models = append(out.Models, modelRow{m.CatalogID, m.Status, m.Source})
			}
			if raw, nerr := adminCall(ctx, cfg, "GET", "/admin/v1/nodes", nil); nerr == nil {
				var ns []struct {
					ID    string `json:"ID"`
					State string `json:"state"`
				}
				_ = json.Unmarshal(raw, &ns)
				for _, n := range ns {
					out.Nodes = append(out.Nodes, nodeRow{n.ID, n.State})
				}
			}
		}
	}

	if asJSON {
		emitJSON(out)
		if !out.Leader.Healthy {
			os.Exit(1)
		}
		return
	}
	switch {
	case !out.Leader.Reachable:
		warn("no leader answering at %s (%s) — start one with `opod up`, or set `listen:` in %s or OPOD_LISTEN", base, err, config.DefaultPath())
	case !out.Leader.Healthy:
		warn("the leader at %s answered %s: %s", base, out.Leader.Status, truncStr(out.Leader.Error, 200))
	default:
		ok(os.Stdout, "leader: %s (%s)", out.Leader.Status, base)
	}
	if out.Engine.Reachable {
		ok(os.Stdout, "engine: %s ok at %s", eng.Name(), eng.Endpoint())
	} else {
		warn("engine: %s — %s", out.Engine.Error, engineStartHint(eng.Name()))
	}
	if !out.Leader.Healthy {
		os.Exit(1)
	}
	if out.ListError != "" {
		warn("models and nodes: %s", out.ListError)
		return
	}
	ready := 0
	for _, n := range out.Nodes {
		if n.State == "ready" {
			ready++
		}
	}
	note(os.Stdout, "nodes: %d (%d ready) — opod node ls for each", len(out.Nodes), ready)
	if len(out.Models) == 0 {
		note(os.Stdout, "no models installed yet — try `opod model add llama-3.2-3b`")
		return
	}
	fmt.Println()
	fmt.Println(bold("  Installed models:"))
	for _, m := range out.Models {
		fmt.Printf("    %s  status=%s  source=%s\n",
			padCyan(m.CatalogID, 22),
			padStatus(m.Status, 0),
			dim(m.Source))
	}
}
