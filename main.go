// pi-bun-update installs the official, checksum-verified Pi Bun binary.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultRepo = "earendil-works/pi"

var (
	httpClient    = &http.Client{Timeout: 5 * time.Minute}
	githubAPIBase = "https://api.github.com"
)

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type options struct {
	repo, version, root, binDir, goos, goarch string
	dryRun, check, json                       bool
	keep                                      int
}

type statusReport struct {
	ActiveVersion   string   `json:"active_version,omitempty"`
	ActivePath      string   `json:"active_path,omitempty"`
	Installed       []string `json:"installed_versions"`
	LatestVersion   string   `json:"latest_version"`
	UpToDate        bool     `json:"up_to_date"`
	UpdateAvailable bool     `json:"update_available"`
}

type operationReport struct {
	Action  string   `json:"action"`
	Version string   `json:"version,omitempty"`
	Removed []string `json:"removed,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
}

type cliExitError struct{ code int }

func (e *cliExitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func exitCode(err error) int {
	var exitErr *cliExitError
	if errors.As(err, &exitErr) {
		return exitErr.code
	}
	if err != nil {
		return 1
	}
	return 0
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if exitCode(err) == 1 {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(exitCode(err))
	}
}

func run(args []string, out, errOut io.Writer) error {
	command := "update"
	if len(args) > 0 {
		switch args[0] {
		case "update", "status", "use", "prune":
			command, args = args[0], args[1:]
		}
	}
	fs := flag.NewFlagSet("pi-bun-update", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o options
	fs.StringVar(&o.repo, "repo", defaultRepo, "GitHub owner/repository")
	fs.StringVar(&o.version, "version", "", "release tag (default: latest)")
	fs.StringVar(&o.root, "root", defaultRoot(), "directory holding immutable versions")
	fs.StringVar(&o.binDir, "bin-dir", defaultBinDir(), "directory for the pi-bun symlink")
	fs.StringVar(&o.goos, "os", runtime.GOOS, "target OS: darwin or linux")
	fs.StringVar(&o.goarch, "arch", runtime.GOARCH, "target architecture: arm64 or amd64")
	fs.BoolVar(&o.dryRun, "dry-run", false, "show mutations without changing files")
	fs.BoolVar(&o.check, "check", false, "show release status only (update command alias)")
	fs.BoolVar(&o.json, "json", false, "emit a machine-readable JSON report")
	fs.IntVar(&o.keep, "keep", 3, "versions to retain during prune (active version is always retained)")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: pi-bun-update [update|status|use|prune] [flags] [version]")
		fmt.Fprintln(errOut, "Install, inspect, activate, and prune the official Pi Bun binary alongside Node/Pnpm Pi.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := validateRepo(o.repo); err != nil {
		return err
	}
	switch command {
	case "status":
		if fs.NArg() != 0 {
			return fmt.Errorf("status accepts no positional arguments")
		}
		return showStatus(context.Background(), o, out)
	case "use":
		if fs.NArg() != 1 {
			return fmt.Errorf("use requires exactly one installed version")
		}
		return withLock(o.root, func() error { return useVersion(o, fs.Arg(0), out) })
	case "prune":
		if fs.NArg() != 0 {
			return fmt.Errorf("prune accepts no positional arguments")
		}
		if o.keep < 0 {
			return fmt.Errorf("keep must be non-negative")
		}
		return withLock(o.root, func() error { return pruneVersions(o, out) })
	case "update":
		if fs.NArg() != 0 {
			return fmt.Errorf("update accepts no positional arguments; use --version")
		}
		if o.check {
			return showStatus(context.Background(), o, out)
		}
		return withLock(o.root, func() error { return update(context.Background(), o, out) })
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func defaultRoot() string {
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return filepath.Join(dataHome, "pi-bun")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".pi-bun"
	}
	return filepath.Join(home, ".local", "share", "pi-bun")
}

func defaultBinDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "bin")
	}
	return "."
}

func update(ctx context.Context, o options, out io.Writer) error {
	r, binary, checksums, err := resolveRelease(ctx, o)
	if err != nil {
		return err
	}
	if o.dryRun {
		return writeReport(out, o.json, operationReport{Action: "update", Version: r.TagName, DryRun: true}, fmt.Sprintf("Would install Pi %s (%s)", r.TagName, binary.Name))
	}
	return install(ctx, o, r.TagName, binary, checksums, out)
}

func showStatus(ctx context.Context, o options, out io.Writer) error {
	r, _, _, err := resolveRelease(ctx, o)
	if err != nil {
		return err
	}
	activeVersion, activePath := activeInstallation(o.root, o.binDir)
	installed, err := installedVersions(o.root)
	if err != nil {
		return err
	}
	report := statusReport{
		ActiveVersion: activeVersion, ActivePath: activePath, Installed: installed,
		LatestVersion: r.TagName, UpToDate: activeVersion == r.TagName,
		UpdateAvailable: activeVersion != r.TagName,
	}
	text := fmt.Sprintf("active: %s\nlatest: %s\nstatus: ", displayVersion(activeVersion), r.TagName)
	if report.UpToDate {
		text += "up to date"
	} else {
		text += "update available"
	}
	if err := writeReport(out, o.json, report, text); err != nil {
		return err
	}
	if report.UpdateAvailable {
		return &cliExitError{code: 2}
	}
	return nil
}

func useVersion(o options, version string, out io.Writer) error {
	if !safeVersion(version) {
		return fmt.Errorf("unsafe version %q", version)
	}
	path := filepath.Join(o.root, "versions", version, "pi", "pi")
	if !isExecutable(path) {
		return fmt.Errorf("Pi %s is not installed or is incomplete", version)
	}
	if o.dryRun {
		return writeReport(out, o.json, operationReport{Action: "use", Version: version, DryRun: true}, "Would activate Pi "+version)
	}
	if err := activate(o.binDir, "pi-bun", path); err != nil {
		return err
	}
	return writeReport(out, o.json, operationReport{Action: "use", Version: version}, "Activated Pi "+version)
}

func pruneVersions(o options, out io.Writer) error {
	versions, err := installedVersions(o.root)
	if err != nil {
		return err
	}
	active, _ := activeInstallation(o.root, o.binDir)
	keep := map[string]bool{}
	if active != "" {
		keep[active] = true
	}
	for i, version := range versions {
		if i >= o.keep {
			break
		}
		keep[version] = true
	}
	var removed []string
	for _, version := range versions {
		if keep[version] {
			continue
		}
		removed = append(removed, version)
		if !o.dryRun {
			if err := os.RemoveAll(filepath.Join(o.root, "versions", version)); err != nil {
				return err
			}
		}
	}
	report := operationReport{Action: "prune", Removed: removed, DryRun: o.dryRun}
	text := "No inactive versions to prune"
	if len(removed) > 0 {
		verb := "Pruned"
		if o.dryRun {
			verb = "Would prune"
		}
		text = verb + ": " + strings.Join(removed, ", ")
	}
	return writeReport(out, o.json, report, text)
}

func resolveRelease(ctx context.Context, o options) (release, asset, asset, error) {
	if err := validateRepo(o.repo); err != nil {
		return release{}, asset{}, asset{}, err
	}
	name, err := assetName(o.goos, o.goarch)
	if err != nil {
		return release{}, asset{}, asset{}, err
	}
	r, err := fetchRelease(ctx, o.repo, o.version)
	if err != nil {
		return release{}, asset{}, asset{}, err
	}
	if !safeVersion(r.TagName) {
		return release{}, asset{}, asset{}, fmt.Errorf("unsafe release tag %q", r.TagName)
	}
	binary, ok := findAsset(r.Assets, name)
	if !ok {
		return release{}, asset{}, asset{}, fmt.Errorf("release %s has no asset %q", r.TagName, name)
	}
	checksums, ok := findAsset(r.Assets, "SHA256SUMS")
	if !ok {
		return release{}, asset{}, asset{}, fmt.Errorf("release %s has no SHA256SUMS asset", r.TagName)
	}
	return r, binary, checksums, nil
}

func writeReport(out io.Writer, asJSON bool, report any, text string) error {
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	_, err := fmt.Fprintln(out, text)
	return err
}

func displayVersion(version string) string {
	if version == "" {
		return "none"
	}
	return version
}

func assetName(goos, goarch string) (string, error) {
	if goos != "darwin" && goos != "linux" {
		return "", fmt.Errorf("unsupported OS %q (Pi publishes Bun binaries for darwin and linux)", goos)
	}
	arch := map[string]string{"arm64": "arm64", "amd64": "x64"}[goarch]
	if arch == "" {
		return "", fmt.Errorf("unsupported architecture %q (Pi publishes arm64 and amd64/x64)", goarch)
	}
	return fmt.Sprintf("pi-%s-%s.tar.gz", goos, arch), nil
}

func validateRepo(repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, "\\?&#") {
		return fmt.Errorf("repo must be an owner/repository pair, got %q", repo)
	}
	return nil
}

func safeVersion(v string) bool {
	return v != "" && !strings.Contains(v, "..") && !strings.ContainsAny(v, "/\\") && strings.Trim(v, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-") == ""
}

func fetchRelease(ctx context.Context, repo, version string) (release, error) {
	endpoint := githubAPIBase + "/repos/" + repo + "/releases/latest"
	if version != "" {
		if !safeVersion(version) {
			return release{}, fmt.Errorf("unsafe version %q", version)
		}
		endpoint = githubAPIBase + "/repos/" + repo + "/releases/tags/" + version
	}
	var r release
	if err := getJSON(ctx, endpoint, &r); err != nil {
		return release{}, err
	}
	return r, nil
}

func getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(dst)
}

func findAsset(assets []asset, name string) (asset, bool) {
	for _, a := range assets {
		if a.Name == name && a.URL != "" {
			return a, true
		}
	}
	return asset{}, false
}

func install(ctx context.Context, o options, tag string, binary, checksums asset, out io.Writer) error {
	if o.goos != runtime.GOOS || o.goarch != runtime.GOARCH {
		return fmt.Errorf("refusing to activate target %s/%s from updater running on %s/%s", o.goos, o.goarch, runtime.GOOS, runtime.GOARCH)
	}
	finalDir := filepath.Join(o.root, "versions", tag)
	binaryPath := filepath.Join(finalDir, "pi", "pi")
	if isExecutable(binaryPath) {
		if err := activate(o.binDir, "pi-bun", binaryPath); err != nil {
			return err
		}
		return writeReport(out, o.json, operationReport{Action: "update", Version: tag}, fmt.Sprintf("Pi %s is already installed; activated %s", tag, filepath.Join(o.binDir, "pi-bun")))
	}
	if err := os.MkdirAll(filepath.Join(o.root, "versions"), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(o.root, ".pi-bun-download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	checksumFile, archiveFile := filepath.Join(tmp, "SHA256SUMS"), filepath.Join(tmp, binary.Name)
	if err := download(ctx, checksums.URL, checksumFile); err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	expected, err := parseChecksumFile(checksumFile, binary.Name)
	if err != nil {
		return err
	}
	if err := download(ctx, binary.URL, archiveFile); err != nil {
		return fmt.Errorf("download %s: %w", binary.Name, err)
	}
	if err := verifySHA256(archiveFile, expected); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Join(o.root, "versions"), "."+tag+".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := extractTarGz(archiveFile, stage); err != nil {
		return err
	}
	stagedBinary := filepath.Join(stage, "pi", "pi")
	if !isExecutable(stagedBinary) {
		return fmt.Errorf("archive does not contain a regular pi/pi executable")
	}
	if err := os.Rename(stage, finalDir); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("activate version directory: %w", err)
	}
	if !isExecutable(binaryPath) {
		return fmt.Errorf("installed version %s is incomplete", tag)
	}
	if err := activate(o.binDir, "pi-bun", binaryPath); err != nil {
		return err
	}
	return writeReport(out, o.json, operationReport{Action: "update", Version: tag}, fmt.Sprintf("Installed Pi %s (%s)\nActivated %s", tag, binary.Name, filepath.Join(o.binDir, "pi-bun")))
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

func installedVersions(root string) ([]string, error) {
	dir := filepath.Join(root, "versions")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && safeVersion(entry.Name()) && isExecutable(filepath.Join(dir, entry.Name(), "pi", "pi")) {
			versions = append(versions, entry.Name())
		}
	}
	sort.Slice(versions, func(i, j int) bool { return compareVersions(versions[i], versions[j]) > 0 })
	return versions, nil
}

func compareVersions(a, b string) int {
	parse := func(v string) []int {
		v = strings.TrimPrefix(v, "v")
		parts := strings.Split(strings.SplitN(v, "-", 2)[0], ".")
		out := make([]int, len(parts))
		for i, part := range parts {
			out[i], _ = strconv.Atoi(part)
		}
		return out
	}
	aa, bb := parse(a), parse(b)
	for i := 0; i < len(aa) || i < len(bb); i++ {
		var av, bv int
		if i < len(aa) {
			av = aa[i]
		}
		if i < len(bb) {
			bv = bb[i]
		}
		if av > bv {
			return 1
		}
		if av < bv {
			return -1
		}
	}
	return strings.Compare(a, b)
}

func activeInstallation(root, binDir string) (string, string) {
	link := filepath.Join(binDir, "pi-bun")
	target, err := os.Readlink(link)
	if err != nil {
		return "", ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(binDir, target)
	}
	target = filepath.Clean(target)
	versionsRoot := filepath.Join(root, "versions")
	rel, err := filepath.Rel(versionsRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[1] != "pi" || parts[2] != "pi" || !safeVersion(parts[0]) || !isExecutable(target) {
		return "", ""
	}
	return parts[0], target
}

func acquireLock(root string) (func(), error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(root, ".update.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another pi-bun update is already running")
		}
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

func withLock(root string, fn func() error) error {
	unlock, err := acquireLock(root)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func download(ctx context.Context, url, destination string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, io.LimitReader(resp.Body, 512<<20))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func parseChecksumFile(path, asset string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return parseChecksum(string(data), asset)
}

func parseChecksum(data, asset string) (string, error) {
	var matches []string
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			if len(fields[0]) != 64 {
				return "", fmt.Errorf("invalid SHA-256 for %s", asset)
			}
			if _, err := hex.DecodeString(fields[0]); err != nil {
				return "", fmt.Errorf("invalid SHA-256 for %s", asset)
			}
			matches = append(matches, strings.ToLower(fields[0]))
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one checksum for %s, found %d", asset, len(matches))
	}
	return matches[0], nil
}

func verifySHA256(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expected {
		return fmt.Errorf("SHA-256 mismatch for %s: got %s, expected %s", filepath.Base(path), got, expected)
	}
	return nil
}

func extractTarGz(archive, destination string) error {
	f, err := os.Open(archive)
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
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeArchivePath(destination, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := hdr.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("refusing unsupported archive entry %q", hdr.Name)
		}
	}
}

func safeArchivePath(root, name string) (string, error) {
	clean := filepath.Clean(name)
	if name == "" || filepath.IsAbs(name) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe archive entry %q", name)
	}
	return filepath.Join(root, clean), nil
}

func activate(binDir, name, target string) error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(binDir, name)
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing to replace non-symlink %s", link)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := link + fmt.Sprintf(".next-%d", os.Getpid())
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, link)
}
