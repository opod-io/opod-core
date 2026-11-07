package adminapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// The T23.11 and T24.25 fields are absent when the leader does not say, and
// carry their value when it does: a zero is never "not said".
func TestLoadAdmissionBoundAndEngineTTFTWire(t *testing.T) {
	raw, err := json.Marshal(Load{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"admission_bound", "engine_ttft_p50_ms"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("an unsaid %s is on the wire: %s", key, raw)
		}
	}
	ms := int64(41)
	raw, err = json.Marshal(Load{AdmissionBound: AdmissionBoundKV, EngineTTFTP50Ms: &ms})
	if err != nil {
		t.Fatal(err)
	}
	var back Load
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.AdmissionBound != "kv" || back.EngineTTFTP50Ms == nil || *back.EngineTTFTP50Ms != 41 {
		t.Fatalf("round trip lost a field: %s → %+v", raw, back)
	}
}
