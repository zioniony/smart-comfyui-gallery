package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/gin-gonic/gin"

	"smart-comfyui-gallery/config"
	"smart-comfyui-gallery/database"
	"smart-comfyui-gallery/scanner"
	"smart-comfyui-gallery/server"
)

type handleRequest struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	RawQuery   string              `json:"raw_query"`
	Headers    map[string][]string `json:"headers"`
	BodyBase64 string              `json:"body_base64"`
}

type handleResponse struct {
	Status     int                 `json:"status"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Mode       string              `json:"mode"`
	BodyBase64 string              `json:"body_base64,omitempty"`
	FilePath   string              `json:"file_path,omitempty"`
	FileName   string              `json:"file_name,omitempty"`
}

var appOnce sync.Once
var appErr error
var router http.Handler
var pluginRootDir string
var cleanup func() error

func initApp(rootDir, envPath string) error {
	appOnce.Do(func() {
		if rootDir == "" {
			rootDir = "."
		}
		pluginRootDir = rootDir

		r, closeFn, err := server.InitApp(server.AppOptions{
			RootDir:        rootDir,
			EnvFile:        envPath,
			GinMode:        gin.ReleaseMode,
			DisableLogger:  true,
			ScanOnStart:    true,
			ScanAsyncStart: true,
		})
		if err != nil {
			appErr = err
			return
		}
		router = r
		cleanup = closeFn
	})
	return appErr
}

func cStringOrNil(s string) *C.char {
	if s == "" {
		return nil
	}
	return C.CString(s)
}

func buildErrorResponse(status int) *C.char {
	resp := handleResponse{
		Status: status,
		Mode:   "bytes",
	}
	b, _ := json.Marshal(resp)
	return C.CString(string(b))
}

func ensureUnderBase(base, target string) (string, error) {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	baseAbs = filepath.Clean(baseAbs)

	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	targetAbs = filepath.Clean(targetAbs)

	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return targetAbs, nil
	}
	if strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return "", fmt.Errorf("path escapes base")
	}
	return targetAbs, nil
}

func safeJoin(base string, parts ...string) (string, error) {
	p := filepath.Join(append([]string{base}, parts...)...)
	return ensureUnderBase(base, p)
}

func mimeTypeByExt(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == "" {
		return ""
	}
	if ext == ".webp" {
		return "image/webp"
	}
	if mt := mime.TypeByExtension(ext); mt != "" {
		return mt
	}
	return ""
}

func fileEtag(info os.FileInfo) string {
	return fmt.Sprintf("%q", fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size()))
}

func resolveDBFileByID(fileID string) (string, string, string, error) {
	db := database.GetDB()
	if db == nil {
		return "", "", "", fmt.Errorf("db not initialized")
	}
	var p, name, t string
	if err := db.QueryRow("SELECT path, name, type FROM files WHERE id = ?", fileID).Scan(&p, &name, &t); err != nil {
		return "", "", "", err
	}
	return p, name, t, nil
}

func tryBuildFileResponse(req handleRequest) (*handleResponse, bool) {
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}
	if method != http.MethodGet && method != http.MethodHead {
		return nil, false
	}

	p := req.Path
	if p == "" || !strings.HasPrefix(p, "/") {
		p = "/" + p
	}

	if strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/galleryout/static/") {
		rel := strings.TrimPrefix(p, "/static/")
		if strings.HasPrefix(p, "/galleryout/static/") {
			rel = strings.TrimPrefix(p, "/galleryout/static/")
		}
		full, err := safeJoin(filepath.Join(pluginRootDir, "static"), filepath.FromSlash(rel))
		if err != nil {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		st, err := os.Stat(full)
		if err != nil || st.IsDir() {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		headers := map[string][]string{}
		if mt := mimeTypeByExt(full); mt != "" {
			headers["Content-Type"] = []string{mt}
		}
		headers["Cache-Control"] = []string{"public, max-age=3600"}
		headers["ETag"] = []string{fileEtag(st)}
		return &handleResponse{Status: http.StatusOK, Mode: "file", FilePath: full, Headers: headers}, true
	}

	if strings.HasPrefix(p, "/galleryout/serve_zip/") {
		filename := filepath.Base(strings.TrimPrefix(p, "/galleryout/serve_zip/"))
		full, err := safeJoin(config.GetZipCacheDir(), filename)
		if err != nil {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		st, err := os.Stat(full)
		if err != nil || st.IsDir() {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		headers := map[string][]string{
			"Content-Disposition": {fmt.Sprintf("attachment; filename=%q", filename)},
			"ETag":                {fileEtag(st)},
		}
		return &handleResponse{Status: http.StatusOK, Mode: "file", FilePath: full, FileName: filename, Headers: headers}, true
	}

	if strings.HasPrefix(p, "/galleryout/input_file/") {
		rel := strings.TrimPrefix(p, "/galleryout/input_file/")
		full, err := safeJoin(config.Cfg.BaseInputPath, filepath.FromSlash(rel))
		if err != nil {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		st, err := os.Stat(full)
		if err != nil || st.IsDir() {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		headers := map[string][]string{}
		if mt := mimeTypeByExt(full); mt != "" {
			headers["Content-Type"] = []string{mt}
		}
		headers["ETag"] = []string{fileEtag(st)}
		return &handleResponse{Status: http.StatusOK, Mode: "file", FilePath: full, Headers: headers}, true
	}

	if strings.HasPrefix(p, "/galleryout/output_file/") {
		rel := strings.TrimPrefix(p, "/galleryout/output_file/")
		full, err := safeJoin(config.Cfg.BaseOutputPath, filepath.FromSlash(rel))
		if err != nil {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		st, err := os.Stat(full)
		if err != nil || st.IsDir() {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		headers := map[string][]string{}
		if mt := mimeTypeByExt(full); mt != "" {
			headers["Content-Type"] = []string{mt}
		}
		headers["ETag"] = []string{fileEtag(st)}
		return &handleResponse{Status: http.StatusOK, Mode: "file", FilePath: full, Headers: headers}, true
	}

	if strings.HasPrefix(p, "/galleryout/thumbnail/") {
		fileID := strings.TrimPrefix(p, "/galleryout/thumbnail/")
		filePath, _, fileType, err := resolveDBFileByID(fileID)
		if err != nil {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		thumbPath, err := scanner.GenerateThumbnail(filePath, fileType)
		if err != nil {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		st, err := os.Stat(thumbPath)
		if err != nil || st.IsDir() {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		headers := map[string][]string{
			"Content-Type": {"image/webp"},
			"ETag":         {fileEtag(st)},
		}
		return &handleResponse{Status: http.StatusOK, Mode: "file", FilePath: thumbPath, Headers: headers}, true
	}

	if strings.HasPrefix(p, "/galleryout/file/") || strings.HasPrefix(p, "/galleryout/download/") || strings.HasPrefix(p, "/galleryout/stream/") {
		var modePrefix string
		if strings.HasPrefix(p, "/galleryout/file/") {
			modePrefix = "/galleryout/file/"
		} else if strings.HasPrefix(p, "/galleryout/download/") {
			modePrefix = "/galleryout/download/"
		} else {
			modePrefix = "/galleryout/stream/"
		}
		fileID := strings.TrimPrefix(p, modePrefix)
		filePath, name, _, err := resolveDBFileByID(fileID)
		if err != nil {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		st, err := os.Stat(filePath)
		if err != nil || st.IsDir() {
			return &handleResponse{Status: http.StatusNotFound, Mode: "bytes"}, true
		}
		etag := fileEtag(st)
		if inm, ok := req.Headers["If-None-Match"]; ok {
			for _, v := range inm {
				if strings.Contains(v, etag) {
					return &handleResponse{Status: http.StatusNotModified, Mode: "bytes", Headers: map[string][]string{"ETag": {etag}}}, true
				}
			}
		}
		headers := map[string][]string{
			"Cache-Control": {"no-cache"},
			"ETag":          {etag},
		}
		if mt := mimeTypeByExt(filePath); mt != "" {
			headers["Content-Type"] = []string{mt}
		}
		if modePrefix == "/galleryout/download/" {
			headers["Content-Disposition"] = []string{fmt.Sprintf("attachment; filename=%q", name)}
		} else {
			headers["Content-Disposition"] = []string{fmt.Sprintf("inline; filename=%q", filepath.Base(filePath))}
		}
		headers["Last-Modified"] = []string{st.ModTime().UTC().Format(http.TimeFormat)}
		headers["Accept-Ranges"] = []string{"bytes"}
		return &handleResponse{Status: http.StatusOK, Mode: "file", FilePath: filePath, FileName: name, Headers: headers}, true
	}

	return nil, false
}

func handleViaRouter(req handleRequest) (*handleResponse, error) {
	if router == nil {
		return nil, fmt.Errorf("router not initialized")
	}

	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}
	path := req.Path
	if path == "" || !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := "http://127.0.0.1" + path
	if req.RawQuery != "" {
		u += "?" + req.RawQuery
	}

	body := []byte{}
	if req.BodyBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.BodyBase64)
		if err != nil {
			return nil, err
		}
		body = decoded
	}

	httpReq, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range req.Headers {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	if httpReq.Header.Get("Date") == "" {
		httpReq.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httpReq)

	resp := handleResponse{
		Status:  rec.Code,
		Headers: map[string][]string(rec.Header()),
		Mode:    "bytes",
	}
	if method != http.MethodHead {
		resp.BodyBase64 = base64.StdEncoding.EncodeToString(rec.Body.Bytes())
	}
	return &resp, nil
}

//export Init
func Init(rootDir *C.char, envPath *C.char) *C.char {
	rd := C.GoString(rootDir)
	ep := C.GoString(envPath)
	if err := initApp(rd, ep); err != nil {
		return cStringOrNil(err.Error())
	}
	return nil
}

//export HandleRequest
func HandleRequest(reqJSON *C.char) *C.char {
	if router == nil && appErr == nil {
		return buildErrorResponse(http.StatusServiceUnavailable)
	}

	raw := C.GoString(reqJSON)
	var req handleRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return buildErrorResponse(http.StatusBadRequest)
	}

	if r, ok := tryBuildFileResponse(req); ok {
		b, _ := json.Marshal(r)
		return C.CString(string(b))
	}

	resp, err := handleViaRouter(req)
	if err != nil {
		return buildErrorResponse(http.StatusInternalServerError)
	}
	b, _ := json.Marshal(resp)
	return C.CString(string(b))
}

//export FreeCString
func FreeCString(ptr *C.char) {
	C.free(unsafe.Pointer(ptr))
}

func main() {}
