package scheduler

// An SGLang pipeline that cannot serve, refused by name before it is formed.
//
// SGLang (read at v0.5.21) gives a Llama-family model with tied word embeddings
// its output layer by pointing `lm_head` at `model.embed_tokens`. Under
// pipeline parallelism only the FIRST stage holds the embedding — every other
// stage has a placeholder there — so the last stage, the one that computes the
// logits, has no output layer. It loads with `Parameter
// model.embed_tokens.weight not found` and dies on the first forward pass with
// `'PPMissingLayer' object has no attribute 'quant_method'` in
// `_compute_lm_head`. Nothing restarts it into working: every re-form crashes
// the same way, on every card it was given.
//
// Qwen2's model file hands the embedding to the last stage itself, and a tied
// Qwen2 gang serves on the same release, so the refusal is by architecture,
// not by "tied". The family is every class that is SGLang's Llama model under
// another name: `llama.py` registers LlamaForCausalLM and three subclasses
// with empty bodies (Phi3, InternLM3, IQuestCoder), and `mistral.py`'s
// MistralForCausalLM is one more. Only the Llama case has been run; the others
// share its code line for line. A class that fixes its own tied head is left
// out — adding a name here is a one-line change, and so is removing one when
// upstream fixes it.
//
// Nothing reads "tied" anywhere else: SGLang downloads the model by repo id
// itself, so the leader asks for config.json — from a complete snapshot on
// disk, else one bounded Hub call. A config that cannot be read never blocks
// a create: the gang forms as it did before this check existed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opod-io/opod/internal/fetch"
	"github.com/opod-io/opod/internal/models"
)

// ReasonSGLangPPTiedLlama names the refusal. It is the create's error.code
// on the admin API and in shard.heal_failed, so it is a contract: never
// renamed or reused for another cause.
const ReasonSGLangPPTiedLlama = "sglang_pp_tied_llama"

// sglangTiedHeadBroken are the architectures whose SGLang model file loses
// a tied output layer on the last pipeline stage (see the file comment).
var sglangTiedHeadBroken = map[string]bool{
	"LlamaForCausalLM":       true,
	"MistralForCausalLM":     true,
	"Phi3ForCausalLM":        true,
	"InternLM3ForCausalLM":   true,
	"IQuestCoderForCausalLM": true,
}

// RefusedError is a create refused by name before anything was touched. It
// is an ErrUnplaceable — the caller reads it as "refused, nothing was
// changed" — and Reason is its machine-readable name.
type RefusedError struct {
	Reason string // ReasonSGLangPPTiedLlama, …
	Msg    string // what a person reads, with the remedy
}

func (e *RefusedError) Error() string { return e.Reason + ": " + e.Msg }

// Unwrap makes errors.Is(err, ErrUnplaceable) true.
func (e *RefusedError) Unwrap() error { return ErrUnplaceable }

// modelConfig is the part of a model's config.json this check reads.
type modelConfig struct {
	Architectures     []string `json:"architectures"`
	TieWordEmbeddings bool     `json:"tie_word_embeddings"` // absent = false, as in transformers' LlamaConfig
}

// checkSGLangPipeline refuses an SGLang gang of more than one pipeline stage
// over a Llama-family model with tied embeddings. Anything else — another
// engine, one stage, a shape the backend will refuse itself, a config that
// cannot be read — passes.
func (o *Orchestrator) checkSGLangPipeline(ctx context.Context, entry models.Entry, parts int, par Parallelism) error {
	if par.Expert || !isSGLangBackend(entry.Sharding.Engine) {
		return nil
	}
	split, err := par.resolve(parts)
	if err != nil || split.PP <= 1 {
		return nil // one stage, or a shape the backend refuses with its own reason
	}
	cfg, err := o.sglangModelConfig(ctx, entry)
	if err != nil {
		o.Log.Warn("could not read the model's config.json before forming an SGLang pipeline — forming it unchecked",
			"model", entry.ID, "pp", split.PP, "reason", fetch.RefusalReason(err), "err", err)
		return nil
	}
	arch := ""
	for _, a := range cfg.Architectures {
		if sglangTiedHeadBroken[a] {
			arch = a
			break
		}
	}
	if arch == "" || !cfg.TieWordEmbeddings {
		return nil
	}
	return &RefusedError{Reason: ReasonSGLangPPTiedLlama, Msg: fmt.Sprintf(
		"model %s (%s, tie_word_embeddings: true) cannot run as an SGLang pipeline (pp=%d): SGLang gives the tied output layer "+
			"to the first stage only, so the last stage crashes loading it (PPMissingLayer in _compute_lm_head). "+
			"Nothing was changed. Use tensor parallelism instead (tp=%d, pp=1), a single stage, or sharding.engine=vllm",
		entry.ID, arch, split.PP, split.TP*split.PP)}
}

// sglangModelConfig reads the config.json of the model an SGLang gang would
// load: from source.path when it is a local directory, else from the repo —
// a complete snapshot under ModelsDir first, then the Hub.
func (o *Orchestrator) sglangModelConfig(ctx context.Context, entry models.Entry) (modelConfig, error) {
	var (
		b   []byte
		err error
	)
	switch {
	case entry.Source.Path != "":
		if st, serr := os.Stat(entry.Source.Path); serr != nil || !st.IsDir() {
			return modelConfig{}, fmt.Errorf("source.path %q is not a local model directory", entry.Source.Path)
		}
		b, err = os.ReadFile(filepath.Join(entry.Source.Path, "config.json"))
	case entry.Source.Repo != "":
		b, err = fetch.HubSmallFile(ctx, entry.Source.Repo, "config.json", o.ModelsDir,
			fetch.Options{HTTP: o.HTTP, Log: o.Log, Token: o.HFToken, Endpoint: o.HFEndpoint})
	default:
		return modelConfig{}, errors.New("the entry names no repo and no path")
	}
	if err != nil {
		return modelConfig{}, err
	}
	var cfg modelConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return modelConfig{}, fmt.Errorf("decode config.json: %w", err)
	}
	return cfg, nil
}
