package engines

import (
	"encoding/json"
	"strconv"
)

// AppendChatDeltaChunk appends one streamed chat delta chunk — the chunk that
// runs once per token — as exactly the bytes encoding/json produces for the
// leader's struct-shaped chunk:
//
//	{"id":ID,"object":"chat.completion.chunk","created":N,"model":M,"choices":[{"index":0,"delta":{"content":C},"finish_reason":null}]}
//
// Only the three strings go through json.Marshal, which is what does the
// escaping (quotes, control characters, invalid UTF-8, the HTML-safe \u003c
// forms, U+2028/9); the fixed structure is appended by hand. The worker's
// encoder built a nested map[string]any per token and marshalled THAT, which
// sorts keys on every call: 1658 ns / 30 allocs per token against ~105 ns / 1
// alloc for this (PLAN T15.13). The role, final and usage chunks run once per
// stream and keep encoding/json. Byte equality with the struct encoding is
// held by a test over a corpus of deltas, because these bytes are the
// leader's frozen contract.
func AppendChatDeltaChunk(dst []byte, id string, created int64, model, content string) []byte {
	dst = append(dst, `{"id":`...)
	dst = appendJSONString(dst, id)
	dst = append(dst, `,"object":"chat.completion.chunk","created":`...)
	dst = strconv.AppendInt(dst, created, 10)
	dst = append(dst, `,"model":`...)
	dst = appendJSONString(dst, model)
	dst = append(dst, `,"choices":[{"index":0,"delta":{"content":`...)
	dst = appendJSONString(dst, content)
	dst = append(dst, `},"finish_reason":null}]}`...)
	return dst
}

// appendJSONString appends s as encoding/json would encode it. A string of
// plain printable ASCII with nothing json escapes — no quote, backslash,
// control byte, or one of the HTML-unsafe `<>&` encoding/json rewrites as
// \u003c — is quoted in place; anything else goes through json.Marshal, which
// cannot fail on a string. The fast path is the common token; the slow path
// is what keeps the bytes identical for everything else.
func appendJSONString(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			b, _ := json.Marshal(s)
			return append(dst, b...)
		}
	}
	dst = append(dst, '"')
	dst = append(dst, s...)
	return append(dst, '"')
}

// AppendSSEData frames one event: "data: " + chunk + blank line.
func AppendSSEData(dst, chunk []byte) []byte {
	dst = append(dst, "data: "...)
	dst = append(dst, chunk...)
	return append(dst, "\n\n"...)
}
