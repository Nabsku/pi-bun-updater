package main

import (
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

func TestStatusReportsAheadAndImplicitUpdateDoesNotDowngrade(t *testing.T) {
	latest := "v1.2.0"
	newer := "v1.3.0-rc.1"
	server, downloads := newReleaseFixture(t, latest, map[string][]byte{
		latest: []byte("#!/bin/sh\necho 1.2.0\n"),
		newer:  []byte("#!/bin/sh\necho 1.3.0-rc.1\n"),
	})
	defer server.Close()
	useGitHubFixture(t, server.URL)

	root, binDir := t.TempDir(), t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"update", "--version", newer, "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("install newer release: %v; stderr=%s", err, stderr.String())
	}
	report, err := runJSONWithError([]string{"status", "--json", "--root", root, "--bin-dir", binDir})
	if err != nil || report.Status != statusAhead || report.UpdateAvailable || report.UpToDate || report.ActiveVersion != newer {
		t.Fatalf("ahead status = %+v, %v", report, err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"update", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("implicit update: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "newer than latest") || downloads[latest] != 0 {
		t.Fatalf("implicit downgrade was not suppressed: stdout=%q downloads=%v", stdout.String(), downloads)
	}
	active := inspectActivation(root, binDir, "pi-bun")
	if active.Installation == nil || active.Installation.Manifest.Tag != newer {
		t.Fatalf("implicit update changed newer activation: %+v", active)
	}

	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"update", "--version", latest, "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("explicit downgrade: %v; stderr=%s", err, stderr.String())
	}
	if downloads[latest] != 1 {
		t.Fatalf("explicit downgrade archive downloads = %d, want 1", downloads[latest])
	}
	active = inspectActivation(root, binDir, "pi-bun")
	if active.Installation == nil || active.Installation.Manifest.Tag != latest {
		t.Fatalf("explicit version did not activate target: %+v", active)
	}
}

func TestStatusUsesDistinctNotInstalledAndCorruptExits(t *testing.T) {
	server, _ := newReleaseFixture(t, "v1.2.3", map[string][]byte{"v1.2.3": []byte("#!/bin/sh\n")})
	defer server.Close()
	useGitHubFixture(t, server.URL)

	root, binDir := t.TempDir(), t.TempDir()
	report, err := runJSONWithError([]string{"status", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 1 || report.Status != statusNotInstalled || report.UpdateAvailable {
		t.Fatalf("not-installed status = %+v, %v", report, err)
	}
	foreign := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(foreign, []byte("foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, filepath.Join(binDir, "pi-bun")); err != nil {
		t.Fatal(err)
	}
	report, err = runJSONWithError([]string{"status", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 1 || report.Status != statusCorrupt || report.Reason != "activation_invalid" || report.UpdateAvailable {
		t.Fatalf("corrupt status = %+v, %v", report, err)
	}
}

func TestSameTagDigestChangeRequiresExplicitIntent(t *testing.T) {
	assetFile, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	archive := archiveForBinary(t, []byte("#!/bin/sh\necho first\n"))
	archiveDownloads := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.Sum256(archive)
		switch r.URL.Path {
		case "/repos/earendil-works/pi/releases/latest", "/repos/earendil-works/pi/releases/tags/v1.2.3":
			_ = json.NewEncoder(w).Encode(release{TagName: "v1.2.3", Assets: []asset{
				{Name: assetFile, URL: server.URL + "/" + assetFile},
				{Name: "SHA256SUMS", URL: server.URL + "/SHA256SUMS"},
			}})
		case "/" + assetFile:
			archiveDownloads++
			_, _ = w.Write(archive)
		case "/SHA256SUMS":
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + assetFile + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useGitHubFixture(t, server.URL)

	root, binDir := t.TempDir(), t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"update", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	archive = archiveForBinary(t, []byte("#!/bin/sh\necho republished\n"))
	report, statusErr := runJSONWithError([]string{"status", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(statusErr) != 1 || report.Status != statusCorrupt || report.Reason != "target_identity_mismatch" {
		t.Fatalf("republished release status = %+v, %v", report, statusErr)
	}
	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"update", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "--version v1.2.3") {
		t.Fatalf("implicit same-tag replacement was allowed: %v", err)
	}
	if archiveDownloads != 1 {
		t.Fatalf("implicit update downloaded replacement archive: %d", archiveDownloads)
	}
	if err := run([]string{"update", "--version", "v1.2.3", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("explicit same-tag replacement: %v", err)
	}
	if archiveDownloads != 2 {
		t.Fatalf("explicit replacement archive downloads = %d, want 2", archiveDownloads)
	}
	active := inspectActivation(root, binDir, "pi-bun")
	newSum := sha256.Sum256(archive)
	if active.Installation == nil || active.Installation.Manifest.ArchiveSHA256 != hex.EncodeToString(newSum[:]) {
		t.Fatalf("republished digest was not activated: %+v", active)
	}
	o := options{repo: defaultRepo, root: root, goos: runtime.GOOS, goarch: runtime.GOARCH}
	installations, err := installationsForVersion(o, "v1.2.3")
	if err != nil || len(installations) != 2 || installations[0].Directory == installations[1].Directory {
		t.Fatalf("republished tag did not retain two distinct identities: %v, %v", installations, err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"prune", "--keep", "0", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("prune obsolete digest: %v", err)
	}
	installations, err = installationsForVersion(o, "v1.2.3")
	if err != nil || len(installations) != 1 || installations[0].Manifest.ArchiveSHA256 != hex.EncodeToString(newSum[:]) {
		t.Fatalf("prune did not retain only the active digest: %v, %v", installations, err)
	}
}

func TestExplicitUpdateRepairsTamperedExactInstall(t *testing.T) {
	server, downloads := newReleaseFixture(t, "v1.2.3", map[string][]byte{"v1.2.3": []byte("#!/bin/sh\necho clean\n")})
	defer server.Close()
	useGitHubFixture(t, server.URL)

	root, binDir := t.TempDir(), t.TempDir()
	var stdout, stderr bytes.Buffer
	args := []string{"update", "--root", root, "--bin-dir", binDir}
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	active := inspectActivation(root, binDir, "pi-bun")
	if active.Installation == nil {
		t.Fatalf("missing active install: %+v", active)
	}
	if err := os.WriteFile(active.Installation.BinaryPath, []byte("#!/bin/sh\necho tampered\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(args, &stdout, &stderr); err == nil {
		t.Fatal("implicit update repaired corrupt activation without explicit intent")
	}
	if err := run([]string{"update", "--version", "v1.2.3", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatalf("explicit repair: %v", err)
	}
	if downloads["v1.2.3"] != 2 {
		t.Fatalf("repair archive downloads = %d, want 2", downloads["v1.2.3"])
	}
	active = inspectActivation(root, binDir, "pi-bun")
	if active.Err != nil || active.Installation == nil {
		t.Fatalf("repaired activation is invalid: %+v", active)
	}
	entries, err := os.ReadDir(filepath.Dir(active.Installation.Directory))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".corrupt-") {
			t.Fatalf("repair left quarantine artifact %q", entry.Name())
		}
	}
}

func TestManifestTamperMakesActiveInstallCorrupt(t *testing.T) {
	server, _ := newReleaseFixture(t, "v1.2.3", map[string][]byte{"v1.2.3": []byte("#!/bin/sh\n")})
	defer server.Close()
	useGitHubFixture(t, server.URL)

	root, binDir := t.TempDir(), t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"update", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	active := inspectActivation(root, binDir, "pi-bun")
	if active.Installation == nil {
		t.Fatalf("missing active install: %+v", active)
	}
	manifest := active.Installation.Manifest
	manifest.Repository = "example/pi"
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active.Installation.Directory, manifestFilename), data, 0o644); err != nil {
		t.Fatal(err)
	}
	report, statusErr := runJSONWithError([]string{"status", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(statusErr) != 1 || report.Status != statusCorrupt || report.Reason != "activation_invalid" {
		t.Fatalf("manifest-tamper status = %+v, %v", report, statusErr)
	}
	if err := run([]string{"use", "--root", root, "--bin-dir", binDir, "v1.2.3"}, &stdout, &stderr); err == nil {
		t.Fatal("use activated an install with a tampered manifest")
	}
}

func newReleaseFixture(t *testing.T, latest string, bodies map[string][]byte) (*httptest.Server, map[string]int) {
	t.Helper()
	assetFile, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	archives := make(map[string][]byte, len(bodies))
	for tag, body := range bodies {
		archives[tag] = archiveForBinary(t, body)
	}
	downloads := make(map[string]int)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag := ""
		switch {
		case r.URL.Path == "/repos/earendil-works/pi/releases/latest":
			tag = latest
		case strings.HasPrefix(r.URL.Path, "/repos/earendil-works/pi/releases/tags/"):
			tag = strings.TrimPrefix(r.URL.Path, "/repos/earendil-works/pi/releases/tags/")
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/assets/"), "/")
			if len(parts) != 2 {
				http.NotFound(w, r)
				return
			}
			tag = parts[0]
			archive, ok := archives[tag]
			if !ok {
				http.NotFound(w, r)
				return
			}
			sum := sha256.Sum256(archive)
			switch parts[1] {
			case assetFile:
				downloads[tag]++
				_, _ = w.Write(archive)
			case "SHA256SUMS":
				_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + assetFile + "\n"))
			default:
				http.NotFound(w, r)
			}
			return
		default:
			http.NotFound(w, r)
			return
		}
		if _, ok := archives[tag]; !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(release{TagName: tag, Assets: []asset{
			{Name: assetFile, URL: server.URL + "/assets/" + tag + "/" + assetFile},
			{Name: "SHA256SUMS", URL: server.URL + "/assets/" + tag + "/SHA256SUMS"},
		}})
	}))
	return server, downloads
}
