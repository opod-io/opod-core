package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/models"
	"github.com/opod-io/opod-core/pkg/catalog"
)

// TestDocsReferenceRealCommands verifies that every `opod <verb>` mentioned
// in README.md and QUICKSTART.md corresponds to a real cobra subcommand
// dispatched from cmd/opod/main.go. Catches "ghost commands" — doc claims
// that don't exist in the binary.
func TestDocsReferenceRealCommands(t *testing.T) {
	repoRoot := findRepoRoot(t)
	verbs := canonicalVerbs(t, filepath.Join(repoRoot, "cmd", "opod", "main.go"))

	docs := []string{
		filepath.Join(repoRoot, "README.md"),
		filepath.Join(repoRoot, "QUICKSTART.md"),
	}

	// Tokens that follow `opod ` in docs but are not commands (placeholders,
	// flag values, or self-references). Add new ones here when intentional.
	placeholders := map[string]bool{
		"<client>": true, "<name>": true, "<command>": true, "<tool>": true,
		"<url>": true, "<id>": true, "<model>": true, "<query>": true,
		"--help": true, "-h": true, "--version": true, "-v": true,
	}

	// Match `opod <verb>` on the same line, where verb starts lowercase
	// (real commands always do). Same-line constraint avoids matching
	// across code-block line breaks like `git clone …/opod\ncd opod`.
	verbRe := regexp.MustCompile(`\bopod[ \t]+([a-z][\w-]*)`)
	// Verb tokens that look like commands but are actually version
	// fragments captured before the "." breaks the regex (e.g. the "v0" in
	// "opod v0.4.0").
	versionish := regexp.MustCompile(`^v\d+$`)

	var ghosts []string
	for _, path := range docs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range verbRe.FindAllStringSubmatch(string(data), -1) {
			tok := m[1]
			if placeholders[tok] || verbs[tok] || versionish.MatchString(tok) {
				continue
			}
			ghosts = append(ghosts, filepath.Base(path)+": opod "+tok)
		}
	}

	if len(ghosts) > 0 {
		sort.Strings(ghosts)
		uniq := dedupe(ghosts)
		t.Errorf("docs reference commands not dispatched in cmd/opod/main.go:\n  %s\n\nFix: either add the command (or alias) or remove the doc reference.",
			strings.Join(uniq, "\n  "))
	}
}

// TestEveryCommandDocumented is the reverse of TestDocsReferenceRealCommands:
// it asserts that every top-level command dispatched in main.go is advertised
// in BOTH the `opod help` screen (printUsage) and the README CLI reference,
// so a real command can never become hidden/undocumented for developers.
func TestEveryCommandDocumented(t *testing.T) {
	repoRoot := findRepoRoot(t)
	mainPath := filepath.Join(repoRoot, "cmd", "opod", "main.go")
	verbs := canonicalVerbs(t, mainPath)

	// Flag-style aliases handled by the dispatch switch but not listed as
	// commands, plus the `help` meta-command itself (documented implicitly).
	skip := map[string]bool{
		"--version": true, "-v": true, "--help": true, "-h": true, "help": true,
	}

	// Extract the printUsage help text (the first raw-string literal in the
	// function) so we check what `opod help` actually prints.
	mainSrc, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainPath, err)
	}
	help := extractRawString(t, string(mainSrc), "func printUsage")

	readme, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readmeStr := string(readme)

	var missingHelp, missingReadme []string
	for v := range verbs {
		if skip[v] {
			continue
		}
		// In help: a command line indented under "Commands:" begins with the verb.
		inHelp := regexp.MustCompile(`(?m)^\s{2,}` + regexp.QuoteMeta(v) + `\b`).MatchString(help)
		if !inHelp {
			missingHelp = append(missingHelp, v)
		}
		// In README: referenced as `opod <verb>` somewhere in the doc.
		if !regexp.MustCompile(`\bopod[ \t]+` + regexp.QuoteMeta(v) + `\b`).MatchString(readmeStr) {
			missingReadme = append(missingReadme, v)
		}
	}
	if len(missingHelp) > 0 {
		sort.Strings(missingHelp)
		t.Errorf("commands dispatched in main.go but absent from `opod help` (printUsage):\n  %s\n\nFix: add them to printUsage so the command isn't hidden.", strings.Join(missingHelp, ", "))
	}
	if len(missingReadme) > 0 {
		sort.Strings(missingReadme)
		t.Errorf("commands dispatched in main.go but not referenced in README.md:\n  %s\n\nFix: document them in the CLI reference.", strings.Join(missingReadme, ", "))
	}
}

// extractRawString returns the contents of the first back-quoted raw string
// literal that appears after the marker substring in src.
func extractRawString(t *testing.T, src, marker string) string {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	rest := src[i:]
	open := strings.IndexByte(rest, '`')
	if open < 0 {
		t.Fatalf("no raw string after %q", marker)
	}
	rest = rest[open+1:]
	end := strings.IndexByte(rest, '`')
	if end < 0 {
		t.Fatalf("unterminated raw string after %q", marker)
	}
	return rest[:end]
}

// TestCatalogLicensePresent asserts every catalog entry declares a
// license. Set by 5d4f… when the license field was added. Backstop for
// a contributor adding a new model without a license — `opod model info`
// would otherwise render "License: —" which is worse than a build break.
func TestCatalogLicensePresent(t *testing.T) {
	entries, err := models.BundledCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	for _, e := range entries {
		if e.License == "" {
			t.Errorf("%s: missing `license:` field — add the SPDX identifier (apache-2.0, mit, llama-3-community, gemma, etc.)", e.ID)
		}
	}
}

// TestCatalogRestrictiveLicenseTagged asserts that any catalog entry with a
// non-permissive license carries a `restricted-license` tag, so a user
// running `opod model search restricted-license` finds them all and
// `opod model info` warns clearly.
//
// Permissive set: apache-2.0, mit, bsd-*, lfm-open. Everything else is
// considered restrictive for tagging purposes (we don't try to encode the
// full open-source-vs-non-OSS distinction here — operators read the URL).
func TestCatalogRestrictiveLicenseTagged(t *testing.T) {
	entries, err := models.BundledCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	permissive := map[string]bool{
		"apache-2.0": true, "mit": true, "bsd-2-clause": true, "bsd-3-clause": true,
		"lfm-open": true,
	}
	for _, e := range entries {
		if e.License == "" {
			continue // covered by TestCatalogLicensePresent
		}
		if permissive[strings.ToLower(e.License)] {
			continue
		}
		hasTag := false
		for _, tag := range e.Tags {
			if tag == "restricted-license" {
				hasTag = true
				break
			}
		}
		if !hasTag {
			t.Errorf("%s: license %q is not permissive; add `restricted-license` to tags so users filter on it. Also confirm license_url points at the canonical terms.",
				e.ID, e.License)
		}
	}
}

// TestCatalogParses verifies every embedded catalog file (pkg/catalog) loads cleanly through
// the same parser the binary uses at startup. Catches malformed entries
// before they reach `opod up`.
func TestCatalogParses(t *testing.T) {
	entries, err := models.BundledCatalog()
	if err != nil {
		t.Fatalf("BundledCatalog: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("catalog parsed to zero entries — wrong directory?")
	}

	// Every entry must have an id, and the filename must match the id.
	fileIDs := map[string]bool{}
	for _, name := range catalog.Names() {
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
		fileIDs[id] = true
	}
	for _, e := range entries {
		if e.ID == "" {
			t.Errorf("entry with empty id")
			continue
		}
		if !fileIDs[e.ID] {
			t.Errorf("entry id %q has no matching <id>.yaml in catalog/", e.ID)
		}
	}
}

// TestVersionStampSane verifies the binary's version variable looks like a
// real semver-ish string. Cheap guard against accidentally landing
// "0.0.0" or an empty stamp.
func TestVersionStampSane(t *testing.T) {
	if version == "" {
		t.Fatal("version is empty")
	}
	semverish := regexp.MustCompile(`^\d+\.\d+\.\d+(-[A-Za-z0-9.-]+)?$`)
	if !semverish.MatchString(version) {
		t.Errorf("version %q does not look like semver (expected MAJOR.MINOR.PATCH[-suffix])", version)
	}
}

// canonicalVerbs returns the set of verbs dispatched from main.go's switch
// statement. Reads the file rather than parsing the AST — the switch is
// stable and simple.
func canonicalVerbs(t *testing.T, mainPath string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainPath, err)
	}
	// Each `case "foo":` or `case "foo", "bar":` line registers one or more verbs.
	caseRe := regexp.MustCompile(`(?m)^\s*case\s+("[\w-]+"(?:\s*,\s*"[\w-]+")*)\s*:`)
	strRe := regexp.MustCompile(`"([\w-]+)"`)
	verbs := map[string]bool{}
	for _, m := range caseRe.FindAllStringSubmatch(string(data), -1) {
		for _, s := range strRe.FindAllStringSubmatch(m[1], -1) {
			verbs[s[1]] = true
		}
	}
	if len(verbs) == 0 {
		t.Fatalf("no `case \"verb\":` lines found in %s", mainPath)
	}
	return verbs
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root (no go.mod above %s)", wd)
		}
		dir = parent
	}
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	out := s[:0]
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// ---- the README's facts (T10.23) --------------------------------------------
//
// The tests above check that the commands a doc names exist. These check the
// numbers and lists a doc states about the binary — the catalog's size, the
// engines it drives and the process-environment contract — against the code
// that defines them, because on 2026-10-31 all three had drifted at once.

// readDoc reads one doc file from the repo root.
func readDoc(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(findRepoRoot(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// catalogCountPhrases are the ways the docs state the catalog's size. Every
// match must equal the embedded catalog; each doc must state it at least once,
// so a rewording that escapes every pattern fails here instead of going
// unchecked.
var catalogCountPhrases = []*regexp.Regexp{
	regexp.MustCompile(`\b(\d+) open-weight models\b`),
	regexp.MustCompile(`\b(\d+) catalog entries\b`),
	regexp.MustCompile(`\bthe (\d+) entries embedded\b`),
	regexp.MustCompile(`\bcurated (\d+) entries\b`),
	regexp.MustCompile(`Catalog has \*\*(\d+) models\*\*`),
}

func TestDocsStateTheCatalogSize(t *testing.T) {
	entries, err := models.BundledCatalog()
	if err != nil {
		t.Fatalf("BundledCatalog: %v", err)
	}
	want := strconv.Itoa(len(entries))
	for _, doc := range []string{"README.md", "MODELS.md", "QUICKSTART.md"} {
		text, found := readDoc(t, doc), 0
		for _, re := range catalogCountPhrases {
			for _, m := range re.FindAllStringSubmatch(text, -1) {
				found++
				if m[1] != want {
					t.Errorf("%s says %q; the embedded catalog has %s entries", doc, m[0], want)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s states the catalog size in none of the checked phrasings; add one or extend catalogCountPhrases", doc)
		}
	}
}

// engineDocNames maps the name a doc gives an engine to its registry name.
var engineDocNames = map[string]string{
	"llama.cpp": "llamacpp", "vLLM": "vllm", "SGLang": "sglang",
	"Ollama": "ollama", "MLX-LM": "mlx", "TensorRT-LLM": "trtllm",
}

var numberWords = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}

func TestReadmeListsTheRegisteredEngines(t *testing.T) {
	registered := engines.Names()
	readme := readDoc(t, "README.md")

	// 1. The Prerequisites list: one bullet per engine, `- **Name** …`.
	sec := readme[strings.Index(readme, "### Prerequisites"):]
	if end := strings.Index(sec, "Install steps, run commands"); end > 0 {
		sec = sec[:end]
	}
	var listed []string
	for _, m := range regexp.MustCompile(`(?m)^- \*\*([^*]+)\*\*`).FindAllStringSubmatch(sec, -1) {
		name, ok := engineDocNames[m[1]]
		if !ok {
			t.Errorf("README Prerequisites lists %q, which is no registered engine (registered: %v)", m[1], registered)
			continue
		}
		listed = append(listed, name)
	}
	sort.Strings(listed)
	if strings.Join(listed, ",") != strings.Join(registered, ",") {
		t.Errorf("README Prerequisites lists engines %v; the registry has %v", listed, registered)
	}

	// 2. The driver list in the tracing bullet: "(ollama, vllm, …)".
	m := regexp.MustCompile(`engine drivers \(([^)]*)\)`).FindStringSubmatch(readme)
	if m == nil {
		t.Fatal("README no longer names the engine drivers as `engine drivers (a, b, …)`; update this test with it")
	}
	drivers := strings.Split(strings.ReplaceAll(m[1], " ", ""), ",")
	sort.Strings(drivers)
	if strings.Join(drivers, ",") != strings.Join(registered, ",") {
		t.Errorf("README's engine drivers are %v; the registry has %v", drivers, registered)
	}

	// 3. Every "N engines" / "the N it drives" says the registry's count.
	if len(registered) >= len(numberWords) {
		t.Fatalf("%d engines: extend numberWords", len(registered))
	}
	want := numberWords[len(registered)]
	words := strings.Join(numberWords[2:], "|")
	countRe := regexp.MustCompile(`\b(` + words + `) (?:engines\b|engine drivers\b|it drives\b)`)
	hits := countRe.FindAllStringSubmatch(readme, -1)
	if len(hits) == 0 {
		t.Error("README states the engine count in none of the checked phrasings (`six engines`, `the six it drives`)")
	}
	for _, h := range hits {
		if h[1] != want {
			t.Errorf("README says %q; the registry has %d engines (%s)", h[0], len(registered), want)
		}
	}
}

// TestReadmeEnvTableIsTheContract: the README's process-environment table
// lists exactly config.Vars(), each with its side.
func TestReadmeEnvTableIsTheContract(t *testing.T) {
	readme := readDoc(t, "README.md")
	start := strings.Index(readme, "**The process-environment contract**")
	if start < 0 {
		t.Fatal("README has no **The process-environment contract** table")
	}
	sec := readme[start:]
	if tbl := strings.Index(sec, "| Var |"); tbl > 0 {
		sec = sec[tbl:]
	}
	if end := strings.Index(sec, "\n\n"); end > 0 {
		sec = sec[:end] // the table ends at the first blank line
	}
	nameRe := regexp.MustCompile("`([A-Z][A-Z0-9_]+)`")
	documented := map[string]string{}
	for _, line := range strings.Split(sec, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 || strings.TrimSpace(cells[1]) == "Var" || strings.HasPrefix(strings.TrimSpace(cells[1]), "---") {
			continue
		}
		side := strings.TrimSpace(cells[2])
		for _, m := range nameRe.FindAllStringSubmatch(cells[1], -1) {
			documented[m[1]] = side
		}
	}
	if len(documented) == 0 {
		t.Fatal("parsed no rows from the README's process-environment table")
	}
	for _, v := range config.Vars() {
		side, ok := documented[v.Name]
		switch {
		case !ok:
			t.Errorf("README's process-environment table is missing %s (side %s, internal/config/env.go)", v.Name, v.Side)
		case side != v.Side:
			t.Errorf("README says %s is read by %q; internal/config/env.go says %q", v.Name, side, v.Side)
		}
		delete(documented, v.Name)
	}
	for name := range documented {
		t.Errorf("README's process-environment table lists %s, which is not in internal/config/env.go", name)
	}
}
