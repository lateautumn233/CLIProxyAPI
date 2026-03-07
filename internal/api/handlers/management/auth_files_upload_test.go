package management

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type countingFileTokenStore struct {
	mu        sync.Mutex
	saveCount int
	inner     *sdkAuth.FileTokenStore
}

func newCountingFileTokenStore(baseDir string) *countingFileTokenStore {
	store := &countingFileTokenStore{
		inner: sdkAuth.NewFileTokenStore(),
	}
	store.SetBaseDir(baseDir)
	return store
}

func (s *countingFileTokenStore) SetBaseDir(dir string) {
	s.inner.SetBaseDir(dir)
}

func (s *countingFileTokenStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	s.mu.Lock()
	s.saveCount++
	s.mu.Unlock()
	return s.inner.Save(ctx, auth)
}

func (s *countingFileTokenStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	return s.inner.List(ctx)
}

func (s *countingFileTokenStore) Delete(ctx context.Context, id string) error {
	return s.inner.Delete(ctx, id)
}

func (s *countingFileTokenStore) SaveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCount
}

func newMultipartAuthRequest(t *testing.T, files map[string]string) (*bytes.Buffer, string) {
	t.Helper()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for name, content := range files {
		fileWriter, errCreate := writer.CreateFormFile("files", name)
		if errCreate != nil {
			t.Fatalf("create form file %s: %v", name, errCreate)
		}
		if _, errWrite := fileWriter.Write([]byte(content)); errWrite != nil {
			t.Fatalf("write multipart payload for %s: %v", name, errWrite)
		}
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}
	return body, writer.FormDataContentType()
}

func TestUploadAuthFile_PersistsOnlyOnce(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	store := newCountingFileTokenStore(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	handler.tokenStore = store

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileWriter, errCreate := writer.CreateFormFile("file", "codex-user.json")
	if errCreate != nil {
		t.Fatalf("create form file: %v", errCreate)
	}
	if _, errWrite := fileWriter.Write([]byte(`{"type":"codex","email":"user@example.com"}`)); errWrite != nil {
		t.Fatalf("write multipart payload: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	handler.UploadAuthFile(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusOK, recorder.Code, recorder.Body.String())
	}
	if got := store.SaveCount(); got != 1 {
		t.Fatalf("expected exactly 1 store save, got %d", got)
	}

	savedPath := filepath.Join(authDir, "codex-user.json")
	if _, errStat := os.Stat(savedPath); errStat != nil {
		t.Fatalf("expected saved auth file at %s: %v", savedPath, errStat)
	}

	auths := manager.List()
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth registered in manager, got %d", len(auths))
	}
	if auths[0].FileName != "codex-user.json" {
		t.Fatalf("expected uploaded file name to be preserved, got %q", auths[0].FileName)
	}
	if auths[0].Provider != "codex" {
		t.Fatalf("expected provider codex, got %q", auths[0].Provider)
	}
}

func TestUploadAuthFile_BatchUploadReturnsSummary(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	store := newCountingFileTokenStore(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	handler.tokenStore = store

	body, contentType := newMultipartAuthRequest(t, map[string]string{
		"codex-a.json":  `{"type":"codex","email":"a@example.com"}`,
		"claude-b.json": `{"type":"claude","email":"b@example.com"}`,
		"broken.json":   `{"type":"codex"`,
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", body)
	req.Header.Set("Content-Type", contentType)
	ctx.Request = req

	handler.UploadAuthFile(ctx)

	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("expected batch upload status %d, got %d with body %s", http.StatusMultiStatus, recorder.Code, recorder.Body.String())
	}

	var response authUploadResponse
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("decode batch upload response: %v", errUnmarshal)
	}
	if response.Status != "partial" {
		t.Fatalf("expected partial status, got %q", response.Status)
	}
	if response.Total != 3 {
		t.Fatalf("expected total 3, got %d", response.Total)
	}
	if len(response.Uploaded) != 2 {
		t.Fatalf("expected 2 uploaded files, got %d", len(response.Uploaded))
	}
	if len(response.Failed) != 1 {
		t.Fatalf("expected 1 failed file, got %d", len(response.Failed))
	}
	if response.Failed[0].Name != "broken.json" {
		t.Fatalf("expected failed file broken.json, got %q", response.Failed[0].Name)
	}
	if response.Failed[0].Status != http.StatusBadRequest {
		t.Fatalf("expected failed status %d, got %d", http.StatusBadRequest, response.Failed[0].Status)
	}
	if got := store.SaveCount(); got != 2 {
		t.Fatalf("expected exactly 2 store saves for successful files, got %d", got)
	}

	auths := manager.List()
	if len(auths) != 2 {
		t.Fatalf("expected 2 auths registered in manager, got %d", len(auths))
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "codex-a.json")); errStat != nil {
		t.Fatalf("expected codex-a.json to be saved: %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "claude-b.json")); errStat != nil {
		t.Fatalf("expected claude-b.json to be saved: %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "broken.json")); !os.IsNotExist(errStat) {
		t.Fatalf("expected broken.json not to be saved, stat err: %v", errStat)
	}
}
