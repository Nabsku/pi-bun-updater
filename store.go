package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	storeSchemaDirectory = "v2"
	manifestFilename     = "manifest.json"
	manifestSchema       = 1
	maxManifestBytes     = 64 << 10
)

type installManifest struct {
	SchemaVersion int    `json:"schema_version"`
	Repository    string `json:"repository"`
	OS            string `json:"os"`
	Architecture  string `json:"architecture"`
	Tag           string `json:"tag"`
	Asset         string `json:"asset"`
	ArchiveSHA256 string `json:"archive_sha256"`
	BinarySHA256  string `json:"binary_sha256"`
}

type installation struct {
	Manifest   installManifest
	Directory  string
	BinaryPath string
}

type activationInspection struct {
	Exists        bool
	Target        string
	Installation  *installation
	LegacyVersion string
	Err           error
}

func canonicalRepository(repo string) (string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." ||
		strings.ContainsAny(repo, "\\?&#\t\r\n") {
		return "", fmt.Errorf("repo must be an owner/repository pair, got %q", repo)
	}
	return strings.ToLower(repo), nil
}

func repositoryKey(repo string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(repo)))
	return hex.EncodeToString(sum[:])
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func versionsDirectory(root string) string {
	return filepath.Join(root, "versions")
}

func schemaDirectory(root string) string {
	return filepath.Join(versionsDirectory(root), storeSchemaDirectory)
}

func scopeDirectory(o options) string {
	return filepath.Join(schemaDirectory(o.root), repositoryKey(o.repo), o.goos, o.goarch)
}

func tagDirectory(o options, tag string) string {
	return filepath.Join(scopeDirectory(o), tag)
}

func installDirectory(o options, tag, archiveSHA256 string) string {
	return filepath.Join(tagDirectory(o, tag), archiveSHA256)
}

func isRegularFileNoSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func isRegularExecutable(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode()&0o111 != 0
}

func isDirectoryNoSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func ensureDirectory(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if !isDirectoryNoSymlink(path) {
		return fmt.Errorf("refusing symlinked or invalid directory %s", path)
	}
	return nil
}

func validateDirectoryChain(root, directory string) error {
	root = filepath.Clean(root)
	directory = filepath.Clean(directory)
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("directory %s is outside store root %s", directory, root)
	}
	if !isDirectoryNoSymlink(root) {
		return fmt.Errorf("refusing symlinked or invalid store root %s", root)
	}
	current := root
	if relative == "." {
		return nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		if !isDirectoryNoSymlink(current) {
			return fmt.Errorf("refusing symlinked or invalid directory %s", current)
		}
	}
	return nil
}

func ensureStoreLayout(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if !isDirectoryNoSymlink(root) {
		return fmt.Errorf("refusing symlinked or invalid store root %s", root)
	}
	if err := ensureDirectory(versionsDirectory(root), 0o755); err != nil {
		return err
	}
	return ensureDirectory(schemaDirectory(root), 0o755)
}

func ensureTagLayout(o options, tag string) error {
	if !safeVersion(tag) {
		return fmt.Errorf("unsafe version %q", tag)
	}
	if _, err := canonicalRepository(o.repo); err != nil {
		return err
	}
	if _, err := assetName(o.goos, o.goarch); err != nil {
		return err
	}
	if err := ensureStoreLayout(o.root); err != nil {
		return err
	}
	paths := []string{
		filepath.Join(schemaDirectory(o.root), repositoryKey(o.repo)),
		filepath.Join(schemaDirectory(o.root), repositoryKey(o.repo), o.goos),
		scopeDirectory(o),
		tagDirectory(o, tag),
	}
	for _, path := range paths {
		if err := ensureDirectory(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func newManifest(o options, tag string, binary asset, archiveSHA256, binarySHA256 string) installManifest {
	return installManifest{
		SchemaVersion: manifestSchema,
		Repository:    o.repo,
		OS:            o.goos,
		Architecture:  o.goarch,
		Tag:           tag,
		Asset:         binary.Name,
		ArchiveSHA256: archiveSHA256,
		BinarySHA256:  binarySHA256,
	}
}

func validateManifest(manifest installManifest) error {
	if manifest.SchemaVersion != manifestSchema {
		return fmt.Errorf("unsupported manifest schema %d", manifest.SchemaVersion)
	}
	repository, err := canonicalRepository(manifest.Repository)
	if err != nil || repository != manifest.Repository {
		return fmt.Errorf("invalid manifest repository %q", manifest.Repository)
	}
	if !safeVersion(manifest.Tag) {
		return fmt.Errorf("invalid manifest tag %q", manifest.Tag)
	}
	wantAsset, err := assetName(manifest.OS, manifest.Architecture)
	if err != nil {
		return fmt.Errorf("invalid manifest target: %w", err)
	}
	if manifest.Asset != wantAsset {
		return fmt.Errorf("manifest asset %q does not match target %s/%s", manifest.Asset, manifest.OS, manifest.Architecture)
	}
	if !isSHA256(manifest.ArchiveSHA256) {
		return fmt.Errorf("invalid manifest archive SHA-256")
	}
	if !isSHA256(manifest.BinarySHA256) {
		return fmt.Errorf("invalid manifest binary SHA-256")
	}
	return nil
}

func writeManifest(directory string, manifest installManifest) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(directory, manifestFilename)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readManifest(path string) (installManifest, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return installManifest{}, fmt.Errorf("manifest is missing or not a regular file")
	}
	if info.Size() > maxManifestBytes {
		return installManifest{}, fmt.Errorf("manifest exceeds %d bytes", maxManifestBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return installManifest{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxManifestBytes+1))
	decoder.DisallowUnknownFields()
	var manifest installManifest
	if err := decoder.Decode(&manifest); err != nil {
		return installManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return installManifest{}, fmt.Errorf("manifest contains trailing JSON")
		}
		return installManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return installManifest{}, err
	}
	return manifest, nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func loadInstallation(root, directory string) (installation, error) {
	if err := validateDirectoryChain(root, directory); err != nil {
		return installation{}, err
	}
	manifest, err := readManifest(filepath.Join(directory, manifestFilename))
	if err != nil {
		return installation{}, err
	}
	wantDirectory := filepath.Join(
		schemaDirectory(root),
		repositoryKey(manifest.Repository),
		manifest.OS,
		manifest.Architecture,
		manifest.Tag,
		manifest.ArchiveSHA256,
	)
	if filepath.Clean(directory) != filepath.Clean(wantDirectory) {
		return installation{}, fmt.Errorf("manifest identity does not match installation path")
	}
	piDirectory := filepath.Join(directory, "pi")
	binaryPath := filepath.Join(piDirectory, "pi")
	if !isDirectoryNoSymlink(piDirectory) || !isRegularExecutable(binaryPath) {
		return installation{}, fmt.Errorf("installation has no regular pi/pi executable")
	}
	digest, err := sha256File(binaryPath)
	if err != nil {
		return installation{}, fmt.Errorf("hash installed binary: %w", err)
	}
	if digest != manifest.BinarySHA256 {
		return installation{}, fmt.Errorf("installed binary SHA-256 mismatch")
	}
	return installation{Manifest: manifest, Directory: directory, BinaryPath: binaryPath}, nil
}

func installationMatchesOptions(inst installation, o options) bool {
	return inst.Manifest.Repository == o.repo && inst.Manifest.OS == o.goos && inst.Manifest.Architecture == o.goarch
}

func exactInstallation(o options, tag, archiveSHA256 string) (installation, bool, error) {
	directory := installDirectory(o, tag, archiveSHA256)
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return installation{}, false, nil
	} else if err != nil {
		return installation{}, false, err
	}
	inst, err := loadInstallation(o.root, directory)
	if err != nil {
		return installation{}, true, err
	}
	if !installationMatchesOptions(inst, o) || inst.Manifest.Tag != tag || inst.Manifest.ArchiveSHA256 != archiveSHA256 {
		return installation{}, true, fmt.Errorf("installation provenance mismatch")
	}
	return inst, true, nil
}

func scopedInstallations(o options) ([]installation, error) {
	if _, err := os.Lstat(o.root); errors.Is(err, os.ErrNotExist) {
		return []installation{}, nil
	} else if err != nil || !isDirectoryNoSymlink(o.root) {
		return nil, fmt.Errorf("refusing symlinked or invalid store root %s", o.root)
	}
	base := scopeDirectory(o)
	if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
		return []installation{}, nil
	} else if err != nil {
		return nil, err
	} else if err := validateDirectoryChain(o.root, base); err != nil {
		return nil, err
	}
	tags, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var installations []installation
	for _, tagEntry := range tags {
		if !tagEntry.IsDir() || !safeVersion(tagEntry.Name()) {
			continue
		}
		tagPath := filepath.Join(base, tagEntry.Name())
		if !isDirectoryNoSymlink(tagPath) {
			continue
		}
		digests, err := os.ReadDir(tagPath)
		if err != nil {
			return nil, err
		}
		for _, digestEntry := range digests {
			if !digestEntry.IsDir() || !isSHA256(digestEntry.Name()) {
				continue
			}
			inst, err := loadInstallation(o.root, filepath.Join(tagPath, digestEntry.Name()))
			if err != nil || !installationMatchesOptions(inst, o) {
				continue
			}
			installations = append(installations, inst)
		}
	}
	sort.Slice(installations, func(i, j int) bool {
		comparison := compareVersions(installations[i].Manifest.Tag, installations[j].Manifest.Tag)
		if comparison != 0 {
			return comparison > 0
		}
		return installations[i].Manifest.ArchiveSHA256 < installations[j].Manifest.ArchiveSHA256
	})
	return installations, nil
}

func installationsForVersion(o options, version string) ([]installation, error) {
	if !safeVersion(version) {
		return nil, fmt.Errorf("unsafe version %q", version)
	}
	installations, err := scopedInstallations(o)
	if err != nil {
		return nil, err
	}
	var matches []installation
	for _, inst := range installations {
		if inst.Manifest.Tag == version {
			matches = append(matches, inst)
		}
	}
	return matches, nil
}

func installedVersions(o options) ([]string, error) {
	installations, err := scopedInstallations(o)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	versions := make([]string, 0, len(installations))
	for _, inst := range installations {
		if seen[inst.Manifest.Tag] {
			continue
		}
		seen[inst.Manifest.Tag] = true
		versions = append(versions, inst.Manifest.Tag)
	}
	return versions, nil
}

func inspectActivation(root, binDir, name string) activationInspection {
	link := filepath.Join(binDir, name)
	info, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return activationInspection{}
	}
	inspection := activationInspection{Exists: true}
	if err != nil {
		inspection.Err = err
		return inspection
	}
	if info.Mode()&os.ModeSymlink == 0 {
		inspection.Target = link
		inspection.Err = fmt.Errorf("activation is not a symlink")
		return inspection
	}
	target, err := os.Readlink(link)
	if err != nil {
		inspection.Err = err
		return inspection
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(binDir, target)
	}
	target = filepath.Clean(target)
	inspection.Target = target

	legacyRoot := versionsDirectory(root)
	rel, err := filepath.Rel(legacyRoot, target)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) == 3 && safeVersion(parts[0]) && parts[1] == "pi" && parts[2] == "pi" {
			inspection.LegacyVersion = parts[0]
			inspection.Err = fmt.Errorf("legacy installation has no provenance manifest")
			return inspection
		}
	}

	versionsRoot := versionsDirectory(root)
	rel, err = filepath.Rel(versionsRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		inspection.Err = fmt.Errorf("activation target is outside the managed store")
		return inspection
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 8 || parts[0] != storeSchemaDirectory || parts[6] != "pi" || parts[7] != "pi" {
		inspection.Err = fmt.Errorf("activation target is not a managed v2 installation")
		return inspection
	}
	directory := filepath.Join(versionsRoot, filepath.Join(parts[:6]...))
	inst, err := loadInstallation(root, directory)
	if err != nil {
		inspection.Err = err
		return inspection
	}
	if filepath.Clean(inst.BinaryPath) != target {
		inspection.Err = fmt.Errorf("activation target does not match manifest")
		return inspection
	}
	inspection.Installation = &inst
	return inspection
}

func publishInstallation(stage, final string) (bool, error) {
	if _, err := os.Lstat(final); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(stage, final); err != nil {
			return false, fmt.Errorf("publish installation: %w", err)
		}
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := exchangePaths(stage, final); err != nil {
		return false, fmt.Errorf("atomically exchange repaired installation: %w", err)
	}
	return true, nil
}
