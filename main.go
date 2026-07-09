// pi-bun-updater installs the official, checksum-verified Pi Bun binary.
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
	"strings"
	"time"
)

const defaultRepo = "earendil-works/pi"

var httpClient = &http.Client{Timeout: 5 * time.Minute}

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
	dryRun, check                             bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, out, errOut io.Writer) error {
	if len(args) > 0 && args[0] == "update" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("pi-bun", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o options
	fs.StringVar(&o.repo, "repo", defaultRepo, "GitHub owner/repository")
	fs.StringVar(&o.version, "version", "", "release tag (default: latest)")
	fs.StringVar(&o.root, "root", defaultRoot(), "directory holding immutable versions")
	fs.StringVar(&o.binDir, "bin-dir", defaultBinDir(), "directory for the pi-bun symlink")
	fs.StringVar(&o.goos, "os", runtime.GOOS, "target OS: darwin or linux")
	fs.StringVar(&o.goarch, "arch", runtime.GOARCH, "target architecture: arm64 or amd64")
	fs.BoolVar(&o.dryRun, "dry-run", false, "show the release and paths without downloading or changing files")
	fs.BoolVar(&o.check, "check", false, "show the latest compatible release without changing files")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: pi-bun [update] [flags]")
		fmt.Fprintln(errOut, "Install the official checksum-verified Pi Bun binary alongside your Node/Pnpm Pi.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := validateRepo(o.repo); err != nil {
		return err
	}
	assetName, err := assetName(o.goos, o.goarch)
	if err != nil {
		return err
	}
	r, err := fetchRelease(context.Background(), o.repo, o.version)
	if err != nil {
		return err
	}
	if !safeVersion(r.TagName) {
		return fmt.Errorf("unsafe release tag %q", r.TagName)
	}
	binaryAsset, ok := findAsset(r.Assets, assetName)
	if !ok {
		return fmt.Errorf("release %s has no asset %q", r.TagName, assetName)
	}
	checksums, ok := findAsset(r.Assets, "SHA256SUMS")
	if !ok {
		return fmt.Errorf("release %s has no SHA256SUMS asset", r.TagName)
	}
	if o.check || o.dryRun {
		fmt.Fprintf(out, "Pi %s: %s\n", r.TagName, assetName)
		fmt.Fprintf(out, "install: %s\nactivate: %s\n", filepath.Join(o.root, "versions", r.TagName, "pi", "pi"), filepath.Join(o.binDir, "pi-bun"))
		return nil
	}
	if o.goos != runtime.GOOS || o.goarch != runtime.GOARCH {
		return fmt.Errorf("refusing to activate target %s/%s from updater running on %s/%s", o.goos, o.goarch, runtime.GOOS, runtime.GOARCH)
	}
	return install(context.Background(), o, r.TagName, binaryAsset, checksums, out)
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
	endpoint := "https://api.github.com/repos/" + repo + "/releases/latest"
	if version != "" {
		if !safeVersion(version) {
			return release{}, fmt.Errorf("unsafe version %q", version)
		}
		endpoint = "https://api.github.com/repos/" + repo + "/releases/tags/" + version
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(dst); err != nil {
		return err
	}
	return nil
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
	finalDir := filepath.Join(o.root, "versions", tag)
	binaryPath := filepath.Join(finalDir, "pi", "pi")
	if info, err := os.Stat(binaryPath); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
		if err := activate(o.binDir, "pi-bun", binaryPath); err != nil {
			return err
		}
		fmt.Fprintf(out, "Pi %s is already installed; activated %s\n", tag, filepath.Join(o.binDir, "pi-bun"))
		return nil
	}
	if err := os.MkdirAll(filepath.Join(o.root, "versions"), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(o.root, ".pi-bun-download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	checksumFile := filepath.Join(tmp, "SHA256SUMS")
	archiveFile := filepath.Join(tmp, binary.Name)
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
	info, err := os.Stat(stagedBinary)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("archive does not contain a regular pi/pi executable")
	}
	if err := os.Chmod(stagedBinary, info.Mode()|0o755); err != nil {
		return err
	}
	if err := os.Rename(stage, finalDir); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("activate version directory: %w", err)
		}
	}
	if info, err := os.Stat(binaryPath); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("installed version %s is incomplete", tag)
	}
	if err := activate(o.binDir, "pi-bun", binaryPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "Installed Pi %s (%s)\nActivated %s\n", tag, binary.Name, filepath.Join(o.binDir, "pi-bun"))
	return nil
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
