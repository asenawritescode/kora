package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const configExportManifest = ".kora-export-manifest.json"
const configExportStagePrefix = ".kora-export-stage-"

type configExportManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type configExportManifestData struct {
	Version     int                        `json:"version"`
	Transaction string                     `json:"transaction"`
	Files       []configExportManifestFile `json:"files"`
}

type configExportJournalItem struct {
	Path    string `json:"path"`
	Publish bool   `json:"publish"`
}

type configExportJournal struct {
	Transaction string                    `json:"transaction"`
	Items       []configExportJournalItem `json:"items"`
}

func configExportFilename(directory, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name is empty")
	}
	var slug strings.Builder
	lastSeparator := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			slug.WriteRune(r)
			lastSeparator = false
		} else if !lastSeparator && slug.Len() > 0 {
			slug.WriteByte('_')
			lastSeparator = true
		}
	}
	value := strings.Trim(slug.String(), "_")
	if value == "" {
		return "", fmt.Errorf("name has no portable filename characters")
	}
	return filepath.ToSlash(filepath.Join(directory, value+".yaml")), nil
}

func addConfigExportFile(files map[string][]byte, name string, data []byte) error {
	if err := validateConfigExportPath(name); err != nil {
		return err
	}
	if name == configExportManifest {
		return fmt.Errorf("path is reserved for export metadata")
	}
	if _, exists := files[name]; exists {
		return fmt.Errorf("generated filename collision at %q", name)
	}
	files[name] = append([]byte(nil), data...)
	return nil
}

// publishConfigExport stages and syncs all content before replacing any output
// file. The manifest is the commit marker and is always published last. A
// leftover transaction directory is rolled forward or restored on the next
// export invocation.
func publishConfigExport(path string, files map[string][]byte) error {
	root, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve export path: %w", err)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return fmt.Errorf("create export directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect export directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("export path must be a real directory")
	}
	if err := recoverConfigExports(root); err != nil {
		return fmt.Errorf("recover previous export: %w", err)
	}
	previous, err := readConfigExportManifest(root)
	if err != nil {
		return err
	}

	transaction, err := newConfigExportID()
	if err != nil {
		return fmt.Errorf("create export transaction identity: %w", err)
	}
	stage := filepath.Join(root, configExportStagePrefix+transaction)
	if err := os.Mkdir(stage, 0700); err != nil {
		return fmt.Errorf("create export staging directory: %w", err)
	}
	committedOrRecovered := false
	defer func() {
		if !committedOrRecovered {
			// Preserve staged data when rollback/recovery itself fails.
			return
		}
		_ = os.RemoveAll(stage)
	}()

	if err := os.MkdirAll(filepath.Join(stage, "files"), 0700); err != nil {
		return fmt.Errorf("create staged files directory: %w", err)
	}
	paths := make([]string, 0, len(files))
	for name := range files {
		if err := validateConfigExportPath(name); err != nil || name == configExportManifest {
			if err == nil {
				err = fmt.Errorf("path is reserved for export metadata")
			}
			return fmt.Errorf("invalid export path %q: %w", name, err)
		}
		paths = append(paths, name)
	}
	sort.Strings(paths)
	manifest := configExportManifestData{Version: 1, Transaction: transaction, Files: make([]configExportManifestFile, 0, len(paths))}
	for _, name := range paths {
		if err := writeConfigExportFile(stage, filepath.ToSlash(filepath.Join("files", filepath.FromSlash(name))), files[name]); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
		digest := sha256.Sum256(files[name])
		manifest.Files = append(manifest.Files, configExportManifestFile{Path: name, SHA256: hex.EncodeToString(digest[:])})
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode export manifest: %w", err)
	}
	manifestStagePath := filepath.ToSlash(filepath.Join("files", configExportManifest))
	if err := writeConfigExportFile(stage, manifestStagePath, append(manifestBytes, '\n')); err != nil {
		return fmt.Errorf("stage export manifest: %w", err)
	}

	newPaths := make(map[string]struct{}, len(paths))
	for _, name := range paths {
		newPaths[name] = struct{}{}
	}
	items := make([]configExportJournalItem, 0, len(paths)+len(previous)+1)
	for _, name := range previous {
		if _, keep := newPaths[name]; !keep {
			items = append(items, configExportJournalItem{Path: name, Publish: false})
		}
	}
	for _, name := range paths {
		items = append(items, configExportJournalItem{Path: name, Publish: true})
	}
	items = append(items, configExportJournalItem{Path: configExportManifest, Publish: true})
	journal := configExportJournal{Transaction: transaction, Items: items}
	journalBytes, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("encode export recovery journal: %w", err)
	}
	if err := writeConfigExportFile(stage, "journal.json", journalBytes); err != nil {
		return fmt.Errorf("persist export recovery journal: %w", err)
	}
	if err := syncConfigExportTree(stage); err != nil {
		return err
	}

	if err := publishConfigExportStage(root, stage, journal); err != nil {
		rollbackErr := rollbackConfigExportStage(root, stage, journal)
		if rollbackErr == nil {
			committedOrRecovered = true
			if removeErr := os.RemoveAll(stage); removeErr != nil {
				rollbackErr = removeErr
			}
		}
		return errors.Join(fmt.Errorf("publish staged export: %w", err), rollbackErr)
	}
	if err := syncConfigExportDir(root); err != nil {
		return fmt.Errorf("sync published export directory: %w", err)
	}
	committedOrRecovered = true
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("remove committed export staging directory: %w", err)
	}
	if err := syncConfigExportDir(root); err != nil {
		return fmt.Errorf("sync export cleanup: %w", err)
	}
	return nil
}

func recoverConfigExports(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list export directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), configExportStagePrefix) {
			continue
		}
		stage := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(filepath.Join(stage, "journal.json"))
		if errors.Is(err, os.ErrNotExist) {
			if err := os.RemoveAll(stage); err != nil {
				return fmt.Errorf("remove unused export stage: %w", err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("read export recovery journal: %w", err)
		}
		var journal configExportJournal
		if err := json.Unmarshal(data, &journal); err != nil || journal.Transaction == "" {
			return fmt.Errorf("export recovery journal is invalid")
		}
		manifest, manifestErr := readConfigExportManifestData(root)
		if manifestErr == nil && manifest.Transaction == journal.Transaction {
			if err := os.RemoveAll(stage); err != nil {
				return fmt.Errorf("clean committed export stage: %w", err)
			}
			continue
		}
		if err := rollbackConfigExportStage(root, stage, journal); err != nil {
			return fmt.Errorf("rollback interrupted export: %w", err)
		}
		if err := os.RemoveAll(stage); err != nil {
			return fmt.Errorf("remove recovered export stage: %w", err)
		}
	}
	return nil
}

func publishConfigExportStage(root, stage string, journal configExportJournal) error {
	for index, item := range journal.Items {
		if err := validateConfigExportPath(item.Path); err != nil && item.Path != configExportManifest {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(item.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := backupConfigExportTarget(target, stage, index); err != nil {
			return fmt.Errorf("backup %s: %w", item.Path, err)
		}
		if !item.Publish {
			continue
		}
		source := filepath.Join(stage, "files", filepath.FromSlash(item.Path))
		if item.Path == configExportManifest {
			source = filepath.Join(stage, "files", configExportManifest)
		}
		if err := os.Rename(source, target); err != nil {
			return fmt.Errorf("replace %s: %w", item.Path, err)
		}
		if err := syncConfigExportDir(filepath.Dir(target)); err != nil {
			return err
		}
	}
	return nil
}

func backupConfigExportTarget(target, stage string, index int) error {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("target exists and is not a regular file")
	}
	backup := filepath.Join(stage, "backups", fmt.Sprintf("%06d", index))
	if err := os.MkdirAll(filepath.Dir(backup), 0700); err != nil {
		return err
	}
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	return syncConfigExportDir(filepath.Dir(target))
}

func rollbackConfigExportStage(root, stage string, journal configExportJournal) error {
	var rollbackErr error
	for index := len(journal.Items) - 1; index >= 0; index-- {
		item := journal.Items[index]
		target := filepath.Join(root, filepath.FromSlash(item.Path))
		backup := filepath.Join(stage, "backups", fmt.Sprintf("%06d", index))
		if _, err := os.Stat(backup); err == nil {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErr = errors.Join(rollbackErr, err)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				rollbackErr = errors.Join(rollbackErr, err)
				continue
			}
			if err := os.Rename(backup, target); err != nil {
				rollbackErr = errors.Join(rollbackErr, err)
				continue
			}
		} else if errors.Is(err, os.ErrNotExist) && item.Publish {
			source := filepath.Join(stage, "files", filepath.FromSlash(item.Path))
			if item.Path == configExportManifest {
				source = filepath.Join(stage, "files", configExportManifest)
			}
			if _, sourceErr := os.Stat(source); errors.Is(sourceErr, os.ErrNotExist) {
				if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
					rollbackErr = errors.Join(rollbackErr, err)
				}
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	if rollbackErr == nil {
		rollbackErr = syncConfigExportDir(root)
	}
	return rollbackErr
}

func readConfigExportManifest(root string) ([]string, error) {
	manifest, err := readConfigExportManifestData(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(manifest.Files))
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, file := range manifest.Files {
		if err := validateConfigExportPath(file.Path); err != nil || file.Path == configExportManifest {
			return nil, fmt.Errorf("existing export manifest contains an unsafe path")
		}
		if _, exists := seen[file.Path]; exists {
			return nil, fmt.Errorf("existing export manifest contains duplicate paths")
		}
		seen[file.Path] = struct{}{}
		paths = append(paths, file.Path)
	}
	return paths, nil
}

func readConfigExportManifestData(root string) (configExportManifestData, error) {
	var manifest configExportManifestData
	data, err := os.ReadFile(filepath.Join(root, configExportManifest))
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("decode existing export manifest: %w", err)
	}
	if manifest.Version != 1 || manifest.Transaction == "" {
		return manifest, fmt.Errorf("existing export manifest has unsupported identity")
	}
	return manifest, nil
}

func writeConfigExportFile(root, relative string, data []byte) error {
	if filepath.IsAbs(relative) || strings.Contains(filepath.ToSlash(relative), "../") {
		return fmt.Errorf("unsafe staged path %q", relative)
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
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
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}

func syncConfigExportTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		return syncConfigExportDir(path)
	})
}

func syncConfigExportDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return err
	}
	return nil
}

func validateConfigExportPath(name string) error {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if name == "" || strings.ContainsAny(name, `:\\`) || filepath.IsAbs(name) || clean != name || clean == "." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return fmt.Errorf("path is not a clean relative export path")
	}
	return nil
}

func newConfigExportID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
