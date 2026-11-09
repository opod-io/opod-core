package agent

import (
	"strings"
	"testing"
)

// ADR-120: a plan with no draft, or a draft already on the node, is left
// alone; a llama.cpp draft must name its file; an engine with no draft says so.
func TestResolveSpecDraftLeavesWhatNeedsNoFetch(t *testing.T) {
	for _, flags := range []string{`{}`, `{"spec_method":"ngram"}`, `{"spec_method":"draft","spec_draft":"/data/models/draft.gguf"}`} {
		s := &Server{EngineFlags: ParseEngineFlags(flags), ModelsDir: t.TempDir()}
		before := s.EngineFlags["spec_draft"]
		if err := s.resolveSpecDraft("llamacpp"); err != nil || s.EngineFlags["spec_draft"] != before {
			t.Errorf("%s: err %v, spec_draft %q → %q", flags, err, before, s.EngineFlags["spec_draft"])
		}
	}
	s := &Server{EngineFlags: ParseEngineFlags(`{"spec_method":"draft","spec_draft":"org/draft-gguf"}`), ModelsDir: t.TempDir()}
	if err := s.resolveSpecDraft("llamacpp"); err == nil || !strings.Contains(err.Error(), "<repo>:<file>.gguf") {
		t.Errorf("a llama.cpp draft with no file: %v", err)
	}
	s = &Server{EngineFlags: ParseEngineFlags(`{"spec_method":"draft","spec_draft":"org/draft"}`), ModelsDir: t.TempDir()}
	if err := s.resolveSpecDraft("trtllm"); err == nil || !strings.Contains(err.Error(), "takes no draft") {
		t.Errorf("an engine with no draft: %v", err)
	}
	s = &Server{EngineFlags: ParseEngineFlags(`{"spec_method":"draft","spec_draft":"org/draft"}`)}
	if err := s.resolveSpecDraft("vllm"); err == nil || !strings.Contains(err.Error(), "no models directory") {
		t.Errorf("no models directory: %v", err)
	}
}
