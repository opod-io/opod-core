package main

import (
	"strings"
	"testing"
)

// `opod model move` takes exactly one model and both nodes, in any order and
// either flag spelling; anything else is a usage error that says what is wrong.
func TestParseModelMove(t *testing.T) {
	for _, args := range [][]string{
		{"qwen", "--from", "w1", "--to", "w2"},
		{"--to=w2", "--from=w1", "qwen"},
		{"--from", "w1", "qwen", "--to", "w2"},
	} {
		m, err := parseModelMove(args)
		if err != nil || m != (moveArgs{ID: "qwen", From: "w1", To: "w2"}) {
			t.Errorf("%v: %+v, %v", args, m, err)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "required"},
		{[]string{"qwen", "--from", "w1"}, "required"},
		{[]string{"--from", "w1", "--to", "w2"}, "required"},
		{[]string{"qwen", "--from"}, "--from needs a node id"},
		{[]string{"qwen", "--to="}, "--to needs a node id"},
		{[]string{"qwen", "other", "--from", "w1", "--to", "w2"}, "one model at a time"},
		{[]string{"qwen", "--force", "--from", "w1", "--to", "w2"}, "unknown flag"},
	} {
		if _, err := parseModelMove(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want an error mentioning %q", c.args, err, c.want)
		}
	}
}

func TestMoveStepTextCoversEveryStep(t *testing.T) {
	for _, step := range []string{"started", "loaded", "flipped", "drained", "finished", "aborted"} {
		if moveStepText(step, map[string]any{"reason": "r", "state": "s"}) == "" {
			t.Errorf("step %q has no text", step)
		}
	}
	if got := moveStepText("drained", map[string]any{"timed_out": true, "inflight_left": float64(2)}); !strings.Contains(got, "2 request(s)") {
		t.Errorf("a drain that timed out says what was left: %q", got)
	}
}
