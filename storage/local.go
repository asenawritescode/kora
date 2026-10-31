package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type localBackend struct {
	root    string
	locksMu sync.Mutex
	locks   map[string]*localKeyLock
}

type localKeyLock struct {
	mu   sync.Mutex
	refs int
}

func newLocalBackend(cfg Config) *localBackend {
	root := cfg.LocalPath
	if root == "" {
		root = "."
	}
	return &localBackend{root: root, locks: make(map[string]*localKeyLock)}
}

func (b *localBackend) lockKey(key string) func() {
	b.locksMu.Lock()
	lock := b.locks[key]
	if lock == nil {
		lock = &localKeyLock{}
		b.locks[key] = lock
	}
	lock.refs++
	b.locksMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		b.locksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(b.locks, key)
		}
		b.locksMu.Unlock()
	}
}

func (b *localBackend) fullPath(key string) string {
	return filepath.Join(b.root, filepath.FromSlash(key))
}

func (b *localBackend) Put(ctx context.Context, key string, r io.Reader, _ int64, meta FileMeta) (*FileMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unlock := b.lockKey(key)
	defer unlock()
	full := b.fullPath(key)
	if err := recoverLocalPublish(full); err != nil {
		return nil, fmt.Errorf("recover prior local object publication for %q: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return nil, fmt.Errorf("create storage directory for %q: %w", key, err)
	}
	blobTemp, err := stageLocalFile(filepath.Dir(full), filepath.Base(full)+".blob-", func(out *os.File) error {
		hasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(out, hasher), r)
		if copyErr != nil {
			return copyErr
		}
		meta.Size = written
		meta.Checksum = "sha256:" + hex.EncodeToString(hasher.Sum(nil))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("stage local object %q: %w", key, err)
	}
	defer os.Remove(blobTemp)

	meta.Key = key
	if meta.Filename == "" {
		meta.Filename = baseName(key)
	}
	if meta.MIMEType == "" {
		meta.MIMEType = MimeByExt(extOf(key))
	}
	if meta.UploadedAt.IsZero() {
		meta.UploadedAt = time.Now().UTC()
	}
	meta.ModifiedAt = meta.UploadedAt

	data, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata for %q: %w", key, err)
	}
	metaTemp, err := stageLocalFile(filepath.Dir(full), filepath.Base(full)+".meta-", func(out *os.File) error { _, err := out.Write(data); return err })
	if err != nil {
		return nil, fmt.Errorf("stage metadata for %q: %w", key, err)
	}
	defer os.Remove(metaTemp)
	if err := publishLocalObject(full, blobTemp, metaTemp); err != nil {
		return nil, fmt.Errorf("publish local object %q: %w", key, err)
	}
	return &meta, nil
}

func stageLocalFile(dir, pattern string, write func(*os.File) error) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Chmod(0644); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := write(f); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

type localPublishJournal struct {
	BlobExisted bool   `json:"blob_existed"`
	MetaExisted bool   `json:"meta_existed"`
	BlobBackup  string `json:"blob_backup,omitempty"`
	MetaBackup  string `json:"meta_backup,omitempty"`
	BlobTemp    string `json:"blob_temp"`
	MetaTemp    string `json:"meta_temp"`
}

func publishLocalObject(full, blobTemp, metaTemp string) error {
	if err := recoverLocalPublish(full); err != nil {
		return fmt.Errorf("recover previous publication: %w", err)
	}
	dir := filepath.Dir(full)
	metaPath := full + ".meta.json"
	journalPath := full + ".publish-journal.json"
	backupDir := full + ".publish-backups"
	journal := localPublishJournal{BlobTemp: filepath.Base(blobTemp), MetaTemp: filepath.Base(metaTemp)}
	for _, target := range []struct {
		path    string
		backup  *string
		existed *bool
		pattern string
	}{{full, &journal.BlobBackup, &journal.BlobExisted, "blob-"}, {metaPath, &journal.MetaBackup, &journal.MetaExisted, "meta-"}} {
		info, err := os.Lstat(target.path)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("publication target %q is not a regular file", target.path)
		}
		if err := os.MkdirAll(backupDir, 0700); err != nil {
			return fmt.Errorf("create publication backup directory: %w", err)
		}
		if err := syncDirectory(dir); err != nil {
			return fmt.Errorf("sync publication backup directory entry: %w", err)
		}
		backup, err := backupLocalFile(target.path, backupDir, target.pattern)
		if err != nil {
			return fmt.Errorf("backup %q before publication: %w", target.path, err)
		}
		*target.backup = filepath.Base(backup)
		*target.existed = true
	}
	if err := persistLocalPublishJournal(full, journal); err != nil {
		return errors.Join(fmt.Errorf("sync publication journal: %w", err), recoverLocalPublish(full))
	}
	rollback := func(cause error) error {
		return errors.Join(cause, recoverLocalPublish(full))
	}
	if err := os.Rename(metaTemp, metaPath); err != nil {
		return rollback(fmt.Errorf("publish metadata: %w", err))
	}
	if err := os.Rename(blobTemp, full); err != nil {
		return rollback(fmt.Errorf("publish blob: %w", err))
	}
	if err := syncDirectory(dir); err != nil {
		return rollback(fmt.Errorf("sync published object: %w", err))
	}
	if err := os.Remove(journalPath); err != nil {
		return rollback(fmt.Errorf("remove completed publication journal: %w", err))
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync completed publication journal removal: %w", err)
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("remove completed publication backups: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync publication backup cleanup: %w", err)
	}
	return nil
}

func persistLocalPublishJournal(full string, journal localPublishJournal) error {
	dir := filepath.Dir(full)
	backupDir := full + ".publish-backups"
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return fmt.Errorf("create journal backup directory: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync journal backup directory entry: %w", err)
	}
	if err := syncDirectory(backupDir); err != nil {
		return fmt.Errorf("sync journal backups: %w", err)
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	temp, err := stageLocalFile(dir, filepath.Base(full)+".journal-", func(out *os.File) error {
		_, err := out.Write(data)
		return err
	})
	if err != nil {
		return fmt.Errorf("stage publication journal: %w", err)
	}
	defer os.Remove(temp)
	if err := os.Rename(temp, full+".publish-journal.json"); err != nil {
		return fmt.Errorf("publish durable publication journal: %w", err)
	}
	return syncDirectory(dir)
}

func backupLocalFile(source, dir, pattern string) (string, error) {
	return stageLocalFile(dir, pattern, func(out *os.File) error {
		in, err := os.Open(source)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := in.Close()
		return errors.Join(copyErr, closeErr)
	})
}

func restoreLocalFile(target, backup string, existed bool) error {
	if !existed {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	info, err := os.Lstat(backup)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup %q is not a regular file", backup)
	}
	temp, err := backupLocalFile(backup, filepath.Dir(target), filepath.Base(target)+".restore-")
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	return os.Rename(temp, target)
}

func safeJournalBasename(name string) bool {
	return name != "" && filepath.Base(name) == name && name != "." && name != ".."
}

func recoverLocalPublish(full string) error {
	dir := filepath.Dir(full)
	journalPath := full + ".publish-journal.json"
	backupDir := full + ".publish-backups"
	info, err := os.Lstat(journalPath)
	if os.IsNotExist(err) {
		if err := os.RemoveAll(backupDir); err != nil {
			return fmt.Errorf("remove unreferenced publication backups: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("publication journal is not a regular file")
	}
	data, err := os.ReadFile(journalPath)
	if err != nil {
		return err
	}
	var journal localPublishJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return fmt.Errorf("decode publication journal: %w", err)
	}
	base := filepath.Base(full)
	if (journal.BlobTemp != "" && (!safeJournalBasename(journal.BlobTemp) || !strings.HasPrefix(journal.BlobTemp, base+".blob-"))) ||
		(journal.MetaTemp != "" && (!safeJournalBasename(journal.MetaTemp) || !strings.HasPrefix(journal.MetaTemp, base+".meta-"))) {
		return fmt.Errorf("publication journal contains invalid staged paths")
	}
	if journal.BlobExisted {
		if !safeJournalBasename(journal.BlobBackup) || !strings.HasPrefix(journal.BlobBackup, "blob-") {
			return fmt.Errorf("publication journal contains invalid blob backup path")
		}
	}
	if journal.MetaExisted {
		if !safeJournalBasename(journal.MetaBackup) || !strings.HasPrefix(journal.MetaBackup, "meta-") {
			return fmt.Errorf("publication journal contains invalid metadata backup path")
		}
	}
	if err := restoreLocalFile(full, filepath.Join(backupDir, journal.BlobBackup), journal.BlobExisted); err != nil {
		return fmt.Errorf("restore previous blob: %w", err)
	}
	if err := restoreLocalFile(full+".meta.json", filepath.Join(backupDir, journal.MetaBackup), journal.MetaExisted); err != nil {
		return fmt.Errorf("restore previous metadata: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync recovered object: %w", err)
	}
	if err := os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove recovered publication journal: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync recovered journal removal: %w", err)
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("remove recovered publication backups: %w", err)
	}
	if journal.BlobTemp != "" {
		if err := os.Remove(filepath.Join(dir, journal.BlobTemp)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove staged blob after recovery: %w", err)
		}
	}
	if journal.MetaTemp != "" {
		if err := os.Remove(filepath.Join(dir, journal.MetaTemp)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove staged metadata after recovery: %w", err)
		}
	}
	return syncDirectory(dir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (b *localBackend) EnsureBucket(context.Context) error {
	return nil
}

func (b *localBackend) Head(_ context.Context, key string) (*FileMeta, error) {
	unlock := b.lockKey(key)
	defer unlock()
	full := b.fullPath(key)
	if err := recoverLocalPublish(full); err != nil {
		return nil, fmt.Errorf("recover object metadata for %q: %w", key, err)
	}
	st, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if st.IsDir() {
		return nil, ErrNotFound
	}
	meta := &FileMeta{Key: key, Filename: baseName(key), MIMEType: MimeByExt(extOf(key)), Size: st.Size(), ModifiedAt: st.ModTime()}
	data, err := os.ReadFile(full + ".meta.json")
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read metadata for %q: %w", key, err)
	}
	if err == nil {
		var side FileMeta
		if err := json.Unmarshal(data, &side); err != nil {
			return nil, fmt.Errorf("decode metadata for %q: %w", key, err)
		}
		if side.Filename != "" {
			meta.Filename = side.Filename
		}
		if side.MIMEType != "" {
			meta.MIMEType = side.MIMEType
		}
		if side.Checksum != "" {
			meta.Checksum = side.Checksum
		}
		if !side.UploadedAt.IsZero() {
			meta.UploadedAt = side.UploadedAt
		}
		if side.UploadedBy != "" {
			meta.UploadedBy = side.UploadedBy
		}
	}
	return meta, nil
}

func (b *localBackend) Open(_ context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	unlock := b.lockKey(key)
	full := b.fullPath(key)
	if err := recoverLocalPublish(full); err != nil {
		unlock()
		return nil, fmt.Errorf("recover object for %q: %w", key, err)
	}
	f, err := os.Open(full)
	if err != nil {
		unlock()
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			unlock()
			return nil, err
		}
	}
	unlock()
	if length < 0 {
		return f, nil
	}
	return &readCloser{Reader: io.LimitReader(f, length), Closer: f}, nil
}

func (b *localBackend) Delete(_ context.Context, key string) error {
	unlock := b.lockKey(key)
	defer unlock()
	full := b.fullPath(key)
	if err := recoverLocalPublish(full); err != nil {
		return fmt.Errorf("recover object before delete %q: %w", key, err)
	}
	journal := localPublishJournal{}
	backupDir := full + ".publish-backups"
	for _, target := range []struct {
		path    string
		backup  *string
		existed *bool
		pattern string
	}{{full, &journal.BlobBackup, &journal.BlobExisted, "blob-"}, {full + ".meta.json", &journal.MetaBackup, &journal.MetaExisted, "meta-"}} {
		info, err := os.Lstat(target.path)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("delete target %q is not a regular file", target.path)
		}
		if err := os.MkdirAll(backupDir, 0700); err != nil {
			return fmt.Errorf("create deletion backup directory: %w", err)
		}
		backup, err := backupLocalFile(target.path, backupDir, target.pattern)
		if err != nil {
			return fmt.Errorf("backup %q before delete: %w", target.path, err)
		}
		*target.backup = filepath.Base(backup)
		*target.existed = true
	}
	if !journal.BlobExisted && !journal.MetaExisted {
		return nil
	}
	if err := persistLocalPublishJournal(full, journal); err != nil {
		return errors.Join(fmt.Errorf("persist deletion journal for %q: %w", key, err), recoverLocalPublish(full))
	}
	rollback := func(cause error) error {
		return errors.Join(cause, recoverLocalPublish(full))
	}
	for _, path := range []string{full + ".meta.json", full} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return rollback(fmt.Errorf("remove local object file %q: %w", path, err))
		}
	}
	if err := syncDirectory(filepath.Dir(full)); err != nil {
		return rollback(fmt.Errorf("sync local object deletion for %q: %w", key, err))
	}
	if err := os.Remove(full + ".publish-journal.json"); err != nil {
		return rollback(fmt.Errorf("remove completed deletion journal for %q: %w", key, err))
	}
	if err := syncDirectory(filepath.Dir(full)); err != nil {
		return fmt.Errorf("sync completed deletion journal removal for %q: %w", key, err)
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("remove deletion backups for %q: %w", key, err)
	}
	if err := syncDirectory(filepath.Dir(full)); err != nil {
		return fmt.Errorf("sync deletion backup cleanup for %q: %w", key, err)
	}
	return nil
}

func (b *localBackend) URL(_ context.Context, _ string) (string, error) {
	return "", nil
}

// readCloser adapts an io.Reader + io.Closer pair into an io.ReadCloser.
type readCloser struct {
	io.Reader
	io.Closer
}
