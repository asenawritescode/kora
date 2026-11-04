package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalBackendRoundTrip(t *testing.T) {
	dir := t.TempDir()
	b, err := New(Config{Backend: "local", LocalPath: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	key := "sites/acme/files/2026/08/hello.txt"
	content := []byte("hello world")
	meta, err := b.Put(context.Background(), key, bytes.NewReader(content), int64(len(content)), FileMeta{Filename: "hello.txt", MIMEType: "text/plain"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if meta.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", meta.Size, len(content))
	}
	if meta.Checksum == "" {
		t.Fatalf("expected checksum to be set")
	}

	head, err := b.Head(context.Background(), key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Size != int64(len(content)) {
		t.Fatalf("head size = %d, want %d", head.Size, len(content))
	}
	if head.MIMEType != "text/plain" {
		t.Fatalf("mime = %q, want text/plain", head.MIMEType)
	}

	rc, err := b.Open(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("content = %q, want %q", got, content)
	}

	// Range read: bytes [6, 10] = "world".
	rc2, err := b.Open(context.Background(), key, 6, 5)
	if err != nil {
		t.Fatalf("Open range: %v", err)
	}
	got2, _ := io.ReadAll(rc2)
	rc2.Close()
	if string(got2) != "world" {
		t.Fatalf("range = %q, want world", got2)
	}

	if err := b.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Head(context.Background(), key); err != ErrNotFound {
		t.Fatalf("Head after delete = %v, want ErrNotFound", err)
	}
}

func TestLocalBackendPutReportsMetadataSidecarFailure(t *testing.T) {
	b := newLocalBackend(Config{LocalPath: t.TempDir()})
	key := "folder/file.txt"
	if err := os.MkdirAll(filepath.Dir(b.fullPath(key)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.fullPath(key), []byte("previous"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b.fullPath(key)+".meta.json", 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(context.Background(), key, strings.NewReader("body"), 4, FileMeta{}); err == nil {
		t.Fatal("Put succeeded when metadata sidecar could not be written")
	}
	got, err := os.ReadFile(b.fullPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous" {
		t.Fatalf("failed publication replaced existing blob: %q", got)
	}
}

func TestLocalBackendPersistsMetadata(t *testing.T) {
	dir := t.TempDir()
	b, err := New(Config{Backend: "local", LocalPath: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	key := "sites/acme/files/2026/08/recording.mp3"
	content := []byte("fake-audio")
	uploadedAt := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	meta, err := b.Put(context.Background(), key, bytes.NewReader(content), int64(len(content)), FileMeta{
		Filename:   "voice-note.mp3",
		MIMEType:   "audio/mpeg",
		UploadedBy: "alice",
		UploadedAt: uploadedAt,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	head, err := b.Head(context.Background(), key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Filename != "voice-note.mp3" {
		t.Fatalf("filename = %q, want voice-note.mp3", head.Filename)
	}
	if head.MIMEType != "audio/mpeg" {
		t.Fatalf("mime = %q, want audio/mpeg", head.MIMEType)
	}
	if head.Checksum != meta.Checksum || head.Checksum == "" {
		t.Fatalf("checksum = %q, want %q", head.Checksum, meta.Checksum)
	}
	if head.UploadedBy != "alice" {
		t.Fatalf("uploaded_by = %q, want alice", head.UploadedBy)
	}
}

func TestLocalBackendPublicationPreservesFlatFileCompatibility(t *testing.T) {
	dir := t.TempDir()
	b := newLocalBackend(Config{LocalPath: dir})
	key := "objects/record.txt"
	full := b.fullPath(key)

	first, err := b.Put(context.Background(), key, strings.NewReader("first"), 5, FileMeta{Filename: "record.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if blob, err := os.ReadFile(full); err != nil || string(blob) != "first" {
		t.Fatalf("flat compatibility blob = %q, error=%v", blob, err)
	}

	second, err := b.Put(context.Background(), key, strings.NewReader("second"), 6, FileMeta{Filename: "record.txt"})
	if err != nil {
		t.Fatal(err)
	}
	var persisted FileMeta
	data, err := os.ReadFile(full + ".meta.json")
	if err != nil || json.Unmarshal(data, &persisted) != nil {
		t.Fatalf("legacy FileMeta sidecar is unreadable: %v", err)
	}
	rc, err := b.Open(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" || second.Checksum == first.Checksum || persisted.Checksum != second.Checksum {
		t.Fatalf("flat content=%q, API metadata=%+v, persisted metadata=%+v", got, second, persisted)
	}
	if err := b.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(context.Background(), key); err != nil {
		t.Fatalf("repeat delete should be idempotent: %v", err)
	}

	legacyKey := "legacy/item.txt"
	legacyPath := b.fullPath(legacyKey)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("legacy"), 0644); err != nil {
		t.Fatal(err)
	}
	legacyMeta, err := json.Marshal(FileMeta{Key: legacyKey, Filename: "old-name.txt", MIMEType: "text/plain", UploadedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath+".meta.json", legacyMeta, 0644); err != nil {
		t.Fatal(err)
	}
	legacyHead, err := b.Head(context.Background(), legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	if legacyHead.Filename != "old-name.txt" || legacyHead.UploadedBy != "alice" {
		t.Fatalf("legacy metadata not preserved: %+v", legacyHead)
	}
	legacyReader, err := b.Open(context.Background(), legacyKey, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	legacyBytes, err := io.ReadAll(legacyReader)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyReader.Close(); err != nil {
		t.Fatal(err)
	}
	if string(legacyBytes) != "legacy" {
		t.Fatalf("legacy content = %q", legacyBytes)
	}
}

func TestLocalBackendConcurrentReplacementKeepsBlobAndMetadataPaired(t *testing.T) {
	b := newLocalBackend(Config{LocalPath: t.TempDir()})
	key := "concurrent/file.bin"
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := strings.Repeat(string(rune('a'+i)), i+1)
			_, err := b.Put(context.Background(), key, strings.NewReader(payload), int64(len(payload)), FileMeta{})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	meta, err := b.Head(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := b.Open(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 || len(content) != int(meta.Size) {
		t.Fatalf("opened content size %d disagrees with metadata size %d", len(content), meta.Size)
	}
	for _, value := range content {
		if value != content[0] {
			t.Fatalf("opened partial/mixed object content %q", content)
		}
	}
	if _, err := os.Stat(b.fullPath(key) + ".publish-journal.json"); !os.IsNotExist(err) {
		t.Fatalf("successful replacement left a publication journal behind: %v", err)
	}
}

func TestLocalBackendRecoversInterruptedPublicationAfterRestart(t *testing.T) {
	root := t.TempDir()
	key := "recover/file.txt"
	first := newLocalBackend(Config{LocalPath: root})
	if _, err := first.Put(context.Background(), key, strings.NewReader("committed"), 9, FileMeta{Filename: "saved.txt"}); err != nil {
		t.Fatal(err)
	}
	full := first.fullPath(key)
	backupDir := full + ".publish-backups"
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	blobBackup, err := backupLocalFile(full, backupDir, "blob-")
	if err != nil {
		t.Fatal(err)
	}
	metaBackup, err := backupLocalFile(full+".meta.json", backupDir, "meta-")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(full)
	journal := localPublishJournal{
		BlobExisted: true, MetaExisted: true,
		BlobBackup: filepath.Base(blobBackup), MetaBackup: filepath.Base(metaBackup),
		BlobTemp: base + ".blob-interrupted", MetaTemp: base + ".meta-interrupted",
	}
	journalData, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full+".publish-journal.json", journalData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("partial-new-blob"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full+".meta.json", []byte(`{"filename":"partial-new.txt"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(full), journal.BlobTemp), []byte("staged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(full), journal.MetaTemp), []byte("staged"), 0600); err != nil {
		t.Fatal(err)
	}

	// A new backend instance models process restart. The first read must recover
	// the previous complete pair before exposing the object.
	restarted := newLocalBackend(Config{LocalPath: root})
	meta, err := restarted.Head(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Filename != "saved.txt" || meta.Size != int64(len("committed")) {
		t.Fatalf("recovered metadata = %+v", meta)
	}
	reader, err := restarted.Open(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if string(content) != "committed" {
		t.Fatalf("recovered object = %q", content)
	}
	for _, path := range []string{full + ".publish-journal.json", backupDir, filepath.Join(filepath.Dir(full), journal.BlobTemp), filepath.Join(filepath.Dir(full), journal.MetaTemp)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("recovery left %q behind: %v", path, err)
		}
	}
}

func TestLocalBackendRecoversPublicationAfterChildProcessKill(t *testing.T) {
	root := t.TempDir()
	key := "crash/recover.txt"
	backend := newLocalBackend(Config{LocalPath: root})
	if _, err := backend.Put(context.Background(), key, strings.NewReader("old complete blob"), int64(len("old complete blob")), FileMeta{Filename: "old.txt", MIMEType: "text/plain", UploadedBy: "before"}); err != nil {
		t.Fatal("write pre-crash object:", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("locate test executable:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestLocalBackendCrashHelper$")
	command.Env = append(os.Environ(),
		"KORA_STORAGE_CRASH_HELPER=1",
		"KORA_STORAGE_CRASH_ROOT="+root,
		"KORA_STORAGE_CRASH_KEY="+key,
	)
	readySignal := make(chan string, 1)
	var stdout bytes.Buffer
	command.Stdout = &childReadyWriter{buffer: &stdout, ready: readySignal}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal("start crash helper process:", err)
	}
	select {
	case ready := <-readySignal:
		if ready != "READY\n" {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("child signaled unexpected output %q (stdout=%q stderr=%s)", ready, stdout.String(), stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("child did not reach interrupted publication point: stdout=%q stderr=%s", stdout.String(), stderr.String())
	}
	if err := command.Process.Kill(); err != nil {
		_ = command.Wait()
		t.Fatal("kill child at interrupted publication point:", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("child exited normally; expected process termination")
	}

	// Head performs recovery before exposing the sidecar. Then Open must return
	// the matching old blob, never a mixed old/new pair.
	restarted := newLocalBackend(Config{LocalPath: root})
	meta, err := restarted.Head(context.Background(), key)
	if err != nil {
		t.Fatal("recover metadata after process kill:", err)
	}
	if meta.Filename != "old.txt" || meta.UploadedBy != "before" || meta.Size != int64(len("old complete blob")) {
		t.Fatalf("metadata after process kill = %+v", meta)
	}
	reader, err := restarted.Open(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal("open recovered blob:", err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal("read recovered blob:", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal("close recovered blob:", err)
	}
	if string(content) != "old complete blob" {
		t.Fatalf("blob after process kill = %q", content)
	}
	for _, path := range []string{backend.fullPath(key) + ".publish-journal.json", backend.fullPath(key) + ".publish-backups"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("recovery left %q behind: %v", path, err)
		}
	}
}

type childReadyWriter struct {
	mu     sync.Mutex
	buffer *bytes.Buffer
	ready  chan<- string
}

func (w *childReadyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buffer.Write(p)
	w.mu.Unlock()
	if strings.Contains(string(p), "READY\n") {
		w.ready <- "READY\n"
	}
	return n, err
}

// TestLocalBackendCrashHelper is invoked as a child process by the crash
// recovery test above. It deliberately reaches the real journal/rename point
// and then blocks so the parent can terminate it without running defers.
func TestLocalBackendCrashHelper(t *testing.T) {
	if os.Getenv("KORA_STORAGE_CRASH_HELPER") != "1" {
		return
	}
	root, key := os.Getenv("KORA_STORAGE_CRASH_ROOT"), os.Getenv("KORA_STORAGE_CRASH_KEY")
	if root == "" || key == "" {
		t.Fatal("crash helper requires a root and key")
	}
	full := filepath.Join(root, filepath.FromSlash(key))
	dir, base := filepath.Dir(full), filepath.Base(full)
	blobTemp, err := stageLocalFile(dir, base+".blob-", func(out *os.File) error {
		_, err := out.Write([]byte("new blob must not publish"))
		return err
	})
	if err != nil {
		t.Fatal("stage child blob:", err)
	}
	metaTemp, err := stageLocalFile(dir, base+".meta-", func(out *os.File) error {
		_, err := out.Write([]byte(`{"filename":"new.txt","uploaded_by":"after"}`))
		return err
	})
	if err != nil {
		t.Fatal("stage child metadata:", err)
	}
	backupDir := full + ".publish-backups"
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		t.Fatal("create child backup directory:", err)
	}
	blobBackup, err := backupLocalFile(full, backupDir, "blob-")
	if err != nil {
		t.Fatal("backup child blob:", err)
	}
	metaBackup, err := backupLocalFile(full+".meta.json", backupDir, "meta-")
	if err != nil {
		t.Fatal("backup child metadata:", err)
	}
	journal := localPublishJournal{
		BlobExisted: true, MetaExisted: true,
		BlobBackup: filepath.Base(blobBackup), MetaBackup: filepath.Base(metaBackup),
		BlobTemp: filepath.Base(blobTemp), MetaTemp: filepath.Base(metaTemp),
	}
	if err := persistLocalPublishJournal(full, journal); err != nil {
		t.Fatal("persist child publication journal:", err)
	}
	if err := os.Rename(metaTemp, full+".meta.json"); err != nil {
		t.Fatal("publish child metadata:", err)
	}
	if _, err := os.Stdout.Write([]byte("READY\n")); err != nil {
		t.Fatal("signal parent at crash boundary:", err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(73)
}

func TestLocalBackendRecoversInterruptedDeleteAfterRestart(t *testing.T) {
	root := t.TempDir()
	key := "recover/delete.txt"
	backend := newLocalBackend(Config{LocalPath: root})
	if _, err := backend.Put(context.Background(), key, strings.NewReader("keep me"), 7, FileMeta{Filename: "delete.txt"}); err != nil {
		t.Fatal(err)
	}
	full := backend.fullPath(key)
	backupDir := full + ".publish-backups"
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	blobBackup, err := backupLocalFile(full, backupDir, "blob-")
	if err != nil {
		t.Fatal(err)
	}
	metaBackup, err := backupLocalFile(full+".meta.json", backupDir, "meta-")
	if err != nil {
		t.Fatal(err)
	}
	journalData, err := json.Marshal(localPublishJournal{
		BlobExisted: true, MetaExisted: true,
		BlobBackup: filepath.Base(blobBackup), MetaBackup: filepath.Base(metaBackup),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full+".publish-journal.json", journalData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(full + ".meta.json"); err != nil {
		t.Fatal(err)
	}

	restarted := newLocalBackend(Config{LocalPath: root})
	reader, err := restarted.Open(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep me" {
		t.Fatalf("recovered object = %q", content)
	}
	if _, err := os.Stat(full + ".publish-journal.json"); !os.IsNotExist(err) {
		t.Fatalf("recovered delete journal remains: %v", err)
	}
}
