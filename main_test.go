package main

import (
	"bytes"
	"errors"
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
	assertNoActivationSwaps(t, binDir, "pi-bun")
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

func TestActivateStagesBeforeAtomicRename(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	oldTarget := filepath.Join(root, "old")
	newTarget := filepath.Join(root, "new")
	for _, path := range []string{oldTarget, newTarget} {
		if err := os.WriteFile(path, []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := activate(binDir, "pi-bun", oldTarget); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("injected rename failure")
	err := activateWithExchange(binDir, "pi-bun", newTarget, func(staged, live string) error {
		gotLive, readErr := os.Readlink(live)
		if readErr != nil || gotLive != oldTarget {
			t.Fatalf("old activation was removed before commit: target=%q err=%v", gotLive, readErr)
		}
		gotStaged, readErr := os.Readlink(staged)
		if readErr != nil || gotStaged != newTarget {
			t.Fatalf("replacement was not fully staged: target=%q err=%v", gotStaged, readErr)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("activate error = %v, want injected failure", err)
	}
	got, err := os.Readlink(filepath.Join(binDir, "pi-bun"))
	if err != nil || got != oldTarget {
		t.Fatalf("failed commit changed live activation: target=%q err=%v", got, err)
	}
	assertNoActivationSwaps(t, binDir, "pi-bun")
}

func TestActivateRollsBackConcurrentForeignFile(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	oldTarget := filepath.Join(root, "old")
	newTarget := filepath.Join(root, "new")
	if err := activate(binDir, "pi-bun", oldTarget); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign executable")
	raced := false
	err := activateWithExchange(binDir, "pi-bun", newTarget, func(staged, live string) error {
		if !raced {
			raced = true
			if err := os.Remove(live); err != nil {
				return err
			}
			if err := os.WriteFile(live, foreign, 0o755); err != nil {
				return err
			}
		}
		return exchangePaths(staged, live)
	})
	if err == nil {
		t.Fatal("activate accepted a concurrent foreign file")
	}
	contents, readErr := os.ReadFile(filepath.Join(binDir, "pi-bun"))
	if readErr != nil || !bytes.Equal(contents, foreign) {
		t.Fatalf("concurrent foreign file changed: contents=%q err=%v", contents, readErr)
	}
	assertNoActivationSwaps(t, binDir, "pi-bun")
}

func TestActivateRecoversLegacyPreviousSymlink(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTarget := filepath.Join(root, "old")
	newTarget := filepath.Join(root, "new")
	swapDir := filepath.Join(binDir, ".pi-bun.swap-orphan")
	if err := os.Mkdir(swapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldTarget, filepath.Join(swapDir, "previous")); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("injected rename failure")
	err := activateWithExchange(binDir, "pi-bun", newTarget, func(_, _ string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("activate error = %v, want injected failure", err)
	}
	got, err := os.Readlink(filepath.Join(binDir, "pi-bun"))
	if err != nil || got != oldTarget {
		t.Fatalf("legacy activation was not recovered: target=%q err=%v", got, err)
	}
	assertNoActivationSwaps(t, binDir, "pi-bun")
}

func TestActivateCleansStalePreparedSwap(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	oldTarget := filepath.Join(root, "old")
	newTarget := filepath.Join(root, "new")
	if err := activate(binDir, "pi-bun", oldTarget); err != nil {
		t.Fatal(err)
	}
	swapDir := filepath.Join(binDir, ".pi-bun.swap-orphan")
	if err := os.Mkdir(swapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "abandoned"), filepath.Join(swapDir, "next")); err != nil {
		t.Fatal(err)
	}
	if err := activate(binDir, "pi-bun", newTarget); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(binDir, "pi-bun"))
	if err != nil || got != newTarget {
		t.Fatalf("activation target = %q, %v; want %q", got, err, newTarget)
	}
	assertNoActivationSwaps(t, binDir, "pi-bun")
}

func TestActivateRecoversInterruptedExchangeWithForeignFile(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(binDir, "pi-bun")
	foreign := []byte("foreign executable")
	if err := os.WriteFile(live, foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	swapDir := filepath.Join(binDir, ".pi-bun.swap-interrupted")
	if err := os.Mkdir(swapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(swapDir, "next")
	if err := os.Symlink(filepath.Join(root, "new"), next); err != nil {
		t.Fatal(err)
	}
	if err := exchangePaths(next, live); err != nil {
		t.Fatal(err)
	}
	if err := activate(binDir, "pi-bun", filepath.Join(root, "other")); err == nil {
		t.Fatal("activation did not refuse the recovered foreign file")
	}
	contents, err := os.ReadFile(live)
	if err != nil || !bytes.Equal(contents, foreign) {
		t.Fatalf("interrupted exchange did not restore foreign file: contents=%q err=%v", contents, err)
	}
	assertNoActivationSwaps(t, binDir, "pi-bun")
}

func TestActivateRestoresLegacyForeignFileAndRefusesReplacement(t *testing.T) {
	binDir := t.TempDir()
	swapDir := filepath.Join(binDir, ".pi-bun.swap-orphan")
	if err := os.Mkdir(swapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(swapDir, "previous")
	if err := os.WriteFile(previous, []byte("foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := activate(binDir, "pi-bun", filepath.Join(t.TempDir(), "new"))
	if err == nil {
		t.Fatal("activate replaced a recovered foreign file")
	}
	contents, readErr := os.ReadFile(filepath.Join(binDir, "pi-bun"))
	if readErr != nil || string(contents) != "foreign" {
		t.Fatalf("recovered foreign file changed: contents=%q err=%v", contents, readErr)
	}
	assertNoActivationSwaps(t, binDir, "pi-bun")
}

func assertNoActivationSwaps(t *testing.T, binDir, name string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(binDir, "."+name+".swap-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("activation left swap artifacts: %v", matches)
	}
}
