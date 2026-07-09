package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAssetNameSupportsEveryUpstreamUnixTarget(t *testing.T) {
	tests := []struct {
		goos, goarch, want string
	}{
		{"darwin", "arm64", "pi-darwin-arm64.tar.gz"},
		{"darwin", "amd64", "pi-darwin-x64.tar.gz"},
		{"linux", "arm64", "pi-linux-arm64.tar.gz"},
		{"linux", "amd64", "pi-linux-x64.tar.gz"},
	}
	for _, tt := range tests {
		t.Run(tt.goos+"/"+tt.goarch, func(t *testing.T) {
			got, err := assetName(tt.goos, tt.goarch)
			if err != nil || got != tt.want {
				t.Fatalf("assetName(%q, %q) = %q, %v; want %q, nil", tt.goos, tt.goarch, got, err, tt.want)
			}
		})
	}
}

func TestAssetNameRejectsUnsupportedTarget(t *testing.T) {
	if _, err := assetName("linux", "riscv64"); err == nil {
		t.Fatal("assetName accepted unsupported linux/riscv64")
	}
}

func TestParseChecksumRequiresSingleMatchingSHA256(t *testing.T) {
	const asset = "pi-linux-arm64.tar.gz"
	const want = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, err := parseChecksum("deadbeef  pi-linux-amd64.tar.gz\n"+want+"  pi-linux-arm64.tar.gz\n", asset)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q", got)
	}
	if _, err := parseChecksum("bad  pi-linux-arm64.tar.gz\nother  pi-linux-arm64.tar.gz\n", asset); err == nil {
		t.Fatal("accepted ambiguous checksum")
	}
}

func TestSafeArchivePathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../escape", "/absolute", "pi/../../escape"} {
		if _, err := safeArchivePath(root, name); err == nil {
			t.Fatalf("accepted unsafe archive entry %q", name)
		}
	}
	got, err := safeArchivePath(root, "pi/pi")
	if err != nil || got != filepath.Join(root, "pi", "pi") {
		t.Fatalf("safeArchivePath = %q, %v", got, err)
	}
}

func TestRunHelpSucceeds(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run --help: %v", err)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Usage: pi-bun")) {
		t.Fatalf("help output missing usage: %q", stderr.String())
	}
}

func TestActivateReplacesOnlyItsManagedSymlink(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	first := filepath.Join(root, "versions", "v1", "pi", "pi")
	second := filepath.Join(root, "versions", "v2", "pi", "pi")
	for _, path := range []string{first, second} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := activate(binDir, "pi-bun", first); err != nil {
		t.Fatal(err)
	}
	if err := activate(binDir, "pi-bun", second); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(binDir, "pi-bun"))
	if err != nil {
		t.Fatal(err)
	}
	if got != second {
		t.Fatalf("symlink = %q, want %q", got, second)
	}
	if err := os.Remove(filepath.Join(binDir, "pi-bun")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "pi-bun"), []byte("foreign executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := activate(binDir, "pi-bun", first); err == nil {
		t.Fatal("activate replaced a non-symlink executable")
	}
}
