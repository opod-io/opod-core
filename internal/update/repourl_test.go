package update

// The Go module path is github.com/opod-io/opod; the GitHub repository is
// opod-io/opod-core (Repo). They differ on purpose, and the difference is easy
// to forget: a URL written from the module path looks right and is dead — the
// installer's release lookup, the Homebrew formula, the release publisher and
// every docs link were, all at once. This test walks the tree and fails on any
// github.com / raw.githubusercontent.com / api.github.com address that names
// the module path instead of the repository. Import paths are left alone:
// they carry no scheme and never continue with a GitHub page such as /issues.

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var deadRepoURL = []*regexp.Regexp{
	// a web address of the module path itself, or of anything under it
	regexp.MustCompile(`https?://(www\.)?github\.com/opod-io/opod($|[^-\w])`),
	// scheme-less, but continuing with a page only the website has
	regexp.MustCompile(`github\.com/opod-io/opod/(issues|blob|tree|releases|discussions|security|pulls?|wiki|actions|raw|archive|compare|commits?)($|\W)`),
	// the raw-content and API hosts never serve an import path
	regexp.MustCompile(`(raw\.githubusercontent\.com|codeload\.github\.com|api\.github\.com/repos)/opod-io/opod($|[^-\w])`),
}

// skippedTrees are not ours to keep current: history, vendored code, build
// output.
var skippedTrees = map[string]bool{".git": true, "docs/archive": true, "site/vendor": true, "dist": true, "node_modules": true}

func TestNoURLNamesTheModulePathAsTheRepository(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skippedTrees[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
			return nil // a binary or a bundle, not a place a link is written by hand
		}
		b, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(b, 0) >= 0 {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, re := range deadRepoURL {
				if re.MatchString(line) {
					t.Errorf("%s:%d names the module path where the repository (%s) is meant: %s", rel, i+1, Repo, strings.TrimSpace(line))
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Two files name the repository as a bare slug, where no URL pattern can see
// it: the installer resolves releases from it, and the release publisher
// uploads to it.
func TestInstallerAndPublisherNameTheRepository(t *testing.T) {
	root := filepath.Join("..", "..")
	owner, name, _ := strings.Cut(Repo, "/")
	for file, want := range map[string]string{
		"installer/install.sh": `REPO="` + Repo + `"`,
		".goreleaser.yaml":     "release:\n  github:\n    owner: " + owner + "\n    name: " + name + "\n",
	} {
		b, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s does not say %q", file, want)
		}
	}
}
