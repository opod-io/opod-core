package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/models"
)

// An image for a catalog model without `vision` is refused by name; a vision
// model's image reaches the engine whole (T16.2).
func TestImagesAreGatedOnTheCatalog(t *testing.T) {
	body := `{"model":"%s","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`
	eng := &recordingEngine{}
	h := &Handler{Engine: eng, Store: usageTestStore(t), Catalog: []models.Entry{
		{ID: "text-only", Capabilities: []string{"chat", "tools"}},
		{ID: "seer", Capabilities: []string{"chat", "vision"}},
		{ID: "owners-own", Capabilities: []string{"chat"}, Tags: []string{"sharded", "user"}},
	}}
	rec := postChat(t, h, strings.Replace(body, "%s", "text-only", 1))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_request") || !strings.Contains(rec.Body.String(), "image_url") {
		t.Fatalf("text-only model: %d %s", rec.Code, rec.Body)
	}
	if eng.calls != 0 {
		t.Fatal("a refused image reached the engine")
	}
	rec = postChat(t, h, strings.Replace(body, "%s", "seer", 1))
	if rec.Code != http.StatusOK || eng.got == nil || len(eng.got.Messages[0].Images) != 1 || eng.got.Messages[0].Images[0] != "data:image/png;base64,AAA" {
		t.Fatalf("vision model: %d %s %+v", rec.Code, rec.Body, eng.got)
	}
	// A model the catalog does not know is passed on — and so is an entry a
	// manager wrote for its owner's model: its engine answers.
	for _, m := range []string{"unknown-model", "owners-own"} {
		if rec = postChat(t, h, strings.Replace(body, "%s", m, 1)); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", m, rec.Code, rec.Body)
		}
	}
}
