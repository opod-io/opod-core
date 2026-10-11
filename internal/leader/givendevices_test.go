package leader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/opod-io/opod-sdk/nodeapi"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// Feature worker_devices: the devices a worker states it was given reach a
// manager through GET /admin/v1/nodes exactly as the worker sent them. The
// leader decides nothing with them — it is the channel, so the test is that
// nothing on the way drops or rewrites the list, and that a worker which
// states nothing still reads as no statement.
func TestNodesListingCarriesGivenDevices(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.RequireKeys = false
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := NewServer(cfg, st, &downEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx := context.Background()

	given := []nodeapi.GivenDevice{{Index: 0, Vendor: nodeapi.VendorAMD, PCI: "0000:43:00.0"}}
	withDevs, err := nodeapi.EncodeHardware(nodeapi.Capabilities{Hostname: "w-given", OS: "linux", GivenDevices: &given})
	if err != nil {
		t.Fatal(err)
	}
	older, _ := nodeapi.EncodeHardware(nodeapi.Capabilities{Hostname: "w-older", OS: "linux"})
	for id, hw := range map[string]string{"w-given": withDevs, "w-older": older} {
		if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: id, Hostname: id, Address: "192.0.2.1:8081", HardwareJSON: hw}, Caller{Admin: true}, "tok"); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	srv.listNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/nodes", nil))
	var rows []struct {
		ID           string `json:"ID"`
		HardwareJSON string `json:"HardwareJSON"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode listing: %v\n%s", err, rec.Body.String())
	}
	seen := 0
	for _, r := range rows {
		caps, err := nodeapi.DecodeHardware(r.HardwareJSON)
		if err != nil {
			t.Fatalf("%s: hardware_json does not decode: %v", r.ID, err)
		}
		switch r.ID {
		case "w-given":
			seen++
			if caps.GivenDevices == nil || !reflect.DeepEqual(*caps.GivenDevices, given) {
				t.Errorf("w-given: listing carries %+v, want %+v", caps.GivenDevices, given)
			}
		case "w-older":
			seen++
			if caps.GivenDevices != nil {
				t.Errorf("w-older stated nothing, listing reads %+v", *caps.GivenDevices)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("listing has %d of the two workers: %s", seen, rec.Body.String())
	}
}
