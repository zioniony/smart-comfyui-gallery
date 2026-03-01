package server

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"smart-comfyui-gallery/config"
	"smart-comfyui-gallery/database"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestServeFileByID_NoCacheAndETag(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmp := t.TempDir()
	config.Cfg = config.Config{
		BaseSmartGalleryPath: tmp,
		BaseOutputPath:       tmp,
		BaseInputPath:        tmp,
	}
	if err := os.MkdirAll(config.GetSQLiteCacheDir(), 0755); err != nil {
		t.Fatalf("mkdir sqlite cache: %v", err)
	}
	if err := database.Init(); err != nil {
		t.Fatalf("database init: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	filePath := filepath.Join(tmp, "a.png")
	content1 := []byte("v1-content")
	if err := os.WriteFile(filePath, content1, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Chtimes(filePath, time.Now(), time.Unix(1700000000, 123456789)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	fi, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	db := database.GetDB()
	if db == nil {
		t.Fatalf("db nil")
	}
	fileID := md5Hex(filePath)
	if _, err := db.Exec(`INSERT INTO files(id, path, mtime, name) VALUES(?,?,?,?)`, fileID, filePath, float64(fi.ModTime().Unix()), filepath.Base(filePath)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	r := gin.New()
	r.GET("/galleryout/file/:file_id", serveFileByID)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/galleryout/file/"+fileID, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control=%q", got)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("missing ETag")
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Fatalf("Content-Disposition=%q", got)
	}
	if _, ok := w.Header()["Last-Modified"]; !ok {
		t.Fatalf("missing Last-Modified")
	}
	if !strings.EqualFold(w.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("Content-Type=%q", w.Header().Get("Content-Type"))
	}
	if got := w.Body.Bytes(); string(got) != string(content1) {
		t.Fatalf("body mismatch got=%q", string(got))
	}

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/galleryout/file/"+fileID, nil)
	req2.Header.Set("If-None-Match", etag)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotModified {
		t.Fatalf("status=%d body=%q", w2.Code, w2.Body.String())
	}

	content2 := []byte("v2-content-changed")
	if err := os.WriteFile(filePath, content2, 0644); err != nil {
		t.Fatalf("write file2: %v", err)
	}
	fi2, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat2: %v", err)
	}
	if _, err := db.Exec(`UPDATE files SET mtime=? WHERE id=?`, float64(fi2.ModTime().Unix()), fileID); err != nil && err != sql.ErrNoRows {
		t.Fatalf("update: %v", err)
	}

	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/galleryout/file/"+fileID, nil)
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w3.Code, w3.Body.String())
	}
	if got := w3.Header().Get("ETag"); got == etag {
		t.Fatalf("etag did not change")
	}
	b3, _ := io.ReadAll(w3.Body)
	if string(b3) != string(content2) {
		t.Fatalf("body mismatch got=%q", string(b3))
	}
}
