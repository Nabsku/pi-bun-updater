package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDoctorReportsHealthyVerifiedStoreOffline(t *testing.T) {
	root, binDir := t.TempDir(), t.TempDir()
	o := options{repo: defaultRepo, root: root, binDir: binDir, goos: runtime.GOOS, goarch: runtime.GOARCH}
	digest := sha256.Sum256([]byte("archive"))
	inst := createInstalledVersionWithOptions(t, o, "v1.2.3", hex.EncodeToString(digest[:]), []byte("#!/bin/sh\n"))
	if err := activate(binDir, "pi-bun", inst.BinaryPath); err != nil {
		t.Fatal(err)
	}

	report, err := runDoctorJSON([]string{"doctor", "--json", "--root", root, "--bin-dir", binDir})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Healthy || len(report.Findings) != 0 || report.Findings == nil {
		t.Fatalf("doctor report = %+v", report)
	}
}

func TestDoctorIgnoresForeignPiExecutable(t *testing.T) {
	root, binDir := t.TempDir(), t.TempDir()
	foreign := filepath.Join(binDir, "pi")
	contents := []byte("foreign package-manager executable")
	if err := os.WriteFile(foreign, contents, 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := runDoctorJSON([]string{"doctor", "--json", "--root", root, "--bin-dir", binDir})
	if err != nil || !report.Healthy {
		t.Fatalf("foreign pi made doctor unhealthy: %+v, %v", report, err)
	}
	got, readErr := os.ReadFile(foreign)
	if readErr != nil || !bytes.Equal(got, contents) {
		t.Fatalf("doctor changed foreign pi: %q, %v", got, readErr)
	}
}

func TestDoctorAndPurgeDryRunDoNotCreateStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-store")
	binDir := t.TempDir()
	if report, err := runDoctorJSON([]string{"doctor", "--json", "--root", root, "--bin-dir", binDir}); err != nil || !report.Healthy {
		t.Fatalf("doctor on absent store = %+v, %v", report, err)
	}
	if report, err := runPurgeJSON([]string{"purge", "--legacy", "--dry-run", "--json", "--root", root, "--bin-dir", binDir}); err != nil || len(report.Removed) != 0 {
		t.Fatalf("purge dry-run on absent store = %+v, %v", report, err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only command created store: %v", err)
	}
}

func TestDoctorReportsSymlinkedLockWithoutFollowingIt(t *testing.T) {
	root, binDir := t.TempDir(), t.TempDir()
	external := filepath.Join(t.TempDir(), "external-lock")
	contents := []byte("external")
	if err := os.WriteFile(external, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, ".update.lock")
	if err := os.Symlink(external, lock); err != nil {
		t.Fatal(err)
	}

	report, err := runDoctorJSON([]string{"doctor", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 1 || !hasFinding(report.Findings, "update_lock_invalid", lock) {
		t.Fatalf("symlinked lock report = %+v, %v", report, err)
	}
	got, readErr := os.ReadFile(external)
	if readErr != nil || !bytes.Equal(got, contents) {
		t.Fatalf("doctor changed external lock target: %q, %v", got, readErr)
	}
	if unlock, err := acquireLock(root); err == nil {
		unlock()
		t.Fatal("mutating lock followed a symlink")
	}
}

func TestDoctorReportsCorruptionWithoutMutation(t *testing.T) {
	root, binDir := t.TempDir(), t.TempDir()
	o := options{repo: defaultRepo, root: root, binDir: binDir, goos: runtime.GOOS, goarch: runtime.GOARCH}
	digest := sha256.Sum256([]byte("archive"))
	inst := createInstalledVersionWithOptions(t, o, "v1.2.3", hex.EncodeToString(digest[:]), []byte("#!/bin/sh\n"))
	tampered := []byte("#!/bin/sh\necho tampered\n")
	if err := os.WriteFile(inst.BinaryPath, tampered, 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := runDoctorJSON([]string{"doctor", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 1 || report.Healthy || !hasFinding(report.Findings, "installation_corrupt", inst.Directory) {
		t.Fatalf("doctor report = %+v, err = %v", report, err)
	}
	got, readErr := os.ReadFile(inst.BinaryPath)
	if readErr != nil || !bytes.Equal(got, tampered) {
		t.Fatalf("read-only doctor changed corrupt binary: %q, %v", got, readErr)
	}
}

func TestDoctorRepairRecoversRecognizedActivationTransaction(t *testing.T) {
	root, binDir := t.TempDir(), t.TempDir()
	o := options{repo: defaultRepo, root: root, binDir: binDir, goos: runtime.GOOS, goarch: runtime.GOARCH}
	digest := sha256.Sum256([]byte("archive"))
	inst := createInstalledVersionWithOptions(t, o, "v1.2.3", hex.EncodeToString(digest[:]), []byte("#!/bin/sh\n"))
	swap := filepath.Join(binDir, ".pi-bun.swap-orphan")
	if err := os.Mkdir(swap, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inst.BinaryPath, filepath.Join(swap, "previous")); err != nil {
		t.Fatal(err)
	}

	readOnly, err := runDoctorJSON([]string{"doctor", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 1 || !hasFinding(readOnly.Findings, "activation_recovery_artifact", swap) {
		t.Fatalf("read-only doctor report = %+v, err = %v", readOnly, err)
	}
	if _, err := os.Lstat(swap); err != nil {
		t.Fatalf("read-only doctor removed swap: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(binDir, "pi-bun")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only doctor restored activation: %v", err)
	}

	repaired, err := runDoctorJSON([]string{"doctor", "--repair", "--json", "--root", root, "--bin-dir", binDir})
	if err != nil || !repaired.Healthy || len(repaired.Repaired) != 1 || repaired.Repaired[0].Path != swap {
		t.Fatalf("repair report = %+v, err = %v", repaired, err)
	}
	got, readErr := os.Readlink(filepath.Join(binDir, "pi-bun"))
	if readErr != nil || got != inst.BinaryPath {
		t.Fatalf("recovered activation = %q, %v", got, readErr)
	}
	if _, err := os.Lstat(swap); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repaired swap remains: %v", err)
	}
}

func TestDoctorRepairPreservesSuspiciousActivationArtifact(t *testing.T) {
	root, binDir := t.TempDir(), t.TempDir()
	swap := filepath.Join(binDir, ".pi-bun.swap-suspicious")
	if err := os.Mkdir(swap, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(swap, "previous")
	if err := os.Symlink(filepath.Join(root, "old"), previous); err != nil {
		t.Fatal(err)
	}

	report, err := runDoctorJSON([]string{"doctor", "--repair", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 1 || report.Healthy || !hasFinding(report.Findings, "suspicious_activation_artifact", swap) {
		t.Fatalf("suspicious repair report = %+v, %v", report, err)
	}
	if _, err := os.Lstat(previous); err != nil {
		t.Fatalf("suspicious artifact was modified: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(binDir, "pi-bun")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("suspicious artifact was activated: %v", err)
	}
}

func TestPurgeLegacyRequiresExplicitSelectionAndProtectsActivation(t *testing.T) {
	root, binDir := t.TempDir(), t.TempDir()
	legacy := createExactLegacyInstall(t, root, "v1.2.3")
	legacyDirectory := filepath.Dir(filepath.Dir(legacy))
	if err := os.Symlink(legacy, filepath.Join(binDir, "pi-bun")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"purge", "--root", root, "--bin-dir", binDir}, &stdout, &stderr); err == nil {
		t.Fatal("purge without an explicit class succeeded")
	}
	report, err := runPurgeJSON([]string{"purge", "--legacy", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 1 || len(report.Removed) != 0 || len(report.Preserved) != 1 {
		t.Fatalf("active legacy purge = %+v, %v", report, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("active legacy install was removed: %v", err)
	}
	if err := os.Remove(filepath.Join(binDir, "pi-bun")); err != nil {
		t.Fatal(err)
	}
	report, err = runPurgeJSON([]string{"purge", "--legacy", "--json", "--root", root, "--bin-dir", binDir})
	if err != nil || len(report.Removed) != 1 || report.Removed[0] != legacyDirectory {
		t.Fatalf("inactive legacy purge = %+v, %v", report, err)
	}
	if _, err := os.Lstat(legacyDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy directory remains: %v", err)
	}
}

func TestPurgeOrphansRequiresValidPublishedReplacement(t *testing.T) {
	root, binDir := t.TempDir(), t.TempDir()
	o := options{repo: defaultRepo, root: root, binDir: binDir, goos: runtime.GOOS, goarch: runtime.GOARCH}
	digest := sha256.Sum256([]byte("archive"))
	digestText := hex.EncodeToString(digest[:])
	inst := createInstalledVersionWithOptions(t, o, "v1.2.3", digestText, []byte("#!/bin/sh\n"))
	tagDir := filepath.Dir(inst.Directory)
	removableStage := filepath.Join(tagDir, "."+digestText+".staging-orphan")
	if err := os.Mkdir(removableStage, 0o700); err != nil {
		t.Fatal(err)
	}
	missingDigest := sha256.Sum256([]byte("missing published replacement"))
	missingText := hex.EncodeToString(missingDigest[:])
	preservedStage := filepath.Join(tagDir, "."+missingText+".staging-orphan")
	if err := os.Mkdir(preservedStage, 0o700); err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(root, ".pi-bun-download-orphan")
	if err := os.Mkdir(download, 0o700); err != nil {
		t.Fatal(err)
	}

	report, err := runPurgeJSON([]string{"purge", "--orphans", "--json", "--root", root, "--bin-dir", binDir})
	if exitCode(err) != 1 || len(report.Removed) != 2 || len(report.Preserved) != 1 || report.Preserved[0].Path != preservedStage {
		t.Fatalf("orphan purge = %+v, %v", report, err)
	}
	for _, path := range []string{removableStage, download} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("purgeable orphan remains at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(preservedStage); err != nil {
		t.Fatalf("unrecoverable staging directory was removed: %v", err)
	}
	if _, err := loadInstallation(root, inst.Directory); err != nil {
		t.Fatalf("published installation was changed: %v", err)
	}
}

func runDoctorJSON(args []string) (doctorReport, error) {
	var stdout, stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	var report doctorReport
	decodeErr := json.Unmarshal(stdout.Bytes(), &report)
	if decodeErr != nil {
		return doctorReport{}, decodeErr
	}
	return report, err
}

func runPurgeJSON(args []string) (purgeReport, error) {
	var stdout, stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	var report purgeReport
	decodeErr := json.Unmarshal(stdout.Bytes(), &report)
	if decodeErr != nil {
		return purgeReport{}, decodeErr
	}
	return report, err
}

func hasFinding(findings []doctorFinding, code, path string) bool {
	for _, finding := range findings {
		if finding.Code == code && finding.Path == path {
			return true
		}
	}
	return false
}

func createExactLegacyInstall(t *testing.T, root, version string) string {
	t.Helper()
	binary := filepath.Join(root, "versions", version, "pi", "pi")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}
