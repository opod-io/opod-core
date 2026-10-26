package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// A disk tier (PLAN T26.9) is the host tier's connector with an fs tier below
// it: one --kv-transfer-config, never a second connector, and only when the
// host tier it is fed from is on the line.
func TestVLLMDiskTierLaunchLine(t *testing.T) {
	const host = "--kv-offloading-size 4 --kv-offloading-backend native"
	for _, tc := range []struct {
		name  string
		flags string
		want  bool // the fs tier is on the line
	}{
		{"host tier and a directory", `{"kv_tier_fs":"/data/models/.kvtier/ep_1/k","extra":"` + host + `"}`, true},
		{"size spelled with =", `{"kv_tier_fs":"/d/t","extra":"--kv-offloading-size=4"}`, true},
		{"no host tier to feed it", `{"kv_tier_fs":"/d/t"}`, false},
		{"another backend", `{"kv_tier_fs":"/d/t","extra":"--kv-offloading-size 4 --kv-offloading-backend lmcache"}`, false},
		{"a connector of the plan's own", `{"kv_tier_fs":"/d/t","extra":"` + host + ` --kv-transfer-config.kv_connector=NixlConnector"}`, false},
		{"relative directory", `{"kv_tier_fs":"data/t","extra":"` + host + `"}`, false},
		{"escaping directory", `{"kv_tier_fs":"/data/../etc","extra":"` + host + `"}`, false},
		{"shell in the directory", `{"kv_tier_fs":"/d/t;rm -rf /","extra":"` + host + `"}`, false},
		{"quote in the directory", `{"kv_tier_fs":"/d/t'x","extra":"` + host + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, extra := ParseEngineFlags(tc.flags).vllmShellOverrides()
			has := strings.Contains(extra, "--kv-transfer-config' '")
			if has != tc.want {
				t.Fatalf("fs tier on the line = %v, want %v: %s", has, tc.want, extra)
			}
			if seeded := strings.Contains(sh, "PYTHONHASHSEED"); seeded != tc.want {
				t.Fatalf("PYTHONHASHSEED exported = %v, want %v: %q", seeded, tc.want, sh)
			}
			if strings.Count(extra, "--kv-transfer-config") > 1 {
				t.Fatalf("two connectors: %s", extra)
			}
		})
	}
}

func TestVLLMDiskTierConfigIsTheWholeTier(t *testing.T) {
	_, extra := ParseEngineFlags(`{"kv_tier_fs":"/data/models/.kvtier/ep_1/vllm-nvidia-auto-tp1-pp1-bsauto-abc","extra":"--kv-offloading-size 4 --kv-offloading-backend native"}`).vllmShellOverrides()
	i := strings.Index(extra, "--kv-transfer-config' '")
	if i < 0 {
		t.Fatalf("no config: %s", extra)
	}
	raw := extra[i+len("--kv-transfer-config' '"):]
	raw = raw[:strings.Index(raw, "'")]
	var cfg struct {
		Connector string `json:"kv_connector"`
		Role      string `json:"kv_role"`
		Extra     struct {
			Spec  string           `json:"spec_name"`
			CPU   int64            `json:"cpu_bytes_to_use"`
			Tiers []map[string]any `json:"secondary_tiers"`
		} `json:"kv_connector_extra_config"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("config is not JSON: %v: %s", err, raw)
	}
	if cfg.Connector != "OffloadingConnector" || cfg.Role != "kv_both" || cfg.Extra.Spec != "TieringOffloadingSpec" || cfg.Extra.CPU != 4<<30 {
		t.Fatalf("config %+v", cfg)
	}
	if len(cfg.Extra.Tiers) != 1 || cfg.Extra.Tiers[0]["type"] != "fs" || cfg.Extra.Tiers[0]["root_dir"] != "/data/models/.kvtier/ep_1/vllm-nvidia-auto-tp1-pp1-bsauto-abc" {
		t.Fatalf("secondary tiers %+v", cfg.Extra.Tiers)
	}
	// The host tier's own arguments stay: vLLM merges the size into this config.
	if !strings.Contains(extra, "'--kv-offloading-size' '4'") {
		t.Fatalf("host tier dropped: %s", extra)
	}
}
