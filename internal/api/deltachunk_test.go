package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// deltaCorpus is every shape a delta has been seen to take, plus the ones
// that break hand-written JSON: quotes, backslashes, newlines and control
// characters, invalid UTF-8, the HTML-unsafe runes encoding/json escapes,
// U+2028/U+2029, emoji, empty, and a 100 KB single token.
func deltaCorpus() []string {
	return []string{
		"", "hello", " world", "\"quoted\"", `back\slash`, "line\nbreak", "tab\tcr\r", "\x00\x01\x1f",
		"bad utf8 \xff\xfe", "<script>&amp;</script>", "\u2028\u2029", "🚀 emoji 👩‍👩‍👧", "日本語",
		strings.Repeat("x", 100*1024), "mixed \"\\\n\x07 \xc3\x28 <b> \u2028",
	}
}

// The hand-appended delta chunk is BYTE-IDENTICAL to encoding/json's encoding
// of the leader's struct — these bytes are the frozen leader contract, so
// "semantically equal" is not enough (PLAN T15.13).
func TestDeltaChunkBytesEqualTheStructEncoding(t *testing.T) {
	for _, model := range []string{"m", "org/model:q4", "a\"b<c>"} {
		for _, d := range deltaCorpus() {
			want, _ := json.Marshal(chatChunk{
				ID: "chatcmpl-1", Object: "chat.completion.chunk", Created: 1700000000, Model: model,
				Choices: []chatChunkChoice{{Index: 0, Delta: map[string]any{"content": d}}},
			})
			got := engines.AppendChatDeltaChunk(nil, "chatcmpl-1", 1700000000, model, d)
			if !bytes.Equal(got, want) {
				t.Fatalf("delta %q model %q:\n got %s\nwant %s", d, model, got, want)
			}
		}
	}
}

func FuzzDeltaChunkBytes(f *testing.F) {
	for _, d := range deltaCorpus() {
		f.Add(d)
	}
	f.Fuzz(func(t *testing.T, d string) {
		want, _ := json.Marshal(chatChunk{ID: "id", Object: "chat.completion.chunk", Created: 7, Model: "m",
			Choices: []chatChunkChoice{{Index: 0, Delta: map[string]any{"content": d}}}})
		if got := engines.AppendChatDeltaChunk(nil, "id", 7, "m", d); !bytes.Equal(got, want) {
			t.Fatalf("delta %q:\n got %s\nwant %s", d, got, want)
		}
	})
}

func BenchmarkDeltaChunkByHand(b *testing.B) {
	var buf []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf = engines.AppendChatDeltaChunk(buf[:0], "chatcmpl-1", 1700000000, "m", "token")
	}
}

func BenchmarkDeltaChunkStruct(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(chatChunk{ID: "chatcmpl-1", Object: "chat.completion.chunk", Created: 1700000000, Model: "m",
			Choices: []chatChunkChoice{{Index: 0, Delta: map[string]any{"content": "token"}}}})
	}
}

func BenchmarkDeltaChunkMap(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": int64(1700000000), "model": "m",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "token"}, "finish_reason": nil}},
		})
	}
}
