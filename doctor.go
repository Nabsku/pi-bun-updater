package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

const (
	doctorWarning = "warning"
	doctorError   = "error"
)

type doctorFinding struct {
	Code       string `json:"code"`
	Severity   string `json:"severity"`
	Path       string `json:"path"`
	Detail     string `json:"detail"`
	Resolution string `json:"resolution,omitempty"`
}

type doctorReport struct {
	Action   string          `json:"action"`
	Healthy  bool            `json:"healthy"`
	Repair   bool            `json:"repair"`
	Findings []doctorFinding `json:"findings"`
	Repaired []doctorFinding `json:"repaired,omitempty"`
}

type purgeReport struct {
	Action    string          `json:"action"`
	Legacy    bool            `json:"legacy"`
	Orphans   bool            `json:"orphans"`
	DryRun    bool            `json:"dry_run,omitempty"`
	Removed   []string        `json:"removed"`
	Preserved []doctorFinding `json:"preserved,omitempty"`
}

func doctorStore(o options, out io.Writer) error {
	findings := inspectDoctor(o)
	var repaired []doctorFinding
	if o.repair {
		before := make(map[string]doctorFinding)
		for _, finding := range findings {
			if finding.Code == "activation_recovery_artifact" {
				before[finding.Path] = finding
			}
		}
		var repairFailures []doctorFinding
		for _, name := range []string{"pi-bun", "pi"} {
			artifacts := inspectActivationSwapArtifacts(o.binDir, name)
			if len(artifacts) == 0 {
				continue
			}
			recognized := true
			for _, artifact := range artifacts {
				if artifact.Code != "activation_recovery_artifact" {
					recognized = false
					break
				}
			}
			if !recognized {
				repairFailures = append(repairFailures, doctorFinding{
					Code:     "activation_repair_failed",
					Severity: doctorError,
					Path:     filepath.Join(o.binDir, name),
					Detail:   "refusing repair while suspicious activation artifacts are present",
				})
				continue
			}
			if err := recoverActivationSwaps(o.binDir, name); err != nil {
				repairFailures = append(repairFailures, doctorFinding{
					Code:     "activation_repair_failed",
					Severity: doctorError,
					Path:     filepath.Join(o.binDir, name),
					Detail:   err.Error(),
				})
			}
		}
		findings = inspectDoctor(o)
		for path, finding := range before {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				repaired = append(repaired, finding)
			}
		}
		findings = append(findings, repairFailures...)
		sortDoctorFindings(repaired)
		sortDoctorFindings(findings)
	}
	report := doctorReport{
		Action:   "doctor",
		Healthy:  len(findings) == 0,
		Repair:   o.repair,
		Findings: nonNilFindings(findings),
		Repaired: repaired,
	}
	text := doctorText(report)
	if err := writeReport(out, o.json, report, text); err != nil {
		return err
	}
	if !report.Healthy {
		return &cliExitError{code: 1, silent: true}
	}
	return nil
}

func inspectDoctor(o options) []doctorFinding {
	var findings []doctorFinding
	inspectStore(o, &findings)
	for _, name := range []string{"pi-bun", "pi"} {
		active := inspectActivation(o.root, o.binDir, name)
		if active.Exists && active.Err != nil && (name != "pi" || managedActivationCandidate(o.root, active)) {
			code, severity, resolution := "activation_invalid", doctorError, ""
			if active.LegacyVersion != "" {
				code, severity, resolution = "legacy_activation", doctorWarning, "run update --version "+active.LegacyVersion
			}
			findings = append(findings, doctorFinding{
				Code:       code,
				Severity:   severity,
				Path:       filepath.Join(o.binDir, name),
				Detail:     active.Err.Error(),
				Resolution: resolution,
			})
		}
		findings = append(findings, inspectActivationSwapArtifacts(o.binDir, name)...)
	}
	sortDoctorFindings(findings)
	return nonNilFindings(findings)
}

func managedActivationCandidate(root string, active activationInspection) bool {
	if active.Installation != nil || active.LegacyVersion != "" {
		return true
	}
	return active.Target != "" && pathContains(versionsDirectory(root), active.Target)
}

func inspectStore(o options, findings *[]doctorFinding) {
	rootInfo, err := os.Lstat(o.root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		*findings = append(*findings, doctorFinding{Code: "store_root_invalid", Severity: doctorError, Path: o.root, Detail: "store root is missing, unreadable, or not a real directory"})
		return
	}
	rootEntries, err := os.ReadDir(o.root)
	if err != nil {
		*findings = append(*findings, doctorFinding{Code: "store_root_unreadable", Severity: doctorError, Path: o.root, Detail: err.Error()})
		return
	}
	lockPath := filepath.Join(o.root, ".update.lock")
	if lockInfo, lockErr := os.Lstat(lockPath); lockErr == nil && (!lockInfo.Mode().IsRegular() || lockInfo.Mode()&os.ModeSymlink != 0) {
		*findings = append(*findings, doctorFinding{Code: "update_lock_invalid", Severity: doctorError, Path: lockPath, Detail: "update lock is not a regular file"})
	} else if lockErr != nil && !errors.Is(lockErr, os.ErrNotExist) {
		*findings = append(*findings, doctorFinding{Code: "update_lock_unreadable", Severity: doctorError, Path: lockPath, Detail: lockErr.Error()})
	}
	for _, entry := range rootEntries {
		if strings.HasPrefix(entry.Name(), ".pi-bun-download-") {
			path := filepath.Join(o.root, entry.Name())
			if isPrivateDirectory(path) && len(entry.Name()) > len(".pi-bun-download-") {
				*findings = append(*findings, doctorFinding{Code: "orphan_download", Severity: doctorWarning, Path: path, Detail: "interrupted download workspace", Resolution: "run purge --orphans"})
			} else {
				*findings = append(*findings, doctorFinding{Code: "suspicious_download_artifact", Severity: doctorError, Path: path, Detail: "download-like entry is not a private directory"})
			}
		}
	}
	versions := versionsDirectory(o.root)
	if _, err := os.Lstat(versions); errors.Is(err, os.ErrNotExist) {
		return
	} else if err != nil || !isDirectoryNoSymlink(versions) {
		*findings = append(*findings, doctorFinding{Code: "versions_directory_invalid", Severity: doctorError, Path: versions, Detail: "versions path is unreadable or not a real directory"})
		return
	}
	entries, err := os.ReadDir(versions)
	if err != nil {
		*findings = append(*findings, doctorFinding{Code: "versions_directory_unreadable", Severity: doctorError, Path: versions, Detail: err.Error()})
		return
	}
	for _, entry := range entries {
		if entry.Name() == storeSchemaDirectory {
			continue
		}
		path := filepath.Join(versions, entry.Name())
		if safeVersion(entry.Name()) && recognizedLegacyDirectory(path) {
			*findings = append(*findings, doctorFinding{Code: "legacy_unverified", Severity: doctorWarning, Path: path, Detail: "tag-only installation has no provenance manifest", Resolution: "run purge --legacy after activating a verified v2 installation"})
			continue
		}
		*findings = append(*findings, doctorFinding{Code: "unrecognized_store_entry", Severity: doctorError, Path: path, Detail: "entry is not a recognized legacy installation or v2 store"})
	}
	schema := schemaDirectory(o.root)
	if _, err := os.Lstat(schema); errors.Is(err, os.ErrNotExist) {
		return
	} else if err != nil || !isDirectoryNoSymlink(schema) {
		*findings = append(*findings, doctorFinding{Code: "schema_directory_invalid", Severity: doctorError, Path: schema, Detail: "v2 path is unreadable or not a real directory"})
		return
	}
	inspectV2Level(o.root, schema, 0, findings)
}

func inspectV2Level(root, directory string, level int, findings *[]doctorFinding) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		*findings = append(*findings, doctorFinding{Code: "store_directory_unreadable", Severity: doctorError, Path: directory, Detail: err.Error()})
		return
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if level == 4 {
			if digest, ok := stagingDigest(entry.Name()); ok {
				if !isPrivateDirectory(path) {
					*findings = append(*findings, doctorFinding{Code: "suspicious_staging_artifact", Severity: doctorError, Path: path, Detail: "staging-like entry is not a private directory", Resolution: "inspect manually"})
					continue
				}
				*findings = append(*findings, doctorFinding{Code: "orphan_staging", Severity: doctorWarning, Path: path, Detail: "interrupted installation staging directory for archive " + digest, Resolution: "run purge --orphans"})
				continue
			}
			if isSHA256(entry.Name()) && isDirectoryNoSymlink(path) {
				if _, err := loadInstallation(root, path); err != nil {
					*findings = append(*findings, doctorFinding{Code: "installation_corrupt", Severity: doctorError, Path: path, Detail: err.Error(), Resolution: "run update --version for this exact tag to reinstall the published build"})
				}
				continue
			}
			*findings = append(*findings, doctorFinding{Code: "unrecognized_store_entry", Severity: doctorError, Path: path, Detail: "entry is not an archive-digest installation or staging directory"})
			continue
		}
		if !validV2Component(level, entry.Name()) || !isDirectoryNoSymlink(path) {
			*findings = append(*findings, doctorFinding{Code: "unrecognized_store_entry", Severity: doctorError, Path: path, Detail: "entry does not match the v2 store layout"})
			continue
		}
		inspectV2Level(root, path, level+1, findings)
	}
}

func validV2Component(level int, name string) bool {
	switch level {
	case 0:
		return isSHA256(name)
	case 1:
		return name == "darwin" || name == "linux"
	case 2:
		return name == "arm64" || name == "amd64"
	case 3:
		return safeVersion(name)
	default:
		return false
	}
}

func stagingDigest(name string) (string, bool) {
	const marker = ".staging-"
	if len(name) <= 1+64+len(marker) || name[0] != '.' {
		return "", false
	}
	digest := name[1 : 1+64]
	if !isSHA256(digest) || !strings.HasPrefix(name[1+64:], marker) || len(name[1+64+len(marker):]) == 0 {
		return "", false
	}
	return digest, true
}

func recognizedLegacyDirectory(directory string) bool {
	if !isDirectoryNoSymlink(directory) {
		return false
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "pi" || !isDirectoryNoSymlink(filepath.Join(directory, "pi")) {
		return false
	}
	piDirectory := filepath.Join(directory, "pi")
	entries, err = os.ReadDir(piDirectory)
	return err == nil && len(entries) == 1 && entries[0].Name() == "pi" && isRegularExecutable(filepath.Join(piDirectory, "pi"))
}

func inspectActivationSwapArtifacts(binDir, name string) []doctorFinding {
	entries, err := os.ReadDir(binDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []doctorFinding{{Code: "activation_directory_unreadable", Severity: doctorError, Path: binDir, Detail: err.Error()}}
	}
	prefix := "." + name + ".swap-"
	var findings []doctorFinding
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(binDir, entry.Name())
		finding := doctorFinding{Code: "activation_recovery_artifact", Severity: doctorWarning, Path: path, Detail: "interrupted activation transaction", Resolution: "run doctor --repair"}
		if len(entry.Name()) == len(prefix) || !isPrivateDirectory(path) {
			finding.Code = "suspicious_activation_artifact"
			finding.Severity = doctorError
			finding.Detail = "swap-like entry is not a private directory"
			finding.Resolution = "inspect manually"
			findings = append(findings, finding)
			continue
		}
		children, readErr := os.ReadDir(path)
		if readErr != nil || len(children) > 1 || len(children) == 1 && children[0].Name() != "next" && children[0].Name() != "previous" {
			finding.Code = "suspicious_activation_artifact"
			finding.Severity = doctorError
			finding.Detail = "swap directory contains unexpected entries"
			finding.Resolution = "inspect manually"
		} else if len(children) == 1 {
			artifactInfo, statErr := os.Lstat(filepath.Join(path, children[0].Name()))
			if statErr != nil || artifactInfo.Mode()&os.ModeSymlink == 0 && !artifactInfo.Mode().IsRegular() {
				finding.Code = "suspicious_activation_artifact"
				finding.Severity = doctorError
				finding.Detail = "swap directory contains an unsupported recovery entry"
				finding.Resolution = "inspect manually"
			}
		}
		findings = append(findings, finding)
	}
	return findings
}

func purgeStore(o options, out io.Writer) error {
	findings := inspectDoctor(o)
	protectedPaths, protectionErr := purgeProtectedPaths(o)
	var removed []string
	var preserved []doctorFinding
	for _, finding := range findings {
		selected := (o.legacy && finding.Code == "legacy_unverified") || (o.orphans && (finding.Code == "orphan_download" || finding.Code == "orphan_staging"))
		if !selected {
			continue
		}
		if reason := purgeSafetyFailure(o, finding, protectedPaths, protectionErr); reason != "" {
			preserved = append(preserved, doctorFinding{Code: "purge_preserved", Severity: doctorError, Path: finding.Path, Detail: reason})
			continue
		}
		removed = append(removed, finding.Path)
		if !o.dryRun {
			if err := os.RemoveAll(finding.Path); err != nil {
				preserved = append(preserved, doctorFinding{Code: "purge_failed", Severity: doctorError, Path: finding.Path, Detail: err.Error()})
				removed = removed[:len(removed)-1]
			}
		}
	}
	sort.Strings(removed)
	sortDoctorFindings(preserved)
	report := purgeReport{Action: "purge", Legacy: o.legacy, Orphans: o.orphans, DryRun: o.dryRun, Removed: nonNilStrings(removed), Preserved: preserved}
	text := "No recognized entries to purge"
	if len(removed) > 0 {
		verb := "Purged"
		if o.dryRun {
			verb = "Would purge"
		}
		text = verb + ": " + strings.Join(removed, ", ")
	} else if len(preserved) > 0 {
		text = "No entries purged"
		if o.dryRun {
			text = "No entries would be purged"
		}
	}
	if len(preserved) > 0 {
		text += fmt.Sprintf("\nPreserved %d entries requiring attention", len(preserved))
	}
	if err := writeReport(out, o.json, report, text); err != nil {
		return err
	}
	if len(preserved) > 0 {
		return &cliExitError{code: 1, silent: true}
	}
	return nil
}

func purgeProtectedPaths(o options) ([]string, error) {
	var protected []string
	for _, name := range []string{"pi-bun", "pi"} {
		link := filepath.Join(o.binDir, name)
		if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("inspect activation %s: %w", link, err)
		}
		traversed, err := traceResolvedPath(link)
		if err != nil {
			return nil, fmt.Errorf("resolve activation %s: %w", link, err)
		}
		protected = append(protected, traversed...)
	}
	return protected, nil
}

// traceResolvedPath follows path with EvalSymlinks semantics while retaining
// every filesystem entry whose removal would break the resolution chain.
func traceResolvedPath(path string) ([]string, error) {
	volumeLength := len(filepath.VolumeName(path))
	if volumeLength < len(path) && os.IsPathSeparator(path[volumeLength]) {
		volumeLength++
	}
	volume := path[:volumeLength]
	destination := volume
	separator := string(os.PathSeparator)
	linksWalked := 0
	var traversed []string

	for start, end := volumeLength, volumeLength; start < len(path); start = end {
		for start < len(path) && os.IsPathSeparator(path[start]) {
			start++
		}
		end = start
		for end < len(path) && !os.IsPathSeparator(path[end]) {
			end++
		}

		windowsDot := runtime.GOOS == "windows" && path[len(filepath.VolumeName(path)):] == "."
		if end == start {
			break
		}
		component := path[start:end]
		if component == "." && !windowsDot {
			continue
		}
		if component == ".." {
			lastSeparator := len(destination) - 1
			for ; lastSeparator >= volumeLength; lastSeparator-- {
				if os.IsPathSeparator(destination[lastSeparator]) {
					break
				}
			}
			if lastSeparator < volumeLength || destination[lastSeparator+1:] == ".." {
				if len(destination) > volumeLength {
					destination += separator
				}
				destination += ".."
			} else {
				destination = destination[:lastSeparator]
			}
			continue
		}

		if len(destination) > len(filepath.VolumeName(destination)) && !os.IsPathSeparator(destination[len(destination)-1]) {
			destination += separator
		}
		destination += component

		info, err := os.Lstat(destination)
		if err != nil {
			return nil, err
		}
		traversed = append(traversed, filepath.Clean(destination))
		if info.Mode()&os.ModeSymlink == 0 {
			if !info.IsDir() && end < len(path) {
				return nil, syscall.ENOTDIR
			}
			continue
		}

		linksWalked++
		if linksWalked > 255 {
			return nil, errors.New("too many symlinks")
		}
		target, err := os.Readlink(destination)
		if err != nil {
			return nil, err
		}
		if windowsDot && !filepath.IsAbs(target) {
			break
		}

		path = target + path[end:]
		targetVolumeLength := len(filepath.VolumeName(target))
		if targetVolumeLength > 0 {
			if targetVolumeLength < len(target) && os.IsPathSeparator(target[targetVolumeLength]) {
				targetVolumeLength++
			}
			volume = target[:targetVolumeLength]
			destination = volume
			end = len(volume)
		} else if len(target) > 0 && os.IsPathSeparator(target[0]) {
			destination = target[:1]
			end = 1
			volume = target[:1]
			volumeLength = 1
		} else {
			lastSeparator := len(destination) - 1
			for ; lastSeparator >= volumeLength; lastSeparator-- {
				if os.IsPathSeparator(destination[lastSeparator]) {
					break
				}
			}
			if lastSeparator < volumeLength {
				destination = volume
			} else {
				destination = destination[:lastSeparator]
			}
			end = 0
		}
	}

	return traversed, nil
}

func purgeSafetyFailure(o options, finding doctorFinding, protectedPaths []string, protectionErr error) string {
	if protectionErr != nil {
		return "active installation protection could not be established: " + protectionErr.Error()
	}
	resolvedCandidate, err := filepath.EvalSymlinks(finding.Path)
	if err != nil {
		return "purge candidate could not be resolved: " + err.Error()
	}
	for _, protected := range protectedPaths {
		if pathContains(resolvedCandidate, protected) {
			return "entry contains an active activation path"
		}
	}
	switch finding.Code {
	case "legacy_unverified":
		if filepath.Dir(finding.Path) != versionsDirectory(o.root) || !safeVersion(filepath.Base(finding.Path)) || !recognizedLegacyDirectory(finding.Path) {
			return "entry is no longer an exact recognized legacy installation"
		}
	case "orphan_download":
		if filepath.Dir(finding.Path) != o.root || !strings.HasPrefix(filepath.Base(finding.Path), ".pi-bun-download-") || !isPrivateDirectory(finding.Path) {
			return "entry is no longer a recognized private download workspace"
		}
	case "orphan_staging":
		digest, ok := stagingDigest(filepath.Base(finding.Path))
		if !ok || !isPrivateDirectory(finding.Path) {
			return "entry is no longer a recognized private staging directory"
		}
		relative, err := filepath.Rel(schemaDirectory(o.root), finding.Path)
		parts := strings.Split(relative, string(filepath.Separator))
		if err != nil || len(parts) != 5 || !isSHA256(parts[0]) || !validV2Component(1, parts[1]) || !validV2Component(2, parts[2]) || !safeVersion(parts[3]) {
			return "staging directory is outside the exact v2 layout"
		}
		final := filepath.Join(filepath.Dir(finding.Path), digest)
		if _, err := loadInstallation(o.root, final); err != nil {
			return "published replacement is missing or invalid; staging data may be needed for manual recovery"
		}
	default:
		return "entry type is not purgeable"
	}
	return ""
}

func pathContains(directory, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(directory), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func isPrivateDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o700
}

func sortDoctorFindings(findings []doctorFinding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Code < findings[j].Code
	})
}

func nonNilFindings(findings []doctorFinding) []doctorFinding {
	if findings == nil {
		return []doctorFinding{}
	}
	return findings
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func doctorText(report doctorReport) string {
	if report.Healthy {
		if len(report.Repaired) > 0 {
			return fmt.Sprintf("doctor: healthy (%d repaired)", len(report.Repaired))
		}
		return "doctor: healthy"
	}
	var builder strings.Builder
	label := "findings"
	if len(report.Findings) == 1 {
		label = "finding"
	}
	fmt.Fprintf(&builder, "doctor: %d %s", len(report.Findings), label)
	for _, finding := range report.Findings {
		fmt.Fprintf(&builder, "\n%s %s %s: %s", finding.Severity, finding.Code, finding.Path, finding.Detail)
	}
	if len(report.Repaired) > 0 {
		fmt.Fprintf(&builder, "\nrepaired: %d", len(report.Repaired))
	}
	return builder.String()
}
