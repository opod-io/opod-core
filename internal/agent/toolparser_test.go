package agent

import (
	"reflect"
	"strings"
	"testing"
)

// PLAN T19.25: vLLM and SGLang answered a `tools` request with the call as
// text, because nothing ever started them with a tool-call parser. Each engine
// gets its own spelling; a plan whose `extra` already names a parser keeps it.
func TestToolParserArgsPerEngine(t *testing.T) {
	for _, c := range []struct {
		engine, parser, extra string
		want                  []string
	}{
		{"vllm", "hermes", "", []string{"--enable-auto-tool-choice", "--tool-call-parser", "hermes"}},
		{"sglang", "qwen25", "", []string{"--tool-call-parser", "qwen25"}},
		{"vllm", "hermes", "--tool-call-parser llama3_json", nil},                                 // the plan's own parser wins
		{"sglang", "qwen25", "--tool-call-parser=qwen3_coder", nil},                               // in either spelling
		{"vllm", "hermes", "--enable-auto-tool-choice", []string{"--tool-call-parser", "hermes"}}, // not said twice
		{"vllm", "", "", nil},                 // no parser known
		{"llamacpp", "hermes", "", nil},       // parses tool calls itself
		{"vllm", "hermes; rm -rf /", "", nil}, // never a shell word
	} {
		if got := ToolParserArgs(c.engine, c.parser, c.extra); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %q extra %q: %q, want %q", c.engine, c.parser, c.extra, got, c.want)
		}
	}
}

// The worker's own launches carry the parser its catalog names for the model
// it serves, by catalog id; with no catalog hook the line is what it was.
func TestTheLaunchCarriesTheCatalogsToolParser(t *testing.T) {
	parsers := map[string]string{"vllm": "hermes", "sglang": "qwen25"}
	hook := func(id, engine string) string {
		if id != "qwen3-8b" {
			return ""
		}
		return parsers[engine]
	}
	launch := map[string]func(*Server, string) (string, error){
		"vllm": func(s *Server, id string) (string, error) {
			return s.vllmCmdline("Qwen/Qwen3-8B", id, "127.0.0.1", 8000)
		},
		"sglang": func(s *Server, id string) (string, error) {
			return s.sglangCmdline("Qwen/Qwen3-8B", id, "127.0.0.1", 30000)
		},
	}
	for engine, cmdline := range launch {
		want := "'--tool-call-parser' '" + parsers[engine] + "'"
		got, err := cmdline(&Server{ToolParser: hook}, "qwen3-8b")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: the catalog's parser is not on the line:\n%s", engine, got)
		}
		if engine == "vllm" && !strings.Contains(got, "'--enable-auto-tool-choice'") {
			t.Errorf("vllm: a parser without --enable-auto-tool-choice is never used:\n%s", got)
		}
		for name, s := range map[string]*Server{
			"no catalog":     {},
			"another model":  {ToolParser: hook},
			"the plan's own": {ToolParser: hook, EngineFlags: EngineFlags{"extra": "--tool-call-parser pythonic"}},
		} {
			id := "qwen3-8b"
			if name == "another model" {
				id = "llama-3.2-1b"
			}
			got, err := cmdline(s, id)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, want) {
				t.Errorf("%s, %s: the catalog's parser must not be added:\n%s", engine, name, got)
			}
		}
	}
}
