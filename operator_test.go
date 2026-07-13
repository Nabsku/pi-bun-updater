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
	"time"
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
	if got := runJSON(t, []string{"status", "--force", "--json", "--root", root, "--bin-dir", binDir}); !got.UpToDate || got.ActivationName != "pi" || got.ActiveVersion != "v9.9.9" {
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
	if got := runJSON(t, []string{"status", "--json", "--root", root, "--bin-dir", binDir}); !got.UpToDate || got.ActivationName != "pi-bun" || got.ActiveVersion != "v9.9.9" || got.LatestVersion != "v9.9.9" {
		t.Fatalf("unexpected current status: %+v", got)
	}

	createInstalledVersion(t, root, "v9.9.8")
	if err := run([]string{"use", "--root", root, "--bin-dir", binDir, "v9.9.8"}, &stdout, &stderr); err != nil {
		t.Fatalf("use: %v", err)
	}
	status, err := runJSONWithError([]string{"status", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 2 || status.UpToDate || status.ActiveVersion != "v9.9.8" || status.LatestVersion != "v9.9.9" {
		t.Fatalf("expected update-available status and exit 2; status=%+v err=%v", status, err)
	}

	createInstalledVersion(t, root, "v9.9.7")
	if err := os.Chtimes(filepath.Join(root, "versions", "v9.9.7"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"prune", "--keep", "1", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "versions", "v9.9.7")); !os.IsNotExist(err) {
		t.Fatalf("old inactive version survived prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "versions", "v9.9.8")); err != nil {
		t.Fatalf("active version removed by prune: %v", err)
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
	path := filepath.Join(root, "versions", version, "pi", "pi")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
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
