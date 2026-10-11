package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/opod-io/opod-sdk/nodeapi"
)

// The accelerators this worker process was GIVEN (feature worker_devices).
//
// A worker started under a device plugin sees only the devices the plugin
// handed its container: the device nodes in its /dev, and whatever its vendor
// runtime enumerates. Which physical cards those are is the one thing the
// process that asked for them is never told, and the worker is the only party
// that can read it — so it reads it here and states it on register.
//
// Everything is read from inside the sandbox, never from a host-wide list:
// /sys is the host's (sysfs is not namespaced for devices), so a walk of
// /sys/class/drm would list every card on the machine. The device NODES are
// what is scoped to the container; sysfs is only consulted to turn a node the
// container holds into a PCI address.

// deviceSources is where givenDevices reads from. Production uses the real
// roots and nvidia-smi; tests point it at a fake tree.
type deviceSources struct {
	devRoot string // "/dev"
	sysRoot string // "/sys"
	// nvidiaSMI runs `nvidia-smi --query-gpu=index,uuid,pci.bus_id
	// --format=csv,noheader`; NVML enumerates only the devices the container
	// runtime made visible. nil or an error = no NVIDIA tooling here.
	nvidiaSMI func() (string, error)
	// procRoot is "/proc". The NVIDIA container runtime masks
	// /proc/driver/nvidia/gpus to the devices it exposed (one directory per
	// card, named by PCI address), which is the fallback when the image
	// carries no nvidia-smi.
	procRoot string
}

// PCI vendor ids (sysfs <device>/vendor) of the accelerators a DRM node can
// belong to.
var drmVendors = map[string]string{
	"0x1002": nodeapi.VendorAMD,
	"0x8086": nodeapi.VendorIntel,
	"0x10de": nodeapi.VendorNVIDIA,
}

// givenDevices returns the accelerators this process can reach, deduplicated
// by PCI address. It never returns nil: a worker that looked and found
// nothing says so with an empty list (nodeapi.Capabilities.GivenDevices).
func givenDevices(s deviceSources) []nodeapi.GivenDevice {
	out := []nodeapi.GivenDevice{}
	seen := map[string]bool{}
	add := func(d nodeapi.GivenDevice) {
		if d.PCI != "" {
			if seen[d.PCI] {
				return
			}
			seen[d.PCI] = true
		}
		out = append(out, d)
	}
	for _, d := range nvidiaGiven(s) {
		add(d)
	}
	for _, d := range indexed(drmGiven(s)) {
		add(d)
	}
	for _, d := range indexed(ttGiven(s)) {
		add(d)
	}
	return out
}

// nvidiaGiven reads the NVIDIA devices the container runtime exposed: from
// nvidia-smi when it runs (index, UUID and PCI address), else the driver's
// per-card /proc directories.
func nvidiaGiven(s deviceSources) []nodeapi.GivenDevice {
	if s.nvidiaSMI != nil {
		if out, err := s.nvidiaSMI(); err == nil {
			var devs []nodeapi.GivenDevice
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				f := strings.Split(line, ",")
				if len(f) < 3 {
					continue
				}
				idx := atoiOr(strings.TrimSpace(f[0]), len(devs))
				devs = append(devs, nodeapi.GivenDevice{
					Index:  idx,
					Vendor: nodeapi.VendorNVIDIA,
					UUID:   strings.TrimSpace(f[1]),
					PCI:    nodeapi.NormalizePCI(f[2]),
				})
			}
			if len(devs) > 0 {
				return devs
			}
		}
	}
	// No tooling: the driver's own per-card directories, which the container
	// runtime narrows to what it exposed.
	return nvidiaProcGiven(s)
}

// nvidiaProcGiven reads /proc/driver/nvidia/gpus/<pci>/information: the
// directory name is the card's address and the file carries its UUID.
func nvidiaProcGiven(s deviceSources) []nodeapi.GivenDevice {
	if s.procRoot == "" {
		return nil
	}
	dir := filepath.Join(s.procRoot, "driver", "nvidia", "gpus")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var devs []nodeapi.GivenDevice
	for _, e := range entries {
		pci := nodeapi.NormalizePCI(e.Name())
		if strings.Count(pci, ":") != 2 {
			continue
		}
		d := nodeapi.GivenDevice{Vendor: nodeapi.VendorNVIDIA, PCI: pci}
		if b, err := os.ReadFile(filepath.Join(dir, e.Name(), "information")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "GPU UUID" {
					d.UUID = strings.TrimSpace(v)
				}
			}
		}
		devs = append(devs, d)
	}
	return indexed(devs)
}

// drmGiven reads the AMD and Intel devices the container holds: every DRM
// node in its /dev/dri (renderD* and card* — a plugin may pass either), each
// resolved to its PCI device through the host's /sys/class/drm.
func drmGiven(s deviceSources) []nodeapi.GivenDevice {
	entries, err := os.ReadDir(filepath.Join(s.devRoot, "dri"))
	if err != nil {
		return nil
	}
	var devs []nodeapi.GivenDevice
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "renderD") && !strings.HasPrefix(name, "card") {
			continue
		}
		pciDir := filepath.Join(s.sysRoot, "class", "drm", name, "device")
		pci := pciOf(pciDir)
		if pci == "" {
			continue
		}
		vendor := drmVendors[readTrim(filepath.Join(pciDir, "vendor"))]
		if vendor == "" {
			continue // a display controller that is not an accelerator (a BMC's VGA)
		}
		devs = append(devs, nodeapi.GivenDevice{Vendor: vendor, PCI: pci})
	}
	return devs
}

// ttGiven reads the Tenstorrent devices the container holds: /dev/tenstorrent/N,
// resolved through the host's /sys/class/tenstorrent. The kernel spells the
// class entry of a nested node name with "!" for "/", so both spellings are
// tried.
func ttGiven(s deviceSources) []nodeapi.GivenDevice {
	entries, err := os.ReadDir(filepath.Join(s.devRoot, "tenstorrent"))
	if err != nil {
		return nil
	}
	var devs []nodeapi.GivenDevice
	for _, e := range entries {
		n := e.Name()
		pci := ""
		for _, cls := range []string{n, "tenstorrent!" + n} {
			if pci = pciOf(filepath.Join(s.sysRoot, "class", "tenstorrent", cls, "device")); pci != "" {
				break
			}
		}
		// A node the container holds but whose address sysfs does not give is
		// still a device it was given; it is stated without an address rather
		// than dropped, and a reader that matches on PCI simply cannot use it.
		devs = append(devs, nodeapi.GivenDevice{Vendor: nodeapi.VendorTT, PCI: pci})
	}
	return devs
}

// indexed dedupes devices by PCI address, orders them by it, and numbers them
// per vendor in that order — the worker's own enumeration, which is all Index
// claims to be.
func indexed(devs []nodeapi.GivenDevice) []nodeapi.GivenDevice {
	seen := map[string]bool{}
	var out []nodeapi.GivenDevice
	for _, d := range devs {
		if d.PCI != "" && seen[d.PCI] {
			continue
		}
		seen[d.PCI] = true
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PCI < out[j].PCI })
	next := map[string]int{}
	for i := range out {
		out[i].Index = next[out[i].Vendor]
		next[out[i].Vendor]++
	}
	return out
}

// pciOf resolves a sysfs "device" link to the PCI address it points at
// ("…/0000:03:00.0"), or "" when it is not a link to a PCI device.
func pciOf(deviceLink string) string {
	target, err := os.Readlink(deviceLink)
	if err != nil {
		return ""
	}
	base := nodeapi.NormalizePCI(filepath.Base(target))
	if strings.Count(base, ":") != 2 || !strings.Contains(base, ".") {
		return ""
	}
	return base
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(b)))
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}
