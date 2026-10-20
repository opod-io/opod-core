package models

import "testing"

// knownToolParsers are the parser names each engine registers, read from the
// releases our worker images pin (vLLM v0.27.1 vllm/tool_parsers, SGLang
// v0.5.21 function_call_parser.py) — the subset the catalog uses. An engine
// refuses an unknown name at start, so a typo here is a worker that never
// comes up; a name added to the catalog is added here after it is checked
// against the engine's own list.
var knownToolParsers = map[string]map[string]bool{
	"vllm":   {"hermes": true, "qwen3_coder": true, "llama3_json": true, "openai": true, "gemma4": true},
	"sglang": {"qwen25": true, "qwen3_coder": true, "llama3": true, "gpt-oss": true, "gemma4": true},
}

// PLAN T19.25: every tool_parser the bundled catalog names is for an engine
// that takes one, on a model that calls tools, and is a name that engine knows.
func TestTheCatalogsToolParsersAreReal(t *testing.T) {
	entries, err := BundledCatalog()
	if err != nil {
		t.Fatal(err)
	}
	named := 0
	for i := range entries {
		e := &entries[i]
		for engine, parser := range e.ToolParser {
			named++
			known, ok := knownToolParsers[engine]
			if !ok {
				t.Errorf("%s: tool_parser for %q, an engine that takes no parser flag", e.ID, engine)
				continue
			}
			if !known[parser] {
				t.Errorf("%s: %s parser %q is not one the pinned %s release registers", e.ID, engine, parser, engine)
			}
			if ToolParser(e, engine) == "" {
				t.Errorf("%s: names a %s parser but has no `tools` capability", e.ID, engine)
			}
		}
	}
	if named == 0 {
		t.Fatal("the catalog names no tool parser at all")
	}
	if got := ToolParser(FindByID(entries, "qwen3-8b"), "vllm"); got != "hermes" {
		t.Errorf("qwen3-8b on vLLM: %q, want hermes", got)
	}
}

// A parser on a model the catalog says calls no tools is not used.
func TestAToolParserNeedsTheToolsCapability(t *testing.T) {
	e := &Entry{Capabilities: []string{"chat"}, ToolParser: map[string]string{"vllm": "hermes"}}
	if got := ToolParser(e, "vllm"); got != "" {
		t.Errorf("no tools capability: %q", got)
	}
	e.Capabilities = append(e.Capabilities, "tools")
	if got := ToolParser(e, "vllm"); got != "hermes" {
		t.Errorf("with tools: %q", got)
	}
	if got := ToolParser(nil, "vllm"); got != "" {
		t.Errorf("no entry: %q", got)
	}
}
