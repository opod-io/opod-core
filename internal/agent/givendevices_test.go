package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/opod-io/opod-sdk/nodeapi"
)

// fakeHost builds a /dev and a /sys the way a container sees them: sysfs is
// the HOST's (every card on the machine is under /sys/class), while /dev holds
// only the nodes the device plugin handed this container.
type fakeHost struct {
	t        *testing.T
	dev, sys string
}

func newFakeHost(t *testing.T) fakeHost {
	root := t.TempDir()
	h := fakeHost{t: t, dev: filepath.Join(root, "dev"), sys: filepath.Join(root, "sys")}
	for _, d := range []string{h.dev, h.sys} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// pciDevice creates /sys/devices/pci0000:00/<bdf> with its vendor id.
func (h fakeHost) pciDevice(bdf, vendor string) string {
	dir := filepath.Join(h.sys, "devices", "pci0000:00", bdf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vendor"), []byte(vendor+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return dir
}

// classEntry creates /sys/class/<class>/<name>/device → the PCI device, as a
// RELATIVE link, the way sysfs spells it.
func (h fakeHost) classEntry(class, name, pciDir string) {
	dir := filepath.Join(h.sys, "class", class, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, pciDir)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(dir, "device")); err != nil {
		h.t.Fatal(err)
	}
}

// devNode creates a node in the container's /dev (a plain file stands in for
// the character device: only its presence is read).
func (h fakeHost) devNode(rel string) {
	p := filepath.Join(h.dev, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h fakeHost) sources() deviceSources {
	return deviceSources{devRoot: h.dev, sysRoot: h.sys}
}

// The row this exists for: a three-card AMD host, one whole-card worker
// handed the SECOND card. sysfs lists all three; the container's /dev holds
// one render node (and the shared /dev/kfd, which names no card). The worker
// must state that one card, by address — never the three the host has.
func TestGivenDevicesAMDReadsOnlyTheContainersNodes(t *testing.T) {
	h := newFakeHost(t)
	for i, bdf := range []string{"0000:03:00.0", "0000:43:00.0", "0000:83:00.0"} {
		d := h.pciDevice(bdf, "0x1002")
		h.classEntry("drm", fmt.Sprintf("renderD%d", 128+i), d) // every card is in the host's sysfs
		h.classEntry("drm", fmt.Sprintf("card%d", i), d)
	}
	h.devNode("kfd")
	h.devNode("dri/renderD129")
	h.devNode("dri/card1")

	got := givenDevices(h.sources())
	want := []nodeapi.GivenDevice{{Index: 0, Vendor: nodeapi.VendorAMD, PCI: "0000:43:00.0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("given = %+v\nwant  %+v", got, want)
	}
}

// Intel: the same DRM walk, the vendor read off the PCI device. Two cards
// handed to one worker come back in address order, numbered from 0.
func TestGivenDevicesIntelTwoCards(t *testing.T) {
	h := newFakeHost(t)
	a := h.pciDevice("0000:0a:00.0", "0x8086")
	b := h.pciDevice("0000:05:00.0", "0x8086")
	h.classEntry("drm", "renderD130", a)
	h.classEntry("drm", "renderD131", b)
	h.devNode("dri/renderD130")
	h.devNode("dri/renderD131")

	got := givenDevices(h.sources())
	want := []nodeapi.GivenDevice{
		{Index: 0, Vendor: nodeapi.VendorIntel, PCI: "0000:05:00.0"},
		{Index: 1, Vendor: nodeapi.VendorIntel, PCI: "0000:0a:00.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("given = %+v\nwant  %+v", got, want)
	}
}

// A display controller passed into the container that is not an accelerator
// (a board management controller's VGA) is not stated as one.
func TestGivenDevicesSkipsAForeignDisplayController(t *testing.T) {
	h := newFakeHost(t)
	h.classEntry("drm", "card0", h.pciDevice("0000:02:00.0", "0x1a03"))
	h.devNode("dri/card0")
	if got := givenDevices(h.sources()); len(got) != 0 {
		t.Fatalf("a non-accelerator DRM node was stated: %+v", got)
	}
}

// Tenstorrent: /dev/tenstorrent/N resolved through /sys/class/tenstorrent,
// under either spelling of the class entry. A chip whose address sysfs does
// not give is still stated, without one.
func TestGivenDevicesTenstorrent(t *testing.T) {
	h := newFakeHost(t)
	for i, bdf := range []string{"0000:01:00.0", "0000:02:00.0", "0000:03:00.0", "0000:04:00.0"} {
		d := h.pciDevice(bdf, "0x1e52")
		name := fmt.Sprint(i)
		if i%2 == 1 {
			name = "tenstorrent!" + name
		}
		h.classEntry("tenstorrent", name, d)
	}
	h.devNode("tenstorrent/1")
	h.devNode("tenstorrent/2")

	got := givenDevices(h.sources())
	want := []nodeapi.GivenDevice{
		{Index: 0, Vendor: nodeapi.VendorTT, PCI: "0000:02:00.0"},
		{Index: 1, Vendor: nodeapi.VendorTT, PCI: "0000:03:00.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("given = %+v\nwant  %+v", got, want)
	}

	h.devNode("tenstorrent/7") // no class entry at all
	got = givenDevices(h.sources())
	if len(got) != 3 || got[0].PCI != "" || got[0].Vendor != nodeapi.VendorTT {
		t.Fatalf("a chip with no sysfs address must be stated without one, got %+v", got)
	}
}

// NVIDIA: nvidia-smi inside the container enumerates only what the runtime
// exposed, with the container's own index — the address and UUID are what
// name the card. The DRM node the runtime may also inject for the same card is
// not a second device.
func TestGivenDevicesNVIDIAFromTheTool(t *testing.T) {
	h := newFakeHost(t)
	h.classEntry("drm", "renderD129", h.pciDevice("0000:41:00.0", "0x10de"))
	h.devNode("dri/renderD129")
	s := h.sources()
	s.nvidiaSMI = func() (string, error) {
		return "0, GPU-6f1c2a3b-0000-4000-8000-000000000001, 00000000:41:00.0\n", nil
	}
	got := givenDevices(s)
	want := []nodeapi.GivenDevice{{Index: 0, Vendor: nodeapi.VendorNVIDIA, PCI: "0000:41:00.0", UUID: "GPU-6f1c2a3b-0000-4000-8000-000000000001"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("given = %+v\nwant  %+v", got, want)
	}
}

// NVIDIA with no tooling in the image: the driver's per-card directories,
// which the container runtime narrows to what it exposed — the address from
// the directory name, the UUID from the file.
func TestGivenDevicesNVIDIAFromTheDriversProcTree(t *testing.T) {
	h := newFakeHost(t)
	s := h.sources()
	s.nvidiaSMI = func() (string, error) { return "", errors.New("exec: nvidia-smi: not found") }
	s.procRoot = filepath.Join(filepath.Dir(h.dev), "proc")
	for bdf, uuid := range map[string]string{"0000:41:00.0": "GPU-bbbb", "0000:01:00.0": "GPU-aaaa"} {
		dir := filepath.Join(s.procRoot, "driver", "nvidia", "gpus", bdf)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		info := "Model: \t\t NVIDIA RTX A6000\nIRQ:   \t\t 186\nGPU UUID: \t GPU-" + uuid[4:] + "\nBus Location: \t " + bdf + "\n"
		if err := os.WriteFile(filepath.Join(dir, "information"), []byte(info), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := givenDevices(s)
	want := []nodeapi.GivenDevice{
		{Index: 0, Vendor: nodeapi.VendorNVIDIA, PCI: "0000:01:00.0", UUID: "GPU-aaaa"},
		{Index: 1, Vendor: nodeapi.VendorNVIDIA, PCI: "0000:41:00.0", UUID: "GPU-bbbb"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("given = %+v\nwant  %+v", got, want)
	}
}

// A CPU-only part looked and was given nothing: an EMPTY list, never nil —
// on the wire that is "none given", where nil would be "no statement".
func TestGivenDevicesNoneIsAnEmptyList(t *testing.T) {
	got := givenDevices(newFakeHost(t).sources())
	if got == nil || len(got) != 0 {
		t.Fatalf("given = %#v, want an empty, non-nil list", got)
	}
}

// The agent's Capabilities and the SDK's are one document: what a worker
// registers decodes into nodeapi.Capabilities with the devices intact, and a
// worker that states nothing puts no key on the wire.
func TestCapabilitiesGivenDevicesMatchTheSDK(t *testing.T) {
	devs := []nodeapi.GivenDevice{{Index: 0, Vendor: nodeapi.VendorAMD, PCI: "0000:43:00.0"}}
	raw, err := json.Marshal(Capabilities{Hostname: "w", GivenDevices: &devs})
	if err != nil {
		t.Fatal(err)
	}
	back, err := nodeapi.DecodeHardware(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if back.GivenDevices == nil || !reflect.DeepEqual(*back.GivenDevices, devs) {
		t.Fatalf("decoded %+v from %s", back.GivenDevices, raw)
	}
	raw, _ = json.Marshal(Capabilities{Hostname: "w"})
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["GivenDevices"]; ok {
		t.Fatalf("no statement must put no key on the wire: %s", raw)
	}
}
