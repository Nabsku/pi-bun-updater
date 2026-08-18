package main

import (
	"archive/tar"
	"bytes"
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

func TestInstallPersistsProvenanceAndNeverAdoptsLegacyByTag(t *testing.T) {
	assetFile, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	archive := archiveForBinary(t, []byte("#!/bin/sh\necho verified\n"))
	archiveSum := sha256.Sum256(archive)
	archiveSHA256 := hex.EncodeToString(archiveSum[:])
	archiveDownloads := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/earendil-works/pi/releases/latest":
			_ = json.NewEncoder(w).Encode(release{TagName: "v1.2.3", Assets: []asset{
				{Name: assetFile, URL: server.URL + "/" + assetFile},
				{Name: "SHA256SUMS", URL: server.URL + "/SHA256SUMS"},
			}})
		case "/" + assetFile:
			archiveDownloads++
			_, _ = w.Write(archive)
		case "/SHA256SUMS":
			_, _ = w.Write([]byte(archiveSHA256 + "  " + assetFile + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useGitHubFixture(t, server.URL)

	root, binDir := t.TempDir(), t.TempDir()
	legacyBinary := filepath.Join(root, "versions", "v1.2.3", "pi", "pi")
	if err := os.MkdirAll(filepath.Dir(legacyBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyBinary, []byte("#!/bin/sh\necho legacy\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{"update", "--root", root, "--bin-dir", binDir}
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("first update: %v; stderr=%s", err, stderr.String())
	}
	if archiveDownloads != 1 {
		t.Fatalf("archive downloads = %d, want 1", archiveDownloads)
	}
	o := options{repo: defaultRepo, root: root, binDir: binDir, goos: runtime.GOOS, goarch: runtime.GOARCH}
	installations, err := installationsForVersion(o, "v1.2.3")
	if err != nil || len(installations) != 1 {
		t.Fatalf("verified installations = %v, %v", installations, err)
	}
	manifest := installations[0].Manifest
	if manifest.Repository != defaultRepo || manifest.OS != runtime.GOOS || manifest.Architecture != runtime.GOARCH ||
		manifest.Tag != "v1.2.3" || manifest.Asset != assetFile || manifest.ArchiveSHA256 != archiveSHA256 || !isSHA256(manifest.BinarySHA256) {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if _, err := os.Stat(legacyBinary); err != nil {
		t.Fatalf("legacy install was modified or removed: %v", err)
	}
	active := inspectActivation(root, binDir, "pi-bun")
	if active.Installation == nil || active.Installation.Directory != installations[0].Directory || active.Target == legacyBinary {
		t.Fatalf("activation did not move to verified v2 install: %+v", active)
	}

	stdout.Reset()
	stderr.Reset()
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("second update: %v; stderr=%s", err, stderr.String())
	}
	if archiveDownloads != 1 {
		t.Fatalf("verified install was not reused; archive downloads = %d", archiveDownloads)
	}
}

func TestStoreIdentitySeparatesRepositoryAndPlatform(t *testing.T) {
	root := t.TempDir()
	version := "v1.2.3"
	archiveSum := sha256.Sum256([]byte("same release bytes"))
	digest := hex.EncodeToString(archiveSum[:])
	body := []byte("#!/bin/sh\n")
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}

	defaultOptions := options{repo: defaultRepo, root: root, goos: runtime.GOOS, goarch: runtime.GOARCH}
	forkOptions := options{repo: "example/pi", root: root, goos: runtime.GOOS, goarch: runtime.GOARCH}
	platformOptions := options{repo: defaultRepo, root: root, goos: runtime.GOOS, goarch: otherArch}
	defaultInstall := createInstalledVersionWithOptions(t, defaultOptions, version, digest, body)
	forkInstall := createInstalledVersionWithOptions(t, forkOptions, version, digest, body)
	platformInstall := createInstalledVersionWithOptions(t, platformOptions, version, digest, body)

	paths := map[string]bool{defaultInstall.Directory: true, forkInstall.Directory: true, platformInstall.Directory: true}
	if len(paths) != 3 {
		t.Fatalf("provenance identities collided: %q, %q, %q", defaultInstall.Directory, forkInstall.Directory, platformInstall.Directory)
	}
	for _, expectation := range []struct {
		options   options
		directory string
	}{
		{defaultOptions, defaultInstall.Directory},
		{forkOptions, forkInstall.Directory},
		{platformOptions, platformInstall.Directory},
	} {
		o := expectation.options
		versions, err := installedVersions(o)
		if err != nil || len(versions) != 1 || versions[0] != version {
			t.Fatalf("installedVersions(%+v) = %v, %v", o, versions, err)
		}
		installations, err := installationsForVersion(o, version)
		if err != nil || len(installations) != 1 || installations[0].Directory != expectation.directory {
			t.Fatalf("installationsForVersion(%+v) = %v, %v; want %s", o, installations, err, expectation.directory)
		}
	}
	platformOptions.binDir = t.TempDir()
	if err := useVersion(platformOptions, version, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "refusing to activate target") {
		t.Fatalf("use accepted foreign-platform install: %v", err)
	}
}

func TestTamperedInstallIsNotListedOrActivated(t *testing.T) {
	root := t.TempDir()
	o := options{repo: defaultRepo, root: root, binDir: t.TempDir(), goos: runtime.GOOS, goarch: runtime.GOARCH}
	archiveSum := sha256.Sum256([]byte("archive"))
	inst := createInstalledVersionWithOptions(t, o, "v1.2.3", hex.EncodeToString(archiveSum[:]), []byte("#!/bin/sh\n"))
	if err := os.WriteFile(inst.BinaryPath, []byte("#!/bin/sh\necho tampered\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstallation(root, inst.Directory); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("tampered binary validation error = %v", err)
	}
	versions, err := installedVersions(o)
	if err != nil || len(versions) != 0 {
		t.Fatalf("tampered install was listed: %v, %v", versions, err)
	}
	if err := useVersion(o, "v1.2.3", &bytes.Buffer{}); err == nil {
		t.Fatal("tampered install was activated")
	}
}

func TestLegacyInstallIsIgnoredByUseAndPrune(t *testing.T) {
	root, binDir := t.TempDir(), t.TempDir()
	legacyBinary := filepath.Join(root, "versions", "v1.2.3", "pi", "pi")
	if err := os.MkdirAll(filepath.Dir(legacyBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyBinary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"use", "--root", root, "--bin-dir", binDir, "v1.2.3"}, &stdout, &stderr); err == nil {
		t.Fatal("use adopted a legacy tag-only install")
	}
	if err := run([]string{"prune", "--keep", "0", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyBinary); err != nil {
		t.Fatalf("prune removed legacy install: %v", err)
	}
}

func archiveForBinary(t *testing.T, body []byte) []byte {
	t.Helper()
	return archiveWithHeaders(t, []tar.Header{{Name: "pi/pi", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}}, [][]byte{body})
}

func useGitHubFixture(t *testing.T, baseURL string) {
	t.Helper()
	oldBase := githubAPIBase
	githubAPIBase = baseURL
	t.Cleanup(func() { githubAPIBase = oldBase })
}
