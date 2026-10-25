package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Tool-call parsers (PLAN T19.25). vLLM and SGLang return a model's tool call
// as plain text — `content: "<tool_call>{…}</tool_call>"`, finish_reason stop —
// unless they are started with a parser for that model's family, so a client
// that sent `tools` gets an answer instead of a call. llama.cpp and Ollama
// parse tool calls on their own and need nothing here.
//
// The parser's name comes from the model's catalog entry (`tool_parser`, per
// engine, because the engines spell one family differently); the flags are
// added only when the entry names one for this engine. The plan wins: an
// `extra` that already names a parser is the operator's choice and is left
// exactly as written. TensorRT-LLM's trtllm-serve takes `--tool_parser` (PLAN
// T27.8) and answered a tool call as text the same way.
//
// A worker that started one of these engines with NO parser refuses a request
// carrying `tools` by name (toolsRefusal) rather than relaying it: the engine
// would answer 200 with the call as plain text, and the caller would read a
// success in the wrong shape.

// toolParserFlag is each engine's own flag naming a tool-call parser; an
// engine missing here parses tool calls on its own or takes no parser.
var toolParserFlag = map[string]string{"vllm": "--tool-call-parser", "sglang": "--tool-call-parser", "trtllm": "--tool_parser"}

// ToolParserArgs is the engine's own flags for parser, for an engine started
// with extra as its plan's raw arguments: vLLM `--enable-auto-tool-choice
// --tool-call-parser <parser>`, SGLang `--tool-call-parser <parser>`,
// TensorRT-LLM `--tool_parser <parser>`. Nil when parser is empty, the engine
// takes no such flag, the name is not a plain token, or extra already names a
// parser. Exported for the leader's gang backends, which start the same
// engines over the process contract.
func ToolParserArgs(engine, parser, extra string) []string {
	parser = strings.TrimSpace(parser)
	flag, ok := toolParserFlag[engine]
	if !ok || parser == "" || !isToken(parser) || hasFlag(extra, flag) {
		return nil
	}
	if engine == "vllm" && !hasFlag(extra, "--enable-auto-tool-choice") {
		return []string{"--enable-auto-tool-choice", flag, parser}
	}
	return []string{flag, parser}
}

// hasFlag reports whether a raw argument string carries flag, as `--flag v` or
// `--flag=v`.
func hasFlag(extra, flag string) bool {
	for _, t := range strings.Fields(extra) {
		if t == flag || strings.HasPrefix(t, flag+"=") {
			return true
		}
	}
	return false
}

// toolParserArgs is ToolParserArgs for the model this worker launches, by its
// catalog id, with the plan's own `extra`.
func (s *Server) toolParserArgs(engine, modelID string) string {
	if s.ToolParser == nil {
		return ""
	}
	extra, _ := s.EngineFlags.get("extra")
	return shellQuoteAll(ToolParserArgs(engine, s.ToolParser(modelID, engine), extra))
}

// noteToolParser records, for the engine this worker just launched for
// modelID, whether it can turn a tool call into `tool_calls`: a parser from
// the catalog, or one the plan's extra names. Without either, toolsRefusal
// answers every request that carries tools.
func (s *Server) noteToolParser(engine, modelID string) {
	extra, _ := s.EngineFlags.get("extra")
	if hasFlag(extra, toolParserFlag[engine]) || (s.ToolParser != nil && ToolParserArgs(engine, s.ToolParser(modelID, engine), extra) != nil) {
		s.toolsRefused.Store("")
		return
	}
	s.toolsRefused.Store(fmt.Sprintf("tools: this worker started %s for %s with no tool-call parser, so a tool call would come back as plain text — "+
		"name the parser in the plan's extra (%s <name>) or serve a model whose catalog entry names one", engine, modelID, toolParserFlag[engine]))
}

// toolsRefusal is why this worker refuses a request that offers tools, or ""
// when it serves them (or never recorded a launch: a gang's engines are
// started by the leader, which owns that line). tool_choice "none" asks for no
// call, so it is relayed.
func (s *Server) toolsRefusal(tools, toolChoice json.RawMessage) string {
	why, _ := s.toolsRefused.Load().(string)
	if why == "" {
		return ""
	}
	if t := strings.TrimSpace(string(tools)); t == "" || t == "null" || t == "[]" {
		return ""
	}
	if strings.TrimSpace(string(toolChoice)) == `"none"` {
		return ""
	}
	return why
}
