package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForceUseActivatesPiWithoutCreatingPiBun(t *testing.T) {
	root, binDir := t.TempDir(), filepath.Join(t.TempDir(), "bin")
	createInstalledVersion(t, root, "v1.2.3")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"use", "--force", "--root", root, "--bin-dir", binDir, "v1.2.3"}, &stdout, &stderr); err != nil {
		t.Fatalf("force use: %v; stderr=%s", err, stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(binDir, "pi-bun")); !os.IsNotExist(err) {
		t.Fatalf("force mode created pi-bun: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(binDir, "pi"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "versions", "v1.2.3", "pi", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != want {
		t.Fatalf("pi resolves to %q, want %q", resolved, want)
	}
	if !strings.Contains(stdout.String(), "as pi") {
		t.Fatalf("output does not identify forced activation: %q", stdout.String())
	}
}

func TestForceUseRefusesToReplaceRegularPiExecutable(t *testing.T) {
	root, binDir := t.TempDir(), filepath.Join(t.TempDir(), "bin")
	createInstalledVersion(t, root, "v1.2.3")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(binDir, "pi")
	if err := os.WriteFile(foreign, []byte("foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"use", "--force", "--root", root, "--bin-dir", binDir, "v1.2.3"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "refusing to replace non-symlink") {
		t.Fatalf("expected non-symlink refusal, got %v", err)
	}
	contents, readErr := os.ReadFile(foreign)
	if readErr != nil || string(contents) != "foreign" {
		t.Fatalf("foreign pi changed: contents=%q err=%v", contents, readErr)
	}
}

func TestForceDryRunPreflightsActivationTarget(t *testing.T) {
	root := t.TempDir()
	createInstalledVersion(t, root, "v1.2.3")
	for _, tc := range []struct {
		name      string
		prepare   func(t *testing.T, path string)
		wantError bool
	}{
		{name: "regular-file", prepare: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("foreign"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, wantError: true},
		{name: "foreign-symlink", prepare: func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(target, []byte("foreign"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "dangling-symlink", prepare: func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			tc.prepare(t, filepath.Join(binDir, "pi"))
			var stdout, stderr bytes.Buffer
			err := run([]string{"use", "--force", "--dry-run", "--root", root, "--bin-dir", binDir, "v1.2.3"}, &stdout, &stderr)
			if tc.wantError && (err == nil || !strings.Contains(err.Error(), "refusing to replace non-symlink")) {
				t.Fatalf("expected dry-run refusal, got %v", err)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("dry-run unexpectedly failed: %v", err)
			}
		})
	}
}

func TestPrunePreservesBothManagedActivations(t *testing.T) {
	for _, forcePrune := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-prune", true: "forced-prune"}[forcePrune], func(t *testing.T) {
			root, binDir := t.TempDir(), filepath.Join(t.TempDir(), "bin")
			createInstalledVersion(t, root, "v1.2.3")
			createInstalledVersion(t, root, "v1.2.4")
			createInstalledVersion(t, root, "v1.2.5")
			var stdout, stderr bytes.Buffer
			if err := run([]string{"use", "--force", "--root", root, "--bin-dir", binDir, "v1.2.3"}, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			if err := run([]string{"use", "--root", root, "--bin-dir", binDir, "v1.2.4"}, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			args := []string{"prune", "--json", "--keep", "0", "--root", root, "--bin-dir", binDir}
			if forcePrune {
				args = append(args, "--force")
			}
			stdout.Reset()
			if err := run(args, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			var report operationReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			wantName := map[bool]string{false: "pi-bun", true: "pi"}[forcePrune]
			if report.ActivationName != wantName || len(report.ProtectedActivations) != 2 {
				t.Fatalf("unexpected prune report: %+v", report)
			}
			for _, version := range []string{"v1.2.3", "v1.2.4"} {
				if _, err := os.Stat(filepath.Join(root, "versions", version)); err != nil {
					t.Fatalf("managed active version %s was removed: %v", version, err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "versions", "v1.2.5")); !os.IsNotExist(err) {
				t.Fatalf("inactive version survived prune: %v", err)
			}
		})
	}
}

func TestActivationNameDefaultsToPiBun(t *testing.T) {
	if got := activationName(options{}); got != "pi-bun" {
		t.Fatalf("default activation name = %q", got)
	}
	if got := activationName(options{force: true}); got != "pi" {
		t.Fatalf("forced activation name = %q", got)
	}
}
