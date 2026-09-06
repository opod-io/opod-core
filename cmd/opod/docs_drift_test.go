package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/models"
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
	repoRoot := findRepoRoot(t)
	entries, err := models.LoadCatalog(filepath.Join(repoRoot, "catalog"))
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
	repoRoot := findRepoRoot(t)
	entries, err := models.LoadCatalog(filepath.Join(repoRoot, "catalog"))
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

// TestCatalogParses verifies every YAML file in catalog/ loads cleanly through
// the same parser the binary uses at startup. Catches malformed entries
// before they reach `opod up`.
func TestCatalogParses(t *testing.T) {
	repoRoot := findRepoRoot(t)
	catalogDir := filepath.Join(repoRoot, "catalog")

	entries, err := models.LoadCatalog(catalogDir)
	if err != nil {
		t.Fatalf("LoadCatalog(%s): %v", catalogDir, err)
	}
	if len(entries) == 0 {
		t.Fatal("catalog parsed to zero entries — wrong directory?")
	}

	// Every entry must have an id, and the filename must match the id.
	files, err := os.ReadDir(catalogDir)
	if err != nil {
		t.Fatalf("read catalog dir: %v", err)
	}
	fileIDs := map[string]bool{}
	for _, f := range files {
		name := f.Name()
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
