package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/opod-io/opod/internal/engines"
)

// Tokenize asks the server for the prompt's token ids (POST /tokenize, the
// route vLLM and SGLang both serve with the same shape): the chat template
// applied with the generation prompt — the ids the model is fed, and so the
// ids its prefix cache is keyed by. limit > 0 keeps the leading ids only.
func Tokenize(ctx context.Context, c Client, model string, messages []engines.Message, prompt string, limit int) ([]int, error) {
	body := map[string]any{"model": model}
	if len(messages) > 0 {
		msgs := make([]map[string]string, 0, len(messages))
		for _, m := range messages {
			msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
		}
		body["messages"] = msgs
		body["add_generation_prompt"] = true
	} else {
		body["prompt"] = prompt
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/tokenize", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, engines.Unreachable(c.Driver, c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, engines.Upstream(c.Driver, "POST /tokenize", resp.StatusCode, nil)
	}
	var out struct {
		Tokens []int `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if limit > 0 && len(out.Tokens) > limit {
		out.Tokens = out.Tokens[:limit]
	}
	return out.Tokens, nil
}
