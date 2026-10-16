package agent

import (
	"reflect"
	"testing"
)

// The child's output reaches the ring buffer line by line however the writes
// are cut, and a last line with no newline is not lost: the reaper flushes it
// before it records the exit, so the exit carries it.
func TestLineWriterRecordsWholeLines(t *testing.T) {
	buf := newRingBuffer(8)
	w := &lineWriter{dst: buf, id: "e"}
	for _, chunk := range []string{"load", "ing model\r\n", "GGML_ASSERT: rpc", " backend unreachable\nabort"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := buf.tail(0), []string{"loading model", "GGML_ASSERT: rpc backend unreachable"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before flush: %q, want %q", got, want)
	}
	w.flush()
	if got := buf.tail(1); !reflect.DeepEqual(got, []string{"abort"}) {
		t.Fatalf("the unterminated last line must be recorded on flush: %q", got)
	}
	w.flush()
	if got := buf.tail(0); len(got) != 3 {
		t.Fatalf("a second flush records nothing: %q", got)
	}
}
