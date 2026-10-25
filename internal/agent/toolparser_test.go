package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		{"trtllm", "qwen3", "", []string{"--tool_parser", "qwen3"}},                               // T27.8
		{"trtllm", "qwen3", "--tool_parser=qwen3_coder", nil},                                     // the plan's own, TensorRT-LLM's spelling
		{"trtllm", "qwen3", "--tool-call-parser hermes", []string{"--tool_parser", "qwen3"}},      // not its flag
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
	t.Setenv("TMPDIR", t.TempDir())
	parsers := map[string]string{"vllm": "hermes", "sglang": "qwen25", "trtllm": "qwen3"}
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
		"trtllm": func(s *Server, id string) (string, error) {
			got, _, err := s.trtllmCmdline("Qwen/Qwen3-8B", id, "127.0.0.1", 8000)
			return got, err
		},
	}
	for engine, cmdline := range launch {
		want := "'" + toolParserFlag[engine] + "' '" + parsers[engine] + "'"
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
			"the plan's own": {ToolParser: hook, EngineFlags: EngineFlags{"extra": toolParserFlag[engine] + " pythonic"}},
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

// PLAN T27.8: an engine started with no tool-call parser answers a tool call
// as plain text with a 200. The worker that launched it refuses `tools` by
// name instead; one with a parser (the catalog's or the plan's), one that
// launched nothing (a gang's head), and tool_choice "none" are relayed.
func TestAWorkerWithNoParserRefusesToolsByName(t *testing.T) {
	hook := func(id, engine string) string {
		if id == "qwen3-8b" {
			return map[string]string{"vllm": "hermes", "sglang": "qwen25", "trtllm": "qwen3"}[engine]
		}
		return ""
	}
	tools := json.RawMessage(`[{"type":"function","function":{"name":"get_weather"}}]`)
	for _, engine := range []string{"vllm", "sglang", "trtllm"} {
		bare := &Server{ToolParser: hook}
		bare.noteToolParser(engine, "my-model")
		why := bare.toolsRefusal(tools, nil)
		if !strings.HasPrefix(why, "tools:") || !strings.Contains(why, engine) || !strings.Contains(why, toolParserFlag[engine]) {
			t.Errorf("%s with no parser: want a refusal naming tools, the engine and its flag, got %q", engine, why)
		}
		if bare.toolsRefusal(tools, json.RawMessage(`"none"`)) != "" || bare.toolsRefusal(nil, nil) != "" || bare.toolsRefusal(json.RawMessage(`[]`), nil) != "" {
			t.Errorf("%s: no tools offered, or tool_choice none, must be relayed", engine)
		}
		for name, s := range map[string]*Server{
			"the catalog's": {ToolParser: hook},
			"the plan's":    {EngineFlags: EngineFlags{"extra": toolParserFlag[engine] + " hermes"}},
		} {
			s.noteToolParser(engine, "qwen3-8b")
			if why := s.toolsRefusal(tools, nil); why != "" {
				t.Errorf("%s with %s parser: refused %q", engine, name, why)
			}
		}
	}
	if why := (&Server{}).toolsRefusal(tools, nil); why != "" {
		t.Errorf("a worker that launched nothing must relay, got %q", why)
	}
}

// The refusal reaches the caller as a 400 in the OpenAI shape, never the engine.
func TestTheChatRouteAnswersTheToolsRefusal(t *testing.T) {
	s := &Server{}
	s.noteToolParser("vllm", "my-model")
	rec := httptest.NewRecorder()
	body := `{"model":"my-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`
	s.chatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"unsupported_request"`) || !strings.Contains(rec.Body.String(), "tools:") {
		t.Fatalf("want 400 unsupported_request naming tools, got %d %s", rec.Code, rec.Body)
	}
}
