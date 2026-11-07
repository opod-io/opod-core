package sglang

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// PLAN T29.5: SGLang states its version in /server_info (0.5.21 on the
// design-partner cell); a release that only has /get_server_info answers the
// same body there.
func TestVersionIsServerInfosVersion(t *testing.T) {
	for _, path := range []string{"/server_info", "/get_server_info"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"model_path":"/data/models/m","weight_version":"default","version":"0.5.21"}`))
		}))
		v, err := New(srv.URL, "").Version(context.Background())
		srv.Close()
		if err != nil || v != "0.5.21" {
			t.Fatalf("%s: version %q %v", path, v, err)
		}
	}
}
