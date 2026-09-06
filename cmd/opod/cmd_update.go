package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const opodRepo = "opod-io/opod"

// cmdUpdate checks the latest Opod release on GitHub and, unless --check,
// downloads + verifies + installs it in place of the running binary.
//
// Examples:
//
//	opod update                 # check + install latest
//	opod update --check         # just check, don't install
//	opod update --version v0.2  # pin a specific version
//	opod update --force         # reinstall even if up to date
func cmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only check for an update, don't install")
	pinned := fs.String("version", "", "install a specific version (e.g. v0.1.1) instead of latest")
	force := fs.Bool("force", false, "install even if already on the latest version")
	help := helpSpec{
		name:    "update",
		summary: "check for and install the latest Opod release",
		usage:   "opod update [--check] [--version <vX.Y.Z>] [--force]",
		flags:   fs,
		examples: []string{
			"opod update                    # upgrade to latest",
			"opod update --check            # just check, no install",
			"opod update --version v0.1.1   # pin specific version",
			"opod upgrade                   # alias of `update`",
		},
		notes: []string{
			"After installing, restart with `opod down && opod up` if it was running.",
			"If the install path needs sudo (e.g. /usr/local/bin), you'll be told the exact command to run.",
		},
	}
	// Bad flags: print usage to stderr and let ExitOnError exit 2.
	fs.Usage = func() { showUsageErr(help) }
	if wantsHelp(args) {
		showHelp(help)
	}
	_ = fs.Parse(args)

	// 1. Find the current binary so we know where to install over.
	exe, err := os.Executable()
	if err != nil {
		die("could not locate current binary: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// else: keep the un-resolved path; better than the empty string we'd get
	// from swallowing the error.

	// 2. Resolve the target version.
	target := *pinned
	if target == "" {
		note(os.Stdout, "checking for updates…")
		latest, err := fetchLatestVersion()
		if err != nil {
			die("could not fetch latest version: %v", err)
		}
		target = latest
	}

	current := normalizeVersion("v" + version)
	wanted := normalizeVersion(target)
	upToDate := current == wanted

	// 3. Decide what to do based on --check / --force / version compare.
	switch {
	case upToDate && *check && *force:
		note(os.Stdout, "would force-reinstall %s (already on latest)", target)
		return
	case upToDate && *check:
		ok(os.Stdout, "already on the latest version (%s)", target)
		return
	case upToDate && !*force:
		ok(os.Stdout, "already on the latest version (%s)", target)
		return
	case *check:
		note(os.Stdout, "update available: %s → %s", current, target)
		note(os.Stdout, "run: opod update")
		return
	}

	// 4. Download + verify + install.
	note(os.Stdout, "updating: %s → %s", current, target)
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if err := downloadAndInstall(target, platform, exe); err != nil {
		die("update failed: %v", err)
	}

	ok(os.Stdout, "installed %s at %s", target, exe)
	fmt.Println()
	fmt.Println("  To use the new version, restart opod:")
	fmt.Println("    opod down")
	fmt.Println("    opod up")
}

// userAgent identifies us to GitHub. Anonymous requests without a UA hit a
// stricter 60/hour rate limit and GitHub's docs explicitly ask for one.
const userAgent = "opod-update/" + opodRepo

// fetchLatestVersion returns the latest release's tag_name (e.g. "v0.1.0").
func fetchLatestVersion() (string, error) {
	req, _ := http.NewRequest(http.MethodGet,
		"https://api.github.com/repos/"+opodRepo+"/releases/latest", nil)
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

// downloadAndInstall pulls the right release artifact, verifies its sha256
// against checksums.txt, extracts the binary, and renames it over `target`.
func downloadAndInstall(version, platform, target string) error {
	tmpdir, err := os.MkdirTemp("", "opod-update-*")
	if err != nil {
		return fmt.Errorf("tmpdir: %w", err)
	}
	defer os.RemoveAll(tmpdir)

	asset := fmt.Sprintf("opod-%s.tar.gz", platform)
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", opodRepo, version)

	note(os.Stdout, "downloading %s/%s", version, asset)
	tarPath := filepath.Join(tmpdir, asset)
	if err := download(base+"/"+asset, tarPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	// Verify SHA-256. checksums.txt missing entirely is best-effort (older
	// releases didn't ship it). But if it downloads and our artifact isn't
	// listed, fail closed — installing an unverified binary is worse than
	// failing loudly.
	sumPath := filepath.Join(tmpdir, "checksums.txt")
	if err := download(base+"/checksums.txt", sumPath); err == nil {
		expected, found := lookupChecksum(sumPath, asset)
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
		ok(os.Stdout, "checksum verified (sha256)")
	} else {
		warn(os.Stdout, "checksums.txt not available for %s — skipping verification", version)
	}

	// Extract.
	if err := extractTarGz(tarPath, tmpdir); err != nil {
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

	// Replace the running binary. Stage the new binary in the SAME directory
	// as the target, then rename within that directory: os.Rename across
	// filesystems fails with EXDEV, and tmpdir (os.MkdirTemp("")) is often on
	// a different mount than the install dir — renaming straight from tmpdir
	// would misreport EXDEV as "needs sudo". On Unix the rename works even
	// while the old binary is executing (the old inode stays open until exit).
	dir := filepath.Dir(target)
	staged := filepath.Join(dir, ".opod.new")
	if err := copyFile(newBin, staged); err != nil {
		// Can't even write into the install dir — genuinely needs sudo. Park
		// the new binary in a persistent temp file (newBin is under tmpdir,
		// which is removed when this function returns) and show the command.
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
		// Staged file is in the install dir and persists — point sudo at it.
		return fmt.Errorf(
			"could not replace %s (likely needs sudo)\n"+
				"  the new binary is staged at: %s\n"+
				"  to finish, run:\n"+
				"    sudo mv %s %s",
			target, staged, staged, target)
	}
	return nil
}

// stagePersistent copies src to a uniquely-named file in the system temp
// directory (NOT under the auto-removed update tmpdir) and returns its path,
// so a "finish with sudo" hint points at a binary that still exists after the
// command returns.
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

func lookupChecksum(sumFile, artifact string) (string, bool) {
	data, err := os.ReadFile(sumFile)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// `sha256sum` writes "<hash>  <name>" but may prefix the name with a
		// "*" (binary mode) or "./"; normalize before comparing so a present
		// checksum isn't treated as "not listed" (which fails the install).
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

func extractTarGz(src, dstDir string) error {
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
		// Reject absolute paths and any entry that would escape dstDir.
		// A substring "/.." check is too weak (misses absolute paths), so
		// verify the joined path stays under dstDir via filepath.Rel.
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
			// fall through to extract
		default:
			// Skip symlinks, hardlinks, devices, FIFOs, etc. A release
			// tarball is only regular files + dirs; without this a symlink
			// entry would be written as a regular file containing its target
			// path (and a crafted archive could point it outside dstDir).
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

// MaybeShowUpdateNotice prints a one-line "newer version available" notice
// if a fresher Opod release exists. Designed to run during `opod up`:
//
//   - 24h cache at ~/.opod/update-check.json keeps GitHub API hits down to
//     once per day per machine.
//   - On cache miss, the network probe has a hard 1-second budget; if GitHub
//     is slow we skip the notice for this run rather than block startup.
//   - OPOD_NO_UPDATE_CHECK=1 disables the check entirely (offline / privacy).
//
// Safe to call once per process — does not block longer than 1 second.
func MaybeShowUpdateNotice(w io.Writer) {
	if os.Getenv("OPOD_NO_UPDATE_CHECK") == "1" {
		return
	}

	cachePath := updateCheckCachePath()
	if cached, ok := readCachedLatest(cachePath, 24*time.Hour); ok {
		// Self-heal: only when the cached "latest" is strictly OLDER than
		// what we're already running (e.g. cache was written before
		// `opod update` jumped us several patches) is it useless —
		// discard it and refetch. Otherwise we'd cheerfully advertise a
		// downgrade for the next 24h. An up-to-date cache (==) is valid:
		// honor the TTL and stay off the network.
		if cmpVersion(normalizeVersion(cached), normalizeVersion("v"+version)) >= 0 {
			printUpdateNoticeIfNewer(w, cached)
			return
		}
	}

	// Cache stale or missing — bounded async fetch.
	ch := make(chan string, 1)
	go func() {
		latest, err := fetchLatestVersion()
		if err != nil {
			close(ch)
			return
		}
		writeCachedLatest(cachePath, latest)
		ch <- latest
	}()

	select {
	case latest, ok := <-ch:
		if ok {
			printUpdateNoticeIfNewer(w, latest)
		}
	case <-time.After(1 * time.Second):
		// budget exceeded; skip notice this run, refresh next run
	}
}

// updateCheckCache is the JSON shape we persist between runs.
type updateCheckCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

func updateCheckCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".opod", "update-check.json")
}

func readCachedLatest(path string, ttl time.Duration) (string, bool) {
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var c updateCheckCache
	if err := json.Unmarshal(data, &c); err != nil {
		return "", false
	}
	if time.Since(c.CheckedAt) > ttl {
		return "", false
	}
	return c.Latest, true
}

func writeCachedLatest(path, latest string) {
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal(updateCheckCache{CheckedAt: time.Now(), Latest: latest})
	_ = os.WriteFile(path, data, 0o644)
}

func printUpdateNoticeIfNewer(w io.Writer, latest string) {
	// Only nudge when `latest` is strictly newer than the running binary.
	// Equality and "behind" both mean no upgrade exists for the user.
	if cmpVersion(normalizeVersion(latest), normalizeVersion("v"+version)) <= 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  ✨  Opod %s is available (you have v%s).\n", latest, version)
	fmt.Fprintln(w, "      Run `opod update` to upgrade.")
	fmt.Fprintln(w)
}

// cmpVersion compares two normalized "vX.Y.Z" strings numerically and
// returns -1 / 0 / +1. Non-numeric or malformed segments are treated
// as 0, which is fine for our normalize-then-compare path.
func cmpVersion(a, b string) int {
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

// normalizeVersion strips leading "v" and trailing "-dev" / "-snapshot" so
// "v0.1.0", "0.1.0", and "0.1.0-dev" can be loosely compared.
func normalizeVersion(s string) string {
	s = strings.TrimPrefix(s, "v")
	if idx := strings.IndexAny(s, "-+"); idx >= 0 {
		s = s[:idx]
	}
	return "v" + s
}
