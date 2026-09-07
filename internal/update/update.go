// Package update fetches, verifies and installs a released opod binary over
// the running one. Only the explicit `opod update` command calls it (ADR-022:
// no automatic check, no phone-home); the CLI parses flags and prints, this
// package does the work.
package update

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub repository releases are fetched from.
const Repo = "opod-io/opod"

// userAgent identifies us to GitHub: anonymous requests without one hit a
// stricter rate limit and GitHub's docs ask for it.
const userAgent = "opod-update/" + Repo

// LatestVersion returns the latest release's tag (e.g. "v0.1.0").
func LatestVersion() (string, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+Repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s: %s", resp.Status, string(b))
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", fmt.Errorf("no tag_name in API response")
	}
	return body.TagName, nil
}

// Install downloads the release artifact for version+platform, verifies its
// sha256 against checksums.txt when the release ships one (missing file =
// warn and proceed; present but unlisted artifact = refuse), extracts the
// binary and renames it over target. report receives progress lines the CLI
// prints. Errors that need sudo say exactly which command finishes the job.
func Install(version, platform, target string, report func(format string, a ...any)) error {
	if report == nil {
		report = func(string, ...any) {}
	}
	tmpdir, err := os.MkdirTemp("", "opod-update-*")
	if err != nil {
		return fmt.Errorf("tmpdir: %w", err)
	}
	defer os.RemoveAll(tmpdir)

	asset := fmt.Sprintf("opod-%s.tar.gz", platform)
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", Repo, version)

	report("downloading %s/%s", version, asset)
	tarPath := filepath.Join(tmpdir, asset)
	if err := download(base+"/"+asset, tarPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	sumPath := filepath.Join(tmpdir, "checksums.txt")
	if err := download(base+"/checksums.txt", sumPath); err == nil {
		expected, found := LookupChecksum(sumPath, asset)
		if !found {
			return fmt.Errorf("checksums.txt for %s does not list %s — refusing to install unverified binary", version, asset)
		}
		actual, err := sha256File(tarPath)
		if err != nil {
			return fmt.Errorf("sha256: %w", err)
		}
		if expected != actual {
			return fmt.Errorf("checksum MISMATCH (expected %s, got %s) — aborting", expected, actual)
		}
		report("checksum verified (sha256)")
	} else {
		report("checksums.txt not available for %s — skipping verification", version)
	}

	if err := ExtractTarGz(tarPath, tmpdir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	newBin := filepath.Join(tmpdir, "opod")
	st, err := os.Stat(newBin)
	if err != nil {
		return fmt.Errorf("no opod binary in archive: %w", err)
	}
	if st.IsDir() {
		return fmt.Errorf("opod entry in archive is a directory, not a file")
	}
	if err := os.Chmod(newBin, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Stage in the target's own directory, then rename within it: a rename
	// from tmpdir would fail with EXDEV across mounts and misreport as "needs
	// sudo". On Unix the rename works while the old binary is executing.
	dir := filepath.Dir(target)
	staged := filepath.Join(dir, ".opod.new")
	if err := copyFile(newBin, staged); err != nil {
		if persist, perr := stagePersistent(newBin); perr == nil {
			return fmt.Errorf(
				"could not write to %s (likely needs sudo)\n"+
					"  the new binary is staged at: %s\n"+
					"  to finish, run:\n"+
					"    sudo install -m 0755 %s %s",
				dir, persist, persist, target)
		}
		return fmt.Errorf("install: %w (you may need sudo)", err)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("chmod staged binary: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf(
			"could not replace %s (likely needs sudo)\n"+
				"  the new binary is staged at: %s\n"+
				"  to finish, run:\n"+
				"    sudo mv %s %s",
			target, staged, staged, target)
	}
	return nil
}

// stagePersistent copies src to a uniquely named file in the system temp
// directory (outside the auto-removed update tmpdir) so a "finish with sudo"
// hint points at a binary that still exists after the command returns.
func stagePersistent(src string) (string, error) {
	f, err := os.CreateTemp("", "opod-staged-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	_ = f.Close()
	if err := copyFile(src, name); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	_ = os.Chmod(name, 0o755)
	return name, nil
}

func download(url, dst string) error {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", userAgent)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s", resp.Status, url)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// LookupChecksum finds artifact's sha256 in a `sha256sum`-style file; names
// may carry a "*" (binary mode) or "./" prefix, which are normalised away.
func LookupChecksum(sumFile, artifact string) (string, bool) {
	data, err := os.ReadFile(sumFile)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		name = strings.TrimPrefix(name, "./")
		if name == artifact {
			return fields[0], true
		}
	}
	return "", false
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ExtractTarGz extracts regular files and directories from src into dstDir.
// Absolute paths and entries that would escape dstDir are skipped; symlinks,
// hardlinks and devices are skipped too (a release tarball is files + dirs).
func ExtractTarGz(src, dstDir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) {
			continue
		}
		outPath := filepath.Join(dstDir, clean)
		if rel, err := filepath.Rel(dstDir, outPath); err != nil ||
			rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(outPath, 0o755)
			continue
		case tar.TypeReg:
		default:
			continue
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return err
		}
		_ = out.Close()
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// CompareVersions compares two normalised "vX.Y.Z" strings numerically and
// returns -1 / 0 / +1; malformed segments count as 0.
func CompareVersions(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

// NormalizeVersion strips a leading "v" and any "-dev" / "+meta" suffix so
// "v0.1.0", "0.1.0" and "0.1.0-dev" compare loosely equal.
func NormalizeVersion(s string) string {
	s = strings.TrimPrefix(s, "v")
	if idx := strings.IndexAny(s, "-+"); idx >= 0 {
		s = s[:idx]
	}
	return "v" + s
}
