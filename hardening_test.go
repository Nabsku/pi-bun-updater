package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledBinaryRejectsSymlinkedPayload(t *testing.T) {
	root := t.TempDir()
	version := "v1.2.3"
	piDir := filepath.Join(root, "versions", version, "pi")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(external, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(piDir, "pi")); err != nil {
		t.Fatal(err)
	}
	if _, ok := installedBinary(root, version); ok {
		t.Fatal("accepted a symlinked installed binary")
	}
	if versions, err := installedVersions(root); err != nil || len(versions) != 0 {
		t.Fatalf("installedVersions = %v, %v", versions, err)
	}
	if err := useVersion(options{root: root, binDir: filepath.Join(root, "bin")}, version, &bytes.Buffer{}); err == nil {
		t.Fatal("use accepted a symlinked installed binary")
	}
}

func TestUseNormalizesRelativeStoreAndBinPaths(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	createInstalledVersion(t, "store", "v1.2.3")
	var out, errOut bytes.Buffer
	if err := run([]string{"use", "--root", "store", "--bin-dir", "bin", "v1.2.3"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join("bin", "pi-bun"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(filepath.Join("store", "versions", "v1.2.3", "pi", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != want {
		t.Fatalf("resolved symlink = %q, want %q", resolved, want)
	}
}

func TestSemVerOrderingAndValidation(t *testing.T) {
	if compareVersions("v1.0.0", "v1.0.0-rc.1") <= 0 {
		t.Fatal("final release did not sort after prerelease")
	}
	if compareVersions("v1.0.0-rc.2", "v1.0.0-rc.1") <= 0 {
		t.Fatal("prerelease ordering is wrong")
	}
	for _, version := range []string{"vdev", "v1.2", "v01.2.3", "v1.2.3-01", "v18446744073709551616.0.0"} {
		if safeVersion(version) {
			t.Fatalf("accepted invalid version %q", version)
		}
	}
}

func TestExtractTarGzRejectsUnexpectedTopLevelPath(t *testing.T) {
	archive := archiveWithHeaders(t, []tar.Header{{Name: "outside", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}}, [][]byte{[]byte("x")})
	path := filepath.Join(t.TempDir(), "payload.tar.gz")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractTarGz(path, t.TempDir()); err == nil {
		t.Fatal("accepted archive payload outside pi/")
	}
}

func TestExtractTarGzRejectsExcessiveEntries(t *testing.T) {
	headers := make([]tar.Header, maxArchiveEntries+1)
	for i := range headers {
		headers[i] = tar.Header{Name: "pi/empty", Mode: 0o755, Typeflag: tar.TypeDir}
	}
	archive := archiveWithHeaders(t, headers, nil)
	path := filepath.Join(t.TempDir(), "many.tar.gz")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractTarGz(path, t.TempDir()); err == nil {
		t.Fatal("accepted excessive archive entries")
	}
}

func archiveWithHeaders(t *testing.T, headers []tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for i, header := range headers {
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if i < len(bodies) && len(bodies[i]) > 0 {
			if _, err := tw.Write(bodies[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}
