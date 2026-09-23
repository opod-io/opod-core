package images

import (
	"os"
	"strings"
	"testing"
)

// The four-place drift rule, extended to the family axis (T8.1): a family
// declared in images.yaml must be a token the readers recognise, and build.sh
// must actually stamp it on the image — a declared family nobody labels refuses
// nothing, and a labelled family nobody declares cannot be reviewed.
func TestEveryDeclaredFamilyIsAToken(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// The vendors this project ships for. A family's prefix must be one of them,
	// or the control plane's own registry cannot match it to a card.
	vendors := map[string]bool{"nvidia": true, "amd": true, "intel": true, "tt": true}
	var withFamilies int
	for _, img := range m.Images {
		if img.Role != "worker" {
			if len(img.Families) > 0 {
				t.Errorf("%s is not a worker and must not claim GPU families", img.Name)
			}
			continue
		}
		if len(img.Families) == 0 {
			continue // says nothing, which reads as unknown everywhere
		}
		withFamilies++
		for _, f := range img.Families {
			vendor, gen, ok := strings.Cut(f, ":")
			if !ok || gen == "" {
				t.Errorf("%s: family %q must be <vendor>:<generation>", img.Name, f)
				continue
			}
			if !vendors[vendor] {
				t.Errorf("%s: family %q names a vendor this project does not ship for", img.Name, f)
			}
			if f != strings.ToLower(f) || strings.ContainsAny(f, " _") {
				t.Errorf("%s: family %q must be a lower-case token with no spaces or underscores", img.Name, f)
			}
			// And it must belong to the image's OWN vendor: an AMD image
			// claiming an NVIDIA family would be matched against the wrong cards.
			if img.Vendor != "" && vendor != img.Vendor {
				t.Errorf("%s (vendor %s) claims family %q of another vendor", img.Name, img.Vendor, f)
			}
		}
	}
	if withFamilies == 0 {
		t.Fatal("no worker image declares its families — the axis is inert")
	}
}

// build.sh must read them from this manifest and stamp them. A declaration
// nobody labels is a refusal that never fires.
func TestBuildScriptStampsTheFamilyLabel(t *testing.T) {
	b, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{"io.opod.gpu.families", "images/images.yaml"} {
		if !strings.Contains(src, want) {
			t.Errorf("build.sh must stamp the families read from the manifest (%q missing)", want)
		}
	}
	// It must read them, not repeat them: a copy of the list in the build script
	// would be a fifth place to forget.
	if strings.Contains(src, "nvidia:ampere") {
		t.Error("build.sh repeats a family list — read it from images.yaml instead")
	}
}
