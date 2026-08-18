// pi-bun-update installs the official, checksum-verified Pi Bun binary.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
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
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultRepo         = "earendil-works/pi"
	maxArchiveEntries   = 4096
	maxArchiveFileBytes = 128 << 20
	maxArchiveBytes     = 256 << 20
	maxChecksumBytes    = 1 << 20
)

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

type releaseTarget struct {
	Release       release
	Binary        asset
	Checksums     asset
	ArchiveSHA256 string
}

type options struct {
	repo, version, root, binDir, goos, goarch string
	dryRun, check, json, force                bool
	keep                                      int
}

type statusReport struct {
	ActivationName  string   `json:"activation_name"`
	ActiveVersion   string   `json:"active_version"`
	ActivePath      string   `json:"active_path"`
	Installed       []string `json:"installed_versions"`
	LatestVersion   string   `json:"latest_version"`
	Status          string   `json:"status"`
	Reason          string   `json:"reason,omitempty"`
	UpToDate        bool     `json:"up_to_date"`
	UpdateAvailable bool     `json:"update_available"`
}

type operationReport struct {
	Action               string   `json:"action"`
	ActivationName       string   `json:"activation_name,omitempty"`
	ProtectedActivations []string `json:"protected_activations,omitempty"`
	Version              string   `json:"version,omitempty"`
	Removed              []string `json:"removed,omitempty"`
	DryRun               bool     `json:"dry_run,omitempty"`
}

type cliExitError struct {
	code   int
	silent bool
}

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
		var exitErr *cliExitError
		if exitCode(err) == 1 && (!errors.As(err, &exitErr) || !exitErr.silent) {
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
	fs.BoolVar(&o.force, "force", false, "activate as pi instead of pi-bun (may replace an existing pi symlink)")
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
	if err := normalizePaths(&o); err != nil {
		return err
	}
	repository, err := canonicalRepository(o.repo)
	if err != nil {
		return err
	}
	o.repo = repository
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

func normalizePaths(o *options) error {
	root, err := filepath.Abs(o.root)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}
	binDir, err := filepath.Abs(o.binDir)
	if err != nil {
		return fmt.Errorf("resolve bin-dir: %w", err)
	}
	o.root, o.binDir = filepath.Clean(root), filepath.Clean(binDir)
	return nil
}

func activationName(o options) string {
	if o.force {
		return "pi"
	}
	return "pi-bun"
}

const (
	statusNotInstalled = "not_installed"
	statusCurrent      = "current"
	statusBehind       = "behind"
	statusAhead        = "ahead"
	statusCorrupt      = "corrupt"
)

type statusEvaluation struct {
	Status string
	Reason string
}

func evaluateStatus(o options, target releaseTarget, active activationInspection) statusEvaluation {
	if !active.Exists {
		return statusEvaluation{Status: statusNotInstalled}
	}
	if active.Err != nil || active.Installation == nil {
		reason := "activation_invalid"
		if active.LegacyVersion != "" {
			reason = "legacy_unverified"
		}
		return statusEvaluation{Status: statusCorrupt, Reason: reason}
	}
	inst := *active.Installation
	if !installationMatchesOptions(inst, o) {
		return statusEvaluation{Status: statusCorrupt, Reason: "provenance_mismatch"}
	}
	comparison := compareVersions(inst.Manifest.Tag, target.Release.TagName)
	if comparison < 0 {
		return statusEvaluation{Status: statusBehind}
	}
	if comparison > 0 {
		return statusEvaluation{Status: statusAhead}
	}
	if inst.Manifest.Tag != target.Release.TagName || inst.Manifest.Asset != target.Binary.Name || inst.Manifest.ArchiveSHA256 != target.ArchiveSHA256 {
		return statusEvaluation{Status: statusCorrupt, Reason: "target_identity_mismatch"}
	}
	return statusEvaluation{Status: statusCurrent}
}

func activeVersion(active activationInspection) string {
	if active.Installation != nil {
		return active.Installation.Manifest.Tag
	}
	return active.LegacyVersion
}

func update(ctx context.Context, o options, out io.Writer) error {
	name := activationName(o)
	if !o.dryRun {
		if err := recoverActivationSwaps(o.binDir, name); err != nil {
			return err
		}
	}
	target, err := resolveTarget(ctx, o)
	if err != nil {
		return err
	}
	if err := preflightActivation(o.binDir, name); err != nil {
		return err
	}
	active := inspectActivation(o.root, o.binDir, name)
	evaluation := evaluateStatus(o, target, active)
	if o.version == "" {
		switch evaluation.Status {
		case statusCurrent:
			return writeReport(out, o.json, operationReport{Action: "update", ActivationName: name, Version: target.Release.TagName}, fmt.Sprintf("Pi %s is already current", target.Release.TagName))
		case statusAhead:
			return writeReport(out, o.json, operationReport{Action: "update", ActivationName: name, Version: activeVersion(active)}, fmt.Sprintf("Pi %s is newer than latest %s; no update performed", activeVersion(active), target.Release.TagName))
		case statusCorrupt:
			if active.LegacyVersion == "" || compareVersions(active.LegacyVersion, target.Release.TagName) > 0 {
				return fmt.Errorf("%s activation is %s; rerun with --version %s to authorize repair or downgrade", name, evaluation.Reason, target.Release.TagName)
			}
		}
	}
	if o.dryRun {
		if err := preflightActivation(o.binDir, name); err != nil {
			return err
		}
		return writeReport(out, o.json, operationReport{Action: "update", ActivationName: name, Version: target.Release.TagName, DryRun: true}, fmt.Sprintf("Would install Pi %s (%s) and activate it as %s", target.Release.TagName, target.Binary.Name, name))
	}
	return install(ctx, o, target, out)
}

func showStatus(ctx context.Context, o options, out io.Writer) error {
	target, err := resolveTarget(ctx, o)
	if err != nil {
		return err
	}
	name := activationName(o)
	active := inspectActivation(o.root, o.binDir, name)
	installed, err := installedVersions(o)
	if err != nil {
		return err
	}
	evaluation := evaluateStatus(o, target, active)
	report := statusReport{
		ActivationName: name,
		ActiveVersion:  activeVersion(active), ActivePath: active.Target, Installed: installed,
		LatestVersion: target.Release.TagName, Status: evaluation.Status, Reason: evaluation.Reason,
		UpToDate: evaluation.Status == statusCurrent, UpdateAvailable: evaluation.Status == statusBehind,
	}
	text := fmt.Sprintf("activation: %s\nactive: %s\nlatest: %s\nstatus: %s", name, displayVersion(report.ActiveVersion), target.Release.TagName, evaluation.Status)
	if evaluation.Reason != "" {
		text += " (" + evaluation.Reason + ")"
	}
	if err := writeReport(out, o.json, report, text); err != nil {
		return err
	}
	switch evaluation.Status {
	case statusBehind:
		return &cliExitError{code: 2, silent: true}
	case statusNotInstalled, statusCorrupt:
		return &cliExitError{code: 1, silent: true}
	}
	return nil
}

func useVersion(o options, version string, out io.Writer) error {
	if o.goos != runtime.GOOS || o.goarch != runtime.GOARCH {
		return fmt.Errorf("refusing to activate target %s/%s from updater running on %s/%s", o.goos, o.goarch, runtime.GOOS, runtime.GOARCH)
	}
	if !safeVersion(version) {
		return fmt.Errorf("unsafe version %q", version)
	}
	installations, err := installationsForVersion(o, version)
	if err != nil {
		return err
	}
	if len(installations) == 0 {
		return fmt.Errorf("Pi %s is not installed or is incomplete", version)
	}
	if len(installations) > 1 {
		return fmt.Errorf("Pi %s has multiple verified builds installed; run update --version %s to select the published build", version, version)
	}
	path := installations[0].BinaryPath
	name := activationName(o)
	if o.dryRun {
		if err := preflightActivation(o.binDir, name); err != nil {
			return err
		}
		return writeReport(out, o.json, operationReport{Action: "use", ActivationName: name, Version: version, DryRun: true}, fmt.Sprintf("Would activate Pi %s as %s", version, name))
	}
	if err := activate(o.binDir, name, path); err != nil {
		return err
	}
	return writeReport(out, o.json, operationReport{Action: "use", ActivationName: name, Version: version}, fmt.Sprintf("Activated Pi %s as %s", version, name))
}

func pruneVersions(o options, out io.Writer) error {
	versions, err := installedVersions(o)
	if err != nil {
		return err
	}
	installations, err := scopedInstallations(o)
	if err != nil {
		return err
	}
	retainedVersions := map[string]bool{}
	protectedDirectories := map[string]bool{}
	protectedVersions := map[string]bool{}
	protected := make([]string, 0, 2)
	for _, name := range []string{"pi-bun", "pi"} {
		active := inspectActivation(o.root, o.binDir, name)
		if active.Installation == nil {
			continue
		}
		version := active.Installation.Manifest.Tag
		if installationMatchesOptions(*active.Installation, o) {
			protectedDirectories[active.Installation.Directory] = true
			protectedVersions[version] = true
		}
		protected = append(protected, name+"="+version)
	}
	for i, version := range versions {
		if i >= o.keep {
			break
		}
		retainedVersions[version] = true
	}
	removedSet := map[string]bool{}
	var removed []string
	for _, inst := range installations {
		if retainedVersions[inst.Manifest.Tag] || protectedDirectories[inst.Directory] {
			continue
		}
		label := inst.Manifest.Tag
		if protectedVersions[inst.Manifest.Tag] {
			label += "@" + inst.Manifest.ArchiveSHA256[:12]
		}
		if !removedSet[label] {
			removedSet[label] = true
			removed = append(removed, label)
		}
		if !o.dryRun {
			if err := os.RemoveAll(inst.Directory); err != nil {
				return err
			}
			_ = os.Remove(filepath.Dir(inst.Directory))
		}
	}
	report := operationReport{
		Action:               "prune",
		ActivationName:       activationName(o),
		ProtectedActivations: protected,
		Removed:              removed,
		DryRun:               o.dryRun,
	}
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

func resolveTarget(ctx context.Context, o options) (releaseTarget, error) {
	r, binary, checksums, err := resolveRelease(ctx, o)
	if err != nil {
		return releaseTarget{}, err
	}
	digest, err := fetchChecksum(ctx, checksums.URL, binary.Name)
	if err != nil {
		return releaseTarget{}, fmt.Errorf("fetch checksum for %s: %w", binary.Name, err)
	}
	return releaseTarget{Release: r, Binary: binary, Checksums: checksums, ArchiveSHA256: digest}, nil
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
	_, err := canonicalRepository(repo)
	return err
}

type semVersion struct {
	major, minor, patch uint64
	pre                 []string
}

func safeVersion(v string) bool {
	_, ok := parseSemVersion(v)
	return ok
}

func parseSemVersion(v string) (semVersion, bool) {
	v = strings.TrimPrefix(v, "v")
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semVersion{}, false
	}
	values := [3]uint64{}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semVersion{}, false
		}
		parsed, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semVersion{}, false
		}
		values[i] = parsed
	}
	result := semVersion{major: values[0], minor: values[1], patch: values[2]}
	if !hasPre {
		return result, true
	}
	if pre == "" {
		return semVersion{}, false
	}
	result.pre = strings.Split(pre, ".")
	for _, identifier := range result.pre {
		if identifier == "" || (isNumeric(identifier) && len(identifier) > 1 && identifier[0] == '0') {
			return semVersion{}, false
		}
		for _, r := range identifier {
			if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r == '-') {
				return semVersion{}, false
			}
		}
	}
	return result, true
}

func isNumeric(value string) bool {
	return value != "" && strings.Trim(value, "0123456789") == ""
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
	if version != "" && r.TagName != version {
		return release{}, fmt.Errorf("release endpoint returned tag %q for requested tag %q", r.TagName, version)
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

func fetchChecksum(ctx context.Context, url, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxChecksumBytes {
		return "", fmt.Errorf("checksum manifest exceeds %d bytes", maxChecksumBytes)
	}
	return parseChecksum(string(data), assetName)
}

func findAsset(assets []asset, name string) (asset, bool) {
	for _, a := range assets {
		if a.Name == name && a.URL != "" {
			return a, true
		}
	}
	return asset{}, false
}

func install(ctx context.Context, o options, target releaseTarget, out io.Writer) error {
	if o.goos != runtime.GOOS || o.goarch != runtime.GOARCH {
		return fmt.Errorf("refusing to activate target %s/%s from updater running on %s/%s", o.goos, o.goarch, runtime.GOOS, runtime.GOARCH)
	}
	tag := target.Release.TagName
	if err := ensureTagLayout(o, tag); err != nil {
		return err
	}
	linkName := activationName(o)
	installed, exists, loadErr := exactInstallation(o, tag, target.ArchiveSHA256)
	if exists && loadErr == nil {
		if err := activate(o.binDir, linkName, installed.BinaryPath); err != nil {
			return err
		}
		return writeReport(out, o.json, operationReport{Action: "update", ActivationName: linkName, Version: tag}, fmt.Sprintf("Pi %s is already installed; activated %s", tag, filepath.Join(o.binDir, linkName)))
	}
	tmp, err := os.MkdirTemp(o.root, ".pi-bun-download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archiveFile := filepath.Join(tmp, target.Binary.Name)
	if err := download(ctx, target.Binary.URL, archiveFile); err != nil {
		return fmt.Errorf("download %s: %w", target.Binary.Name, err)
	}
	if err := verifySHA256(archiveFile, target.ArchiveSHA256); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(tagDirectory(o, tag), "."+target.ArchiveSHA256+".staging-")
	if err != nil {
		return err
	}
	defer func() {
		if stage != "" {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := extractTarGz(archiveFile, stage); err != nil {
		return err
	}
	stagedBinary := filepath.Join(stage, "pi", "pi")
	if !isRegularExecutable(stagedBinary) {
		return fmt.Errorf("archive does not contain a regular pi/pi executable")
	}
	binarySHA256, err := sha256File(stagedBinary)
	if err != nil {
		return fmt.Errorf("hash extracted binary: %w", err)
	}
	manifest := newManifest(o, tag, target.Binary, target.ArchiveSHA256, binarySHA256)
	if err := writeManifest(stage, manifest); err != nil {
		return fmt.Errorf("write installation manifest: %w", err)
	}
	finalDir := installDirectory(o, tag, target.ArchiveSHA256)
	replaced, err := publishInstallation(stage, finalDir)
	if err != nil {
		return err
	}
	if !replaced {
		stage = ""
	}
	installed, exists, err = exactInstallation(o, tag, target.ArchiveSHA256)
	if err != nil || !exists {
		validationErr := err
		if validationErr == nil {
			validationErr = fmt.Errorf("published installation is missing")
		}
		validationErr = fmt.Errorf("validate published installation: %w", validationErr)
		if replaced {
			if rollbackErr := exchangePaths(stage, finalDir); rollbackErr != nil {
				preserved := stage
				stage = ""
				return errors.Join(validationErr, fmt.Errorf("rollback failed; previous installation preserved at %s: %w", preserved, rollbackErr))
			}
		}
		return validationErr
	}
	if replaced {
		if err := os.RemoveAll(stage); err != nil {
			return fmt.Errorf("remove replaced installation: %w", err)
		}
		stage = ""
	}
	if err := activate(o.binDir, linkName, installed.BinaryPath); err != nil {
		return err
	}
	return writeReport(out, o.json, operationReport{Action: "update", ActivationName: linkName, Version: tag}, fmt.Sprintf("Installed Pi %s (%s)\nActivated %s", tag, target.Binary.Name, filepath.Join(o.binDir, linkName)))
}

func compareVersions(a, b string) int {
	aa, aok := parseSemVersion(a)
	bb, bok := parseSemVersion(b)
	if !aok || !bok {
		return strings.Compare(a, b)
	}
	for _, pair := range [][2]uint64{{aa.major, bb.major}, {aa.minor, bb.minor}, {aa.patch, bb.patch}} {
		if pair[0] > pair[1] {
			return 1
		}
		if pair[0] < pair[1] {
			return -1
		}
	}
	if len(aa.pre) == 0 && len(bb.pre) > 0 {
		return 1
	}
	if len(aa.pre) > 0 && len(bb.pre) == 0 {
		return -1
	}
	for i := 0; i < len(aa.pre) && i < len(bb.pre); i++ {
		an, bn := isNumeric(aa.pre[i]), isNumeric(bb.pre[i])
		if an && bn {
			if len(aa.pre[i]) > len(bb.pre[i]) {
				return 1
			}
			if len(aa.pre[i]) < len(bb.pre[i]) {
				return -1
			}
			if aa.pre[i] > bb.pre[i] {
				return 1
			}
			if aa.pre[i] < bb.pre[i] {
				return -1
			}
			continue
		}
		if an && !bn {
			return -1
		}
		if !an && bn {
			return 1
		}
		if aa.pre[i] > bb.pre[i] {
			return 1
		}
		if aa.pre[i] < bb.pre[i] {
			return -1
		}
	}
	if len(aa.pre) > len(bb.pre) {
		return 1
	}
	if len(aa.pre) < len(bb.pre) {
		return -1
	}
	return 0
}

func acquireLock(root string) (func(), error) {
	if err := ensureStoreLayout(root); err != nil {
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
	got, err := sha256File(path)
	if err != nil {
		return err
	}
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
	var entries, extracted int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxArchiveEntries {
			return fmt.Errorf("archive has more than %d entries", maxArchiveEntries)
		}
		if hdr.Name != "pi" && !strings.HasPrefix(hdr.Name, "pi/") {
			return fmt.Errorf("refusing archive entry outside pi/: %q", hdr.Name)
		}
		if hdr.Size < 0 || hdr.Size > maxArchiveFileBytes || extracted+hdr.Size > maxArchiveBytes {
			return fmt.Errorf("archive entry %q exceeds extraction limits", hdr.Name)
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
			n, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != hdr.Size {
				return fmt.Errorf("truncated archive entry %q", hdr.Name)
			}
			extracted += n
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

func preflightActivation(binDir, name string) error {
	link := filepath.Join(binDir, name)
	info, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing to replace non-symlink %s", link)
	}
	return nil
}

func recoverActivationSwaps(binDir, name string) error {
	entries, err := os.ReadDir(binDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	prefix := "." + name + ".swap-"
	link := filepath.Join(binDir, name)
	matching := make([]string, 0)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			matching = append(matching, filepath.Join(binDir, entry.Name()))
		}
	}
	if len(matching) == 0 {
		return nil
	}
	if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
		legacyCandidates := 0
		for _, swapDir := range matching {
			info, statErr := os.Lstat(swapDir)
			if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			children, readErr := os.ReadDir(swapDir)
			if readErr == nil && len(children) == 1 && children[0].Name() == "previous" {
				legacyCandidates++
			}
		}
		if legacyCandidates > 1 {
			return fmt.Errorf("multiple legacy activation recovery candidates for %s; preserved under %s", link, binDir)
		}
	}
	for _, swapDir := range matching {
		info, err := os.Lstat(swapDir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing suspicious activation swap artifact %s", swapDir)
		}
		children, err := os.ReadDir(swapDir)
		if err != nil {
			return err
		}
		if len(children) == 0 {
			if err := os.Remove(swapDir); err != nil {
				return err
			}
			continue
		}
		if len(children) != 1 || (children[0].Name() != "next" && children[0].Name() != "previous") {
			return fmt.Errorf("refusing suspicious activation swap contents in %s", swapDir)
		}
		artifact := filepath.Join(swapDir, children[0].Name())
		artifactInfo, err := os.Lstat(artifact)
		if err != nil {
			return err
		}
		if children[0].Name() == "next" {
			if artifactInfo.Mode()&os.ModeSymlink == 0 {
				liveInfo, liveErr := os.Lstat(link)
				if liveErr != nil || liveInfo.Mode()&os.ModeSymlink == 0 {
					return fmt.Errorf("refusing suspicious activation swap artifact %s", artifact)
				}
				if err := exchangePaths(artifact, link); err != nil {
					return fmt.Errorf("rollback interrupted activation exchange from %s: %w", artifact, err)
				}
				artifactInfo, err = os.Lstat(artifact)
				if err != nil || artifactInfo.Mode()&os.ModeSymlink == 0 {
					return fmt.Errorf("activation rollback left unexpected entry at %s", artifact)
				}
			}
			if err := os.Remove(artifact); err != nil {
				return err
			}
			if err := os.Remove(swapDir); err != nil {
				return err
			}
			continue
		}

		liveInfo, liveErr := os.Lstat(link)
		if errors.Is(liveErr, os.ErrNotExist) {
			switch {
			case artifactInfo.Mode()&os.ModeSymlink != 0:
				oldTarget, err := os.Readlink(artifact)
				if err != nil {
					return err
				}
				if err := os.Symlink(oldTarget, link); err != nil {
					return fmt.Errorf("restore previous activation: %w", err)
				}
			case artifactInfo.Mode().IsRegular():
				if err := os.Link(artifact, link); err != nil {
					return fmt.Errorf("restore preserved activation file: %w", err)
				}
			default:
				return fmt.Errorf("refusing suspicious legacy activation artifact %s", artifact)
			}
			if err := os.Remove(artifact); err != nil {
				return err
			}
			if err := os.Remove(swapDir); err != nil {
				return err
			}
			continue
		}
		if liveErr != nil {
			return liveErr
		}
		if liveInfo.Mode()&os.ModeSymlink != 0 && artifactInfo.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(artifact); err != nil {
				return err
			}
			if err := os.Remove(swapDir); err != nil {
				return err
			}
			continue
		}
		if liveInfo.Mode().IsRegular() && artifactInfo.Mode().IsRegular() && os.SameFile(liveInfo, artifactInfo) {
			if err := os.Remove(artifact); err != nil {
				return err
			}
			if err := os.Remove(swapDir); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("legacy activation artifact requires manual recovery: %s", artifact)
	}
	return nil
}

func activate(binDir, name, target string) error {
	return activateWithExchange(binDir, name, target, exchangePaths)
}

func activateWithExchange(binDir, name, target string, exchange func(string, string) error) (returnErr error) {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	if err := recoverActivationSwaps(binDir, name); err != nil {
		return err
	}
	if err := preflightActivation(binDir, name); err != nil {
		return err
	}
	link := filepath.Join(binDir, name)
	if err := os.Symlink(target, link); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create activation symlink: %w", err)
	}
	if err := preflightActivation(binDir, name); err != nil {
		return err
	}

	swapDir, err := os.MkdirTemp(binDir, "."+name+".swap-")
	if err != nil {
		return err
	}
	next := filepath.Join(swapDir, "next")
	preserveSwap := false
	defer func() {
		if preserveSwap {
			return
		}
		if next != "" {
			if err := os.Remove(next); err != nil && !errors.Is(err, os.ErrNotExist) {
				returnErr = errors.Join(returnErr, err)
			}
		}
		if swapDir != "" {
			if err := os.Remove(swapDir); err != nil && !errors.Is(err, os.ErrNotExist) {
				returnErr = errors.Join(returnErr, err)
			}
		}
	}()
	if err := os.Symlink(target, next); err != nil {
		return fmt.Errorf("stage activation symlink: %w", err)
	}
	stagedTarget, err := os.Readlink(next)
	if err != nil || stagedTarget != target {
		return fmt.Errorf("verify staged activation symlink: target %q, error %v", stagedTarget, err)
	}
	if err := preflightActivation(binDir, name); err != nil {
		return err
	}
	if err := exchange(next, link); err != nil {
		return fmt.Errorf("atomically exchange activation symlink: %w", err)
	}
	displaced, err := os.Lstat(next)
	if err != nil {
		preserveSwap = true
		return fmt.Errorf("inspect displaced activation at %s: %w", next, err)
	}
	if displaced.Mode()&os.ModeSymlink == 0 {
		if err := exchange(next, link); err != nil {
			preserveSwap = true
			return fmt.Errorf("activation target changed concurrently; rollback failed and displaced entry is preserved at %s: %w", next, err)
		}
		replacementTarget, readErr := os.Readlink(next)
		if readErr != nil || replacementTarget != target {
			preserveSwap = true
			return fmt.Errorf("activation target changed concurrently during rollback; entry preserved at %s", next)
		}
		return fmt.Errorf("refusing to replace non-symlink %s", link)
	}
	if err := os.Remove(next); err != nil {
		return err
	}
	next = ""
	if err := os.Remove(swapDir); err != nil {
		return err
	}
	swapDir = ""
	return nil
}
