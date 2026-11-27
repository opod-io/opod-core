package leader

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod-core/pkg/adminapi"
)

// capChat posts one chat request for "m" (lowered to class when it is set) and
// returns the answer's status and the gate's two headers.
func capChat(t *testing.T, ts *httptest.Server, class string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if class != "" {
		req.Header.Set(classHeader, class)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get(adminapi.HeaderAdmissionCap), resp.Header.Get(adminapi.HeaderAdmission)
}

// An admitted answer states the caller's class cap — the share canGrant
// enforces — and the number follows the slots a heartbeat reports.
func TestAdmissionCapHeaderFollowsClassAndSlots(t *testing.T) {
	srv := slottedLeader(t, 8, PolicyAdmission{HoldMs: 5000, Classes: []PolicyAdmissionClass{
		{Name: "critical"}, {Name: "standard"}, {Name: "sheddable"},
	}})
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	for _, tc := range []struct {
		class, want string
	}{
		{"", "8"},          // standard: share 1 of 8
		{"sheddable", "2"}, // the default share 0.25 of 8
	} {
		_, got, shed := capChat(t, ts, tc.class)
		if got != tc.want || shed != "" {
			t.Fatalf("class %q: want cap %s and no shed marker, got cap %q marker %q", tc.class, tc.want, got, shed)
		}
	}

	beat(t, srv, "w1", 16)
	srv.capacityChanged()
	if _, got, _ := capChat(t, ts, "sheddable"); got != "4" {
		t.Fatalf("16 slots: a sheddable caller's cap is 4, got %q", got)
	}
}

// An ungoverned worker makes admission unbounded: there is no cap to state.
func TestAdmissionCapHeaderAbsentWhenUnbounded(t *testing.T) {
	srv := slottedLeader(t, 0, PolicyAdmission{HoldMs: 5000})
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	if _, got, _ := capChat(t, ts, ""); got != "" {
		t.Fatalf("an ungoverned pool has no cap, got %q", got)
	}
}

// A 503 the gate writes is marked shed: the request reached no router.
func TestAdmissionShedIsMarked(t *testing.T) {
	srv := drainedLeader(t)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p0"})
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	code, capHdr, shed := capChat(t, ts, "")
	if code != http.StatusServiceUnavailable || shed != adminapi.AdmissionShed {
		t.Fatalf("want 503 marked %q, got %d marker %q", adminapi.AdmissionShed, code, shed)
	}
	if capHdr != "" {
		t.Fatalf("a shed answer states no cap, got %q", capHdr)
	}
}
