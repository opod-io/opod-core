package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

func cmdStatus(args []string) {
	if wantsHelp(args) {
		showHelp(helpSpec{
			name:    "status",
			summary: "show local node + cluster status",
			usage:   "opod status [--json]",
			examples: []string{
				"opod status",
				"opod status --json   # machine-readable",
			},
		})
	}
	_, asJSON := extractJSONFlag(args)
	cfg := loadConfigOrExit()
	addr := cfg.Listen
	if addr == "" {
		addr = ":8080"
	}
	url := "http://localhost" + addr + "/healthz"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type engineStatus struct {
		Name      string `json:"name"`
		Endpoint  string `json:"endpoint"`
		Reachable bool   `json:"reachable"`
		Error     string `json:"error,omitempty"`
	}
	type statusOut struct {
		ControlPlane struct {
			URL       string `json:"url"`
			Reachable bool   `json:"reachable"`
			Status    string `json:"status,omitempty"`
			Error     string `json:"error,omitempty"`
		} `json:"control_plane"`
		Engine engineStatus `json:"engine"`
		Models []struct {
			CatalogID string `json:"id"`
			Status    string `json:"status"`
			Source    string `json:"source"`
		} `json:"models"`
	}
	var out statusOut
	out.ControlPlane.URL = url
	out.Models = []struct {
		CatalogID string `json:"id"`
		Status    string `json:"status"`
		Source    string `json:"source"`
	}{}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		out.ControlPlane.Error = err.Error()
		if asJSON {
			emitJSON(out)
			os.Exit(1)
		}
		warn(os.Stdout, "control plane not reachable at %s: %v", url, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	out.ControlPlane.Reachable = true
	out.ControlPlane.Status = resp.Status

	eng := newEngineFromConfig(cfg)
	out.Engine.Name = eng.Name()
	out.Engine.Endpoint = eng.Endpoint()
	if err := eng.Health(ctx); err != nil {
		out.Engine.Error = err.Error()
	} else {
		out.Engine.Reachable = true
	}

	st := openStoreOrExit(cfg)
	defer st.Close()
	ms, err := st.Models().List(context.Background())
	if err != nil {
		die("list models: %v", err)
	}
	for _, m := range ms {
		out.Models = append(out.Models, struct {
			CatalogID string `json:"id"`
			Status    string `json:"status"`
			Source    string `json:"source"`
		}{m.CatalogID, m.Status, m.Source})
	}

	if asJSON {
		emitJSON(out)
		return
	}

	ok(os.Stdout, "control plane: %s (%s)", resp.Status, url)
	if out.Engine.Reachable {
		ok(os.Stdout, "engine: %s ok at %s", eng.Name(), eng.Endpoint())
	} else {
		warn(os.Stdout, "engine: not reachable: %s", out.Engine.Error)
	}
	if len(ms) == 0 {
		note(os.Stdout, "no models installed yet — try `opod model add llama-3.2-3b`")
		return
	}
	fmt.Println()
	fmt.Println(bold("  Installed models:"))
	for _, m := range ms {
		fmt.Printf("    %s  status=%s  source=%s\n",
			padCyan(m.CatalogID, 22),
			padStatus(m.Status, 0),
			dim(m.Source))
	}
}
