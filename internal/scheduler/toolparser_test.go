package scheduler

import (
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
)

// PLAN T19.25, the gang half: a vLLM or SGLang gang serves the same API as a
// worker's own engine, so it needs the same tool-call parser — or a gang of a
// tools model answers every call as text.
func TestGangsCarryTheCatalogsToolParser(t *testing.T) {
	entry := models.Entry{ID: "qwen3-8b", Capabilities: []string{"chat", "tools"},
		ToolParser: map[string]string{"vllm": "hermes", "sglang": "qwen25"}}

	ray := vllmGangCommand(vllmGang{Model: "org/m", ServedAs: entry.ID, Host: "10.0.0.1", Port: 9100, TP: 1, PP: 2,
		ToolArgs: agent.ToolParserArgs("vllm", models.ToolParser(&entry, "vllm"), "")})
	if !strings.Contains(ray, "'--enable-auto-tool-choice' '--tool-call-parser' 'hermes'") {
		t.Errorf("Ray gang: no parser on the head's serve line:\n%s", ray)
	}
	sgl := sglangGangCommand(sglangGang{Model: "org/m", ServedAs: entry.ID, ServePort: 30000,
		DistAddr: "10.0.0.1:29500", NNodes: 2, NodeRank: 0, TP: 2, PP: 1,
		ToolArgs: agent.ToolParserArgs("sglang", models.ToolParser(&entry, "sglang"), "")})
	if !strings.Contains(sgl, "'--tool-call-parser' 'qwen25'") {
		t.Errorf("SGLang gang: no parser on the rank's line:\n%s", sgl)
	}

	// No parser known: the lines are exactly what they were.
	if got := vllmGangCommand(vllmGang{Model: "org/m", ServedAs: "m", Host: "h", Port: 1, TP: 1, PP: 1}); strings.Contains(got, "tool") {
		t.Errorf("no parser: nothing is added:\n%s", got)
	}
	if got := sglangGangCommand(sglangGang{Model: "org/m", ServedAs: "m", ServePort: 1, DistAddr: "h:1", NNodes: 1, TP: 1, PP: 1}); strings.Contains(got, "tool") {
		t.Errorf("no parser: nothing is added:\n%s", got)
	}
}
