package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOperatorLifecycleAgainstReleaseFixture(t *testing.T) {
	assetFile, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	archive := fixtureArchive(t)
	sum := sha256.Sum256(archive)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/earendil-works/pi/releases/latest":
			_ = json.NewEncoder(w).Encode(release{TagName: "v9.9.9", Assets: []asset{{Name: assetFile, URL: server.URL + "/" + assetFile}, {Name: "SHA256SUMS", URL: server.URL + "/SHA256SUMS"}}})
		case "/" + assetFile:
			_, _ = w.Write(archive)
		case "/SHA256SUMS":
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + assetFile + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	oldBase := githubAPIBase
	githubAPIBase = server.URL
	defer func() { githubAPIBase = oldBase }()

	root, binDir := filepath.Join(t.TempDir(), "store"), filepath.Join(t.TempDir(), "bin")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"update", "--force", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("forced update: %v; stderr=%s", err, stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(binDir, "pi")); err != nil {
		t.Fatalf("forced update did not create pi: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(binDir, "pi-bun")); !os.IsNotExist(err) {
		t.Fatalf("forced update created pi-bun: %v", err)
	}
	if got := runJSON(t, []string{"status", "--force", "--json", "--root", root, "--bin-dir", binDir}); !got.UpToDate || got.Status != statusCurrent || got.ActivationName != "pi" || got.ActiveVersion != "v9.9.9" {
		t.Fatalf("unexpected forced status: %+v", got)
	}
	blockedBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(blockedBin, "pi"), []byte("foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"update", "--force", "--dry-run", "--root", root, "--bin-dir", blockedBin}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "refusing to replace non-symlink") {
		t.Fatalf("forced update dry-run did not preflight pi: %v", err)
	}

	if err := run([]string{"update", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("default update: %v; stderr=%s", err, stderr.String())
	}
	if got := runJSON(t, []string{"status", "--json", "--root", root, "--bin-dir", binDir}); !got.UpToDate || got.Status != statusCurrent || got.ActivationName != "pi-bun" || got.ActiveVersion != "v9.9.9" || got.LatestVersion != "v9.9.9" {
		t.Fatalf("unexpected current status: %+v", got)
	}

	createInstalledVersion(t, root, "v9.9.8")
	if err := run([]string{"use", "--root", root, "--bin-dir", binDir, "v9.9.8"}, &stdout, &stderr); err != nil {
		t.Fatalf("use: %v", err)
	}
	status, err := runJSONWithError([]string{"status", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 2 || status.Status != statusBehind || !status.UpdateAvailable || status.UpToDate || status.ActiveVersion != "v9.9.8" || status.LatestVersion != "v9.9.9" {
		t.Fatalf("expected update-available status and exit 2; status=%+v err=%v", status, err)
	}

	createInstalledVersion(t, root, "v9.9.7")
	if err := run([]string{"prune", "--keep", "1", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("prune: %v", err)
	}
	o := options{repo: defaultRepo, root: root, goos: runtime.GOOS, goarch: runtime.GOARCH}
	if installations, err := installationsForVersion(o, "v9.9.7"); err != nil || len(installations) != 0 {
		t.Fatalf("old inactive version survived prune: %v, %v", installations, err)
	}
	if installations, err := installationsForVersion(o, "v9.9.8"); err != nil || len(installations) != 1 {
		t.Fatalf("active version removed by prune: %v, %v", installations, err)
	}
}

func TestMutationsFailWhileAnotherUpdaterHoldsTheLock(t *testing.T) {
	root := t.TempDir()
	unlock, err := acquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var stdout, stderr bytes.Buffer
	err = run([]string{"prune", "--root", root}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "another pi-bun update") {
		t.Fatalf("expected contention error, got %v", err)
	}
}

func TestUpdateRecoversLegacyActivationBeforeReleaseLookupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	useGitHubFixture(t, server.URL)

	root, binDir := t.TempDir(), t.TempDir()
	oldTarget := filepath.Join(root, "previous-pi")
	swapDir := filepath.Join(binDir, ".pi-bun.swap-orphan")
	if err := os.Mkdir(swapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldTarget, filepath.Join(swapDir, "previous")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := run([]string{"update", "--root", root, "--bin-dir", binDir}, &stdout, &stderr)
	if err == nil {
		t.Fatal("update unexpectedly succeeded while release lookup was unavailable")
	}
	got, readErr := os.Readlink(filepath.Join(binDir, "pi-bun"))
	if readErr != nil || got != oldTarget {
		t.Fatalf("release lookup failure left activation unrecovered: target=%q err=%v; update err=%v", got, readErr, err)
	}
	assertNoActivationSwaps(t, binDir, "pi-bun")
}

func fixtureArchive(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho 9.9.9\n")
	if err := tw.WriteHeader(&tar.Header{Name: "pi/pi", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func createInstalledVersion(t *testing.T, root, version string) {
	t.Helper()
	archiveSum := sha256.Sum256([]byte("archive:" + version))
	createInstalledVersionWithOptions(t, options{
		repo: defaultRepo, root: root, goos: runtime.GOOS, goarch: runtime.GOARCH,
	}, version, hex.EncodeToString(archiveSum[:]), []byte("#!/bin/sh\n"))
}

func createInstalledVersionWithOptions(t *testing.T, o options, version, archiveSHA256 string, body []byte) installation {
	t.Helper()
	if o.repo == "" {
		o.repo = defaultRepo
	}
	repository, err := canonicalRepository(o.repo)
	if err != nil {
		t.Fatal(err)
	}
	o.repo = repository
	if o.goos == "" {
		o.goos = runtime.GOOS
	}
	if o.goarch == "" {
		o.goarch = runtime.GOARCH
	}
	assetFile, err := assetName(o.goos, o.goarch)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(installDirectory(o, version, archiveSHA256), "pi", "pi")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	binarySum := sha256.Sum256(body)
	manifest := newManifest(o, version, asset{Name: assetFile}, archiveSHA256, hex.EncodeToString(binarySum[:]))
	if err := writeManifest(filepath.Dir(filepath.Dir(path)), manifest); err != nil {
		t.Fatal(err)
	}
	inst, err := loadInstallation(o.root, filepath.Dir(filepath.Dir(path)))
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func runJSON(t *testing.T, args []string) statusReport {
	t.Helper()
	got, err := runJSONWithError(args)
	if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return got
}

func runJSONWithError(args []string) (statusReport, error) {
	var stdout, stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	var result statusReport
	decodeErr := json.Unmarshal(stdout.Bytes(), &result)
	if decodeErr != nil {
		return statusReport{}, decodeErr
	}
	return result, err
}
