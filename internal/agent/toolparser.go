package agent

import "strings"

// Tool-call parsers (PLAN T19.25). vLLM and SGLang return a model's tool call
// as plain text — `content: "<tool_call>{…}</tool_call>"`, finish_reason stop —
// unless they are started with a parser for that model's family, so a client
// that sent `tools` gets an answer instead of a call. llama.cpp and Ollama
// parse tool calls on their own and need nothing here.
//
// The parser's name comes from the model's catalog entry (`tool_parser`, per
// engine, because the two engines spell one family differently); the flags are
// added only when the entry names one for this engine. The plan wins: an
// `extra` that already names a parser is the operator's choice and is left
// exactly as written.

// ToolParserArgs is the engine's own flags for parser, for an engine started
// with extra as its plan's raw arguments: vLLM `--enable-auto-tool-choice
// --tool-call-parser <parser>`, SGLang `--tool-call-parser <parser>`. Nil when
// parser is empty, the engine takes no such flag, the name is not a plain token,
// or extra already names a parser. Exported for the leader's gang backends,
// which start the same engines over the process contract.
func ToolParserArgs(engine, parser, extra string) []string {
	parser = strings.TrimSpace(parser)
	if parser == "" || !isToken(parser) || hasFlag(extra, "--tool-call-parser") {
		return nil
	}
	switch engine {
	case "vllm":
		args := []string{"--tool-call-parser", parser}
		if !hasFlag(extra, "--enable-auto-tool-choice") {
			args = append([]string{"--enable-auto-tool-choice"}, args...)
		}
		return args
	case "sglang":
		return []string{"--tool-call-parser", parser}
	}
	return nil
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
