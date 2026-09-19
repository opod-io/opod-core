package main

import (
	"flag"
	"fmt"
	"regexp"
	"strings"

	"github.com/opod-io/opod/images"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
)

// opod image ls | show <image> | recommend <model> — the container images this
// repository publishes, from the manifest embedded in the binary
// (images/images.yaml). Read-only and offline: it opens no config, no store and
// no registry, so it answers the same on a laptop, in CI and for an agent
// planning a deployment. Every sub-verb takes --json.
func cmdImage(args []string) {
	help := helpSpec{
		name:    "image",
		summary: "the container images opod publishes, and which one serves a model",
		usage:   "opod image ls [--engine E] [--vendor V] [--json]\n  opod image show <image> [--json]\n  opod image recommend <model> [--vendor V] [--json]",
		examples: []string{
			"opod image ls                                  # every image: engine, vendor, platforms, gang, proven",
			"opod image ls --vendor amd --json              # what runs on AMD, for a script or an agent",
			"opod image show vllm-nvidia                    # base, node requirements, limits, how to pin it",
			"opod image recommend llama-3.1-8b --vendor nvidia",
			"opod image recommend hf:Qwen/Qwen2.5-7B-Instruct --json",
		},
		notes: []string{
			"Vendors: nvidia, amd, intel, tt (Tenstorrent), cpu. GANG is how a worker on the image takes part",
			"in a model split across nodes: rpc (llama.cpp RPC parts), ray (vLLM multi-node), - (one node only).",
			"PROVEN means the image has served requests on the hardware it names.",
			"recommend is a lookup — the model's recommended engines against the images — not a measurement:",
			"whether the model fits a card is `opod model info <id>` (min VRAM) and your call.",
			"Pin what you deploy by digest: `crane digest <repository>:<tag>` or `docker buildx imagetools inspect`.",
		},
	}
	if len(args) == 0 {
		dieHelp(help)
	}
	if wantsHelp(args) {
		showHelp(help)
	}
	manifest, err := images.Load()
	if err != nil {
		die("%v", err)
	}
	rest, asJSON := extractJSONFlag(args[1:])
	switch args[0] {
	case "ls", "list":
		imageLs(manifest, rest, asJSON)
	case "show", "info":
		imageShow(manifest, rest, asJSON)
	case "recommend", "for":
		imageRecommend(manifest, rest, asJSON)
	default:
		dieUnknownSubcommand("image", args[0], []string{"ls", "show", "recommend"})
	}
}

// imageRef is what a consumer pulls: the repository, tagged with this binary's
// release when it is one. A dev build names no tag — the release lane publishes
// no floating `latest`, so there is nothing truthful to print.
type imageRef struct {
	images.Image
	Tag string `json:"tag,omitempty"`
}

var releaseVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func refOf(img images.Image) imageRef {
	ref := imageRef{Image: img}
	if releaseVersion.MatchString(version) {
		ref.Tag = version
	}
	return ref
}

func (r imageRef) pull() string {
	if r.Tag == "" {
		return r.Repository
	}
	return r.Repository + ":" + r.Tag
}

func imageFilterFlags(name string, args []string, withEngine bool) (images.Filter, []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	vendor := fs.String("vendor", "", "accelerator vendor: nvidia | amd | intel | tt | cpu")
	engine := new(string)
	valueFlags := map[string]bool{"--vendor": true, "-vendor": true}
	if withEngine {
		engine = fs.String("engine", "", "engine: "+strings.Join(engines.Names(), " | "))
		valueFlags["--engine"], valueFlags["-engine"] = true, true
	}
	_ = fs.Parse(reorderFlagsFirst(args, valueFlags))
	return images.Filter{Engine: engines.Canonical(strings.ToLower(*engine)), Vendor: *vendor}, fs.Args()
}

func imageLs(m images.Manifest, args []string, asJSON bool) {
	filter, _ := imageFilterFlags("image ls", args, true)
	rows := m.Select(filter)
	if asJSON {
		refs := make([]imageRef, len(rows))
		for i, img := range rows {
			refs[i] = refOf(img)
		}
		emitJSON(refs)
		return
	}
	if len(rows) == 0 {
		die("no image matches (vendors: %s; engines with an image: %s)", strings.Join(m.Vendors(), ", "), strings.Join(m.Engines(), ", "))
	}
	printImageTable(rows, nil)
	fmt.Println()
	fmt.Println(dim("Tip: `opod image show <image>` for node requirements and limits. `opod image recommend <model>` to pick one."))
}

// printImageTable prints one line per image; mark, when set, prefixes each row
// (the recommend view stars its pick).
func printImageTable(rows []images.Image, mark func(i int) string) {
	prefix := ""
	if mark != nil {
		prefix = "  "
	}
	fmt.Printf("%s%s %s %s %s %s %s\n", prefix,
		bold(fmt.Sprintf("%-30s", "IMAGE")),
		bold(fmt.Sprintf("%-9s", "ENGINE")),
		bold(fmt.Sprintf("%-7s", "VENDOR")),
		bold(fmt.Sprintf("%-12s", "ARCH")),
		bold(fmt.Sprintf("%-5s", "GANG")),
		bold("PROVEN"))
	for i, img := range rows {
		if mark != nil {
			prefix = mark(i) + " "
		}
		proven := dim("no")
		if img.Proven {
			proven = green("yes")
		}
		fmt.Printf("%s%s %-9s %-7s %-12s %-5s %s\n", prefix,
			padCyan(img.Name, 30), dash(img.Engine), dash(img.Vendor),
			strings.Join(img.Arch, ","), dash(img.Gang), proven)
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func imageShow(m images.Manifest, args []string, asJSON bool) {
	if len(args) != 1 {
		die("usage: opod image show <image> [--json]")
	}
	img, ok := m.Find(args[0])
	if !ok {
		names := make([]string, len(m.Images))
		for i, x := range m.Images {
			names[i] = x.Name
		}
		if g := suggest(args[0], names); g != "" {
			die("no image %q\n\nDid you mean %s?", args[0], cyan(g))
		}
		die("no image %q (run `opod image ls`)", args[0])
	}
	ref := refOf(img)
	if asJSON {
		emitJSON(ref)
		return
	}
	fmt.Printf("%s\n%s\n\n", bold(img.Name), img.Summary)
	field := func(k, v string) {
		if v != "" {
			fmt.Printf("  %-12s %s\n", k, v)
		}
	}
	field("pull", ref.pull())
	field("role", img.Role)
	field("engine", img.Engine)
	field("vendor", img.Vendor)
	field("hardware", img.Hardware)
	field("platforms", strings.Join(img.Arch, ", "))
	field("weights", strings.Join(img.Weights, ", "))
	if img.Role == images.RoleWorker {
		field("gang", map[string]string{
			"":             "one node only",
			images.GangRPC: "rpc — a part of a llama.cpp RPC split across nodes (label io.opod.llamacpp.rpc=true)",
			images.GangRay: "ray — vLLM multi-node",
		}[img.Gang])
	}
	field("proven", map[bool]string{true: "yes — has served on this hardware", false: "no — built and published, not yet run on this hardware"}[img.Proven])
	field("base", img.Base)
	field("dockerfile", img.Dockerfile)
	printList := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Printf("\n%s\n", bold(title))
		for _, it := range items {
			fmt.Printf("  - %s\n", it)
		}
	}
	printList("The node must provide", img.Requires)
	printList("Limits", img.Notes)
	fmt.Println()
	if ref.Tag == "" {
		fmt.Println(dim("This is a dev build, so no tag is named: releases are tagged X.Y.Z and there is no floating latest."))
	}
	fmt.Println(dim("Pin it by digest before deploying: crane digest " + ref.Repository + ":<tag>"))
}

func imageRecommend(m images.Manifest, args []string, asJSON bool) {
	filter, pos := imageFilterFlags("image recommend", args, false)
	if len(pos) != 1 {
		die("usage: opod image recommend <model> [--vendor V] [--json]")
	}
	if v := images.CanonicalVendor(filter.Vendor); v != "" && len(m.Select(images.Filter{Vendor: v})) == 0 {
		die("no image is built for vendor %q (vendors: %s)", filter.Vendor, strings.Join(m.Vendors(), ", "))
	}
	entry := resolveModelForImages(m, pos[0])
	rec := m.Recommend(*entry, filter.Vendor)

	if asJSON {
		type candidate struct {
			imageRef
			Recommended bool `json:"recommended"`
		}
		out := struct {
			images.Recommendation
			Candidates []candidate `json:"candidates"`
		}{Recommendation: rec, Candidates: make([]candidate, len(rec.Candidates))}
		for i, c := range rec.Candidates {
			out.Candidates[i] = candidate{imageRef: refOf(c.Image), Recommended: c.Recommended}
		}
		emitJSON(out)
		return
	}

	fmt.Printf("%s — weights: %s", bold(rec.Model), dash(rec.Weights))
	if rec.MinVRAMGB > 0 {
		fmt.Printf(", min VRAM %d GB", rec.MinVRAMGB)
	}
	if rec.Sharded {
		fmt.Printf(", split across workers by %s", rec.ShardBy)
	}
	fmt.Print("\n\n")
	if len(rec.Candidates) > 0 {
		rows := make([]images.Image, len(rec.Candidates))
		for i, c := range rec.Candidates {
			rows[i] = c.Image
		}
		printImageTable(rows, func(i int) string {
			if rec.Candidates[i].Recommended {
				return green("★")
			}
			return " "
		})
		fmt.Println()
		fmt.Println(dim("★ = the pick for that vendor: the model's first recommended engine with an image, proven hardware first."))
	} else {
		fmt.Println(yellow("No published image can serve this entry."))
	}
	if len(rec.Skipped) > 0 {
		fmt.Printf("\n%s\n", bold("Not offered"))
		for _, s := range rec.Skipped {
			what := s.Engine
			if s.Image != "" {
				what = s.Image
			}
			fmt.Printf("  - %s: %s\n", what, s.Reason)
		}
	}
	fmt.Println()
	fmt.Println(dim("Every endpoint also runs one opod-leader. `opod image show <image>` lists what the node must provide."))
}

// resolveModelForImages finds the model in the bundled catalog, or accepts the
// hf: / file: / ollama: scheme ids `opod model add` takes. It reads no config:
// the answer must not depend on the machine asking.
func resolveModelForImages(m images.Manifest, id string) *models.Entry {
	cat, err := models.BundledCatalog()
	if err != nil {
		die("catalog: %v", err)
	}
	if e := models.FindByID(cat, id); e != nil {
		return e
	}
	e, ok := models.ParseSchemeID(id)
	if !ok {
		die("no catalog entry for %q (try `opod model search`, or hf:<owner>/<repo>)", id)
	}
	// A scheme id has no curated engine list: offer every engine that has an
	// image, and let the weight format decide.
	e.RecommendedEngines = m.Engines()
	return e
}
