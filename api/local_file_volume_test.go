package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/storage"
	"github.com/gin-gonic/gin"
)

func TestLocalFileVolumeSurvivesEngineRestartAndBackupRestore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engineVolume := t.TempDir()
	originalContent := []byte("application-managed file survives the self-host volume restore\n")

	initialBackend, err := storage.New(storage.Config{Backend: "local", LocalPath: engineVolume})
	if err != nil {
		t.Fatal("create initial local storage backend:", err)
	}
	initialRouter := fileVolumeAPIRouter(initialBackend, "tenant-local")
	fileKey, uploadedChecksum := uploadVolumeFixture(t, initialRouter, originalContent)
	if got := readVolumeFixture(t, initialRouter, fileKey); !bytes.Equal(got, originalContent) {
		t.Fatalf("initial uploaded file = %q, want %q", got, originalContent)
	}

	// Reconstruct both the backend and API composition as a replacement Engine
	// process would after the named volume is reattached.
	restartedBackend, err := storage.New(storage.Config{Backend: "local", LocalPath: engineVolume})
	if err != nil {
		t.Fatal("reopen local storage after process restart:", err)
	}
	restartedRouter := fileVolumeAPIRouter(restartedBackend, "tenant-local")
	if got := readVolumeFixture(t, restartedRouter, fileKey); !bytes.Equal(got, originalContent) {
		t.Fatalf("file after storage/runtime reconstruction = %q, want %q", got, originalContent)
	}
	metadata, err := restartedBackend.Head(t.Context(), fileKey)
	if err != nil {
		t.Fatal("read restored-volume metadata after restart:", err)
	}
	if metadata.Checksum != uploadedChecksum || metadata.Size != int64(len(originalContent)) || metadata.Filename != "volume-proof.txt" {
		t.Fatalf("reopened file metadata = %+v, want checksum %q, size %d, filename volume-proof.txt", metadata, uploadedChecksum, len(originalContent))
	}

	// Match the operator runbook: archive the stopped Engine's complete local
	// storage root, then extract it into a fresh volume before Engine startup.
	backupDir := t.TempDir()
	archive := filepath.Join(backupDir, "engine-sites.tgz")
	command := exec.Command("tar", "-C", engineVolume, "-czf", archive, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("archive application-managed Engine files: %v: %s", err, output)
	}
	restoredVolume := t.TempDir()
	command = exec.Command("tar", "-C", restoredVolume, "-xzf", archive)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restore application-managed Engine files: %v: %s", err, output)
	}

	restoredBackend, err := storage.New(storage.Config{Backend: "local", LocalPath: restoredVolume})
	if err != nil {
		t.Fatal("create backend over restored Engine volume:", err)
	}
	restoredRouter := fileVolumeAPIRouter(restoredBackend, "tenant-local")
	if got := readVolumeFixture(t, restoredRouter, fileKey); !bytes.Equal(got, originalContent) {
		t.Fatalf("restored file through Engine download API = %q, want %q", got, originalContent)
	}
	restoredMetadata, err := restoredBackend.Head(t.Context(), fileKey)
	if err != nil {
		t.Fatal("read restored file metadata:", err)
	}
	if restoredMetadata.Checksum != uploadedChecksum || restoredMetadata.Size != int64(len(originalContent)) {
		t.Fatalf("restored metadata = %+v, want checksum %q and size %d", restoredMetadata, uploadedChecksum, len(originalContent))
	}
}

func fileVolumeAPIRouter(backend storage.Backend, site string) *gin.Engine {
	handler := NewHandler(nil, nil)
	services := NewSiteRuntimeServices()
	services.SetStorage(site, backend)
	handler.RuntimeServices = services
	router := gin.New()
	withSite := func(next gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set("site_name", site)
			next(c)
		}
	}
	router.POST("/api/upload", withSite(handler.HandleUpload))
	router.GET("/api/v1/files/*path", withSite(handler.HandleFileServe))
	return router
}

func uploadVolumeFixture(t *testing.T, router http.Handler, content []byte) (string, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "volume-proof.txt")
	if err != nil {
		t.Fatal("create upload part:", err)
	}
	if _, err := file.Write(content); err != nil {
		t.Fatal("write upload fixture:", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal("close upload form:", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/upload", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	var result struct {
		Data struct {
			Path     string `json:"path"`
			Checksum string `json:"checksum"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal("decode upload result:", err)
	}
	if result.Data.Path == "" || result.Data.Checksum == "" {
		t.Fatalf("upload omitted persisted reference/checksum: %+v", result.Data)
	}
	return result.Data.Path, result.Data.Checksum
}

func readVolumeFixture(t *testing.T, router http.Handler, key string) []byte {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/files/"+key, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("file download status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("read file download:", err)
	}
	return content
}
