package server

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"smart-comfyui-gallery/config"
	"smart-comfyui-gallery/database"
	"smart-comfyui-gallery/scanner"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

type zipJob struct {
	Status     string
	Filename   string
	Error      string
	CreatedAt  time.Time
	FinishedAt time.Time
}

type rescanJob struct {
	Status     string
	Current    int
	Total      int
	FolderKey  string
	FolderName string
	Error      string
	CreatedAt  time.Time
	FinishedAt time.Time
}

var zipJobsMu sync.Mutex
var zipJobs = map[string]*zipJob{}

var rescanJobsMu sync.Mutex
var rescanJobs = map[string]*rescanJob{}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func uuidV4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return md5Hex(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func getDBOr500(c *gin.Context) *sql.DB {
	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Database not initialized"})
		return nil
	}
	return db
}

func ensureUnderBase(base, p string) (string, error) {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return targetAbs, nil
	}
	if strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return "", errors.New("path escapes base")
	}
	return targetAbs, nil
}

func safeJoin(base string, parts ...string) (string, error) {
	p := filepath.Join(append([]string{base}, parts...)...)
	return ensureUnderBase(base, p)
}

func pathToFolderKey(relPath string) string {
	relPath = filepath.ToSlash(relPath)
	relPath = strings.TrimPrefix(relPath, "/")
	if relPath == "" || relPath == "." {
		return "_root_"
	}
	return base64.URLEncoding.EncodeToString([]byte(relPath))
}

func tryDecodeFolderKey(folderKey string) (string, bool) {
	if folderKey == "" || folderKey == "_root_" {
		return "", true
	}

	s := strings.TrimSpace(folderKey)
	if s == "" {
		return "", true
	}

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return "", false
	}
	if !utf8.Valid(raw) {
		return "", false
	}

	decoded := string(raw)
	decoded = strings.TrimPrefix(decoded, "/")
	decoded = filepath.Clean(filepath.FromSlash(decoded))
	decoded = strings.TrimPrefix(decoded, string(filepath.Separator))
	if decoded == "." || decoded == "" {
		return "", true
	}
	if decoded == ".." || strings.HasPrefix(decoded, ".."+string(filepath.Separator)) {
		return "", false
	}
	return decoded, true
}

func resolveFolderKeyToPath(folderKey string) (string, error) {
	if folderKey == "" || folderKey == "_root_" {
		return filepath.Abs(config.Cfg.BaseOutputPath)
	}

	if rel, ok := tryDecodeFolderKey(folderKey); ok {
		if rel == "" {
			return filepath.Abs(config.Cfg.BaseOutputPath)
		}
		return safeJoin(config.Cfg.BaseOutputPath, rel)
	}

	clean := filepath.Clean(strings.ReplaceAll(folderKey, "\\", string(os.PathSeparator)))
	clean = strings.TrimPrefix(clean, string(os.PathSeparator))
	return safeJoin(config.Cfg.BaseOutputPath, clean)
}

func lookupFileByID(db *sql.DB, fileID string) (map[string]interface{}, error) {
	row := db.QueryRow(`SELECT id, path, mtime, name, type, duration, dimensions, has_workflow, is_favorite, size, last_scanned FROM files WHERE id = ?`, fileID)
	var id, path, name, fileType, duration, dimensions sql.NullString
	var mtime, lastScanned sql.NullFloat64
	var hasWorkflow, isFavorite sql.NullInt64
	var size sql.NullInt64
	if err := row.Scan(&id, &path, &mtime, &name, &fileType, &duration, &dimensions, &hasWorkflow, &isFavorite, &size, &lastScanned); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"id":           id.String,
		"path":         path.String,
		"mtime":        mtime.Float64,
		"name":         name.String,
		"type":         fileType.String,
		"duration":     duration.String,
		"dimensions":   dimensions.String,
		"has_workflow": hasWorkflow.Int64 == 1,
		"is_favorite":  isFavorite.Int64 == 1,
		"size":         size.Int64,
		"last_scanned": lastScanned.Float64,
	}, nil
}

func setContentTypeByExt(c *gin.Context, filePath string) {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == "" {
		return
	}
	if ext == ".webp" {
		c.Header("Content-Type", "image/webp")
		return
	}
	if mt := mime.TypeByExtension(ext); mt != "" {
		c.Header("Content-Type", mt)
	}
}

func uniqueCleanPrefixes(prefixes ...string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		p = filepath.Clean(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func listPathsUnderPrefixes(db *sql.DB, prefixes []string) ([]string, error) {
	if db == nil {
		return nil, fmt.Errorf("db nil")
	}
	out := make([]string, 0, 256)
	for _, prefix := range prefixes {
		like := prefix + string(os.PathSeparator) + "%"
		rows, err := db.Query("SELECT path FROM files WHERE path LIKE ?", like)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err == nil && strings.TrimSpace(p) != "" {
				out = append(out, p)
			}
		}
		_ = rows.Close()
	}
	return out, nil
}

func computePrunePaths(dbPaths []string, visited map[string]struct{}) []string {
	toDelete := make([]string, 0, 64)
	seenDelete := map[string]struct{}{}
	for _, p := range dbPaths {
		pClean := filepath.Clean(p)
		if pClean == "" {
			continue
		}
		norm, err := scanner.NormalizePath(pClean)
		if err != nil {
			norm = pClean
		}
		_, ok := visited[norm]
		if !ok || pClean != norm {
			if _, exists := seenDelete[pClean]; !exists {
				seenDelete[pClean] = struct{}{}
				toDelete = append(toDelete, pClean)
			}
		}
	}
	sort.Strings(toDelete)
	return toDelete
}

func syncStatus(c *gin.Context) {
	folderKey := c.Param("folder_key")
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	type progress struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Current int    `json:"current"`
		Total   int    `json:"total"`
	}

	path, err := resolveFolderKeyToPath(folderKey)
	if err != nil {
		c.Writer.WriteString("data: {\"status\":\"error\",\"message\":\"Invalid folder\",\"current\":0,\"total\":0}\n\n")
		c.Writer.Flush()
		return
	}
	path = filepath.Clean(path)
	pathNorm, err := scanner.NormalizePath(path)
	if err != nil {
		pathNorm = path
	}
	prefixes := uniqueCleanPrefixes(path, pathNorm)
	if wd, err := os.Getwd(); err == nil && wd != "" {
		if rel, err := filepath.Rel(wd, pathNorm); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			prefixes = uniqueCleanPrefixes(append(prefixes, rel)...)
		}
	}

	db := getDBOr500(c)
	if db == nil {
		return
	}

	stmt, err := db.Prepare("SELECT mtime FROM files WHERE path = ? LIMIT 1")
	if err != nil {
		c.Writer.WriteString("data: {\"status\":\"error\",\"message\":\"DB error\",\"current\":0,\"total\":0}\n\n")
		c.Writer.Flush()
		return
	}
	defer stmt.Close()

	changed := []string{}
	visited := map[string]struct{}{}
	filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		name := d.Name()
		if name != "" && name[0] == '.' {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			switch name {
			case ".thumbnails_cache", ".sqlite_cache", ".zip_downloads":
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		fsMtime := float64(fi.ModTime().Unix())
		pNorm, err := scanner.NormalizePath(p)
		if err != nil {
			pNorm = filepath.Clean(p)
		}
		visited[pNorm] = struct{}{}

		var dbMtime sql.NullFloat64
		rowErr := stmt.QueryRow(pNorm).Scan(&dbMtime)
		if rowErr != nil || !dbMtime.Valid || int64(dbMtime.Float64) != int64(fsMtime) {
			changed = append(changed, pNorm)
		}
		return nil
	})

	dbPaths, err := listPathsUnderPrefixes(db, prefixes)
	if err != nil {
		c.Writer.WriteString("data: {\"status\":\"error\",\"message\":\"DB error\",\"current\":0,\"total\":0}\n\n")
		c.Writer.Flush()
		return
	}
	toDelete := computePrunePaths(dbPaths, visited)
	if len(toDelete) > 0 {
		delStmt, err := db.Prepare("DELETE FROM files WHERE path = ?")
		if err == nil {
			for _, dp := range toDelete {
				_, _ = delStmt.Exec(dp)
			}
			_ = delStmt.Close()
		}
	}

	if len(changed) == 0 && len(toDelete) == 0 {
		payload, _ := json.Marshal(progress{Status: "no_changes", Message: "No changes detected.", Current: 0, Total: 0})
		c.Writer.WriteString("data: " + string(payload) + "\n\n")
		c.Writer.Flush()
		return
	}
	if len(changed) == 0 && len(toDelete) > 0 {
		payload, _ := json.Marshal(progress{Status: "processing", Message: fmt.Sprintf("Pruned %d stale records.", len(toDelete)), Current: 1, Total: 1})
		c.Writer.WriteString("data: " + string(payload) + "\n\n")
		c.Writer.Flush()
		payload, _ = json.Marshal(progress{Status: "reloading", Message: "Reloading...", Current: 1, Total: 1})
		c.Writer.WriteString("data: " + string(payload) + "\n\n")
		c.Writer.Flush()
		return
	}

	total := len(changed)
	for i, fp := range changed {
		_ = scanner.UpsertFile(fp, db)
		payload, _ := json.Marshal(progress{
			Status:  "processing",
			Message: fmt.Sprintf("Scanning %d/%d", i+1, total),
			Current: i + 1,
			Total:   total,
		})
		c.Writer.WriteString("data: " + string(payload) + "\n\n")
		c.Writer.Flush()
	}

	payload, _ := json.Marshal(progress{Status: "reloading", Message: "Reloading...", Current: total, Total: total})
	c.Writer.WriteString("data: " + string(payload) + "\n\n")
	c.Writer.Flush()
}

func serveFileByID(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	filePath := info["path"].(string)
	f, err := os.Open(filePath)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	setContentTypeByExt(c, filePath)
	c.Header("Cache-Control", "no-cache")
	etag := fmt.Sprintf("%q", fmt.Sprintf("%d-%d", fi.ModTime().UnixNano(), fi.Size()))
	c.Header("ETag", etag)
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=%q", filepath.Base(filePath)))
	if inm := c.GetHeader("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		c.Status(http.StatusNotModified)
		return
	}
	http.ServeContent(c.Writer, c.Request, filepath.Base(filePath), fi.ModTime(), f)
}

func downloadFileByID(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	filePath := info["path"].(string)
	name := info["name"].(string)
	c.FileAttachment(filePath, name)
}

func downloadWorkflowByID(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	filePath := info["path"].(string)
	originalName := info["name"].(string)
	workflow := scanner.ExtractWorkflow(filePath, "ui")
	if workflow == "" {
		c.Status(http.StatusNotFound)
		return
	}
	base := strings.TrimSuffix(originalName, filepath.Ext(originalName))
	newFilename := base + ".json"
	c.Header("Content-Disposition", fmt.Sprintf("attachment;filename=%q", newFilename))
	c.Data(http.StatusOK, "application/json", []byte(workflow))
}

func nodeSummaryByID(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		b, _ := json.Marshal(struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}{Status: "error", Message: "File not found"})
		c.Data(http.StatusNotFound, "application/json", b)
		return
	}
	filePath := info["path"].(string)
	uiJSON := scanner.ExtractWorkflow(filePath, "ui")
	if uiJSON == "" {
		b, _ := json.Marshal(struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}{Status: "error", Message: "Workflow not found for this file."})
		c.Data(http.StatusNotFound, "application/json", b)
		return
	}
	summary := generateNodeSummary(uiJSON)
	apiJSON := scanner.ExtractWorkflow(filePath, "api")
	meta := map[string]interface{}{}
	if apiJSON == "" {
		apiJSON = uiJSON
	}
	if apiJSON != "" {
		parsed := parseComfyMetadata(apiJSON)
		if isMetaSolid(parsed) {
			if w, ok := parsed["width"]; (!ok || parsed["width"] == nil) && info["dimensions"] != "" {
				_ = w
				if dims, ok2 := info["dimensions"].(string); ok2 && strings.Contains(dims, "x") {
					parts := strings.SplitN(dims, "x", 2)
					if len(parts) == 2 {
						parsed["width"] = strings.TrimSpace(parts[0])
						parsed["height"] = strings.TrimSpace(parts[1])
					}
				}
			}
			meta = parsed
		}
	}
	b, _ := json.Marshal(struct {
		Meta    map[string]interface{} `json:"meta"`
		Status  string                 `json:"status"`
		Summary []nodeSummaryItem      `json:"summary"`
	}{Meta: meta, Status: "success", Summary: summary})
	c.Data(http.StatusOK, "application/json", b)
}

func checkMetadataByID(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "File not found"})
		return
	}
	hasWorkflow := false
	if v, ok := info["has_workflow"].(bool); ok {
		hasWorkflow = v
	}
	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"has_workflow": hasWorkflow,
		"real_path":    info["path"],
	})
}

func findExecutable(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil && p != "" {
			return p
		}
	}
	return ""
}

func streamVideoByID(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	filePath := info["path"].(string)
	ffmpeg := findExecutable("ffmpeg", "ffmpeg.exe")
	if ffmpeg == "" {
		c.JSON(http.StatusNotImplemented, gin.H{"status": "error", "message": "FFmpeg not available"})
		return
	}
	cmd := exec.Command(ffmpeg,
		"-i", filePath,
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov",
		"-vcodec", "libx264",
		"-acodec", "aac",
		"-preset", "veryfast",
		"-crf", "23",
		"-g", "30",
		"-pix_fmt", "yuv420p",
		"pipe:1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer cmd.Process.Kill()
	go io.Copy(io.Discard, stderr)

	c.Header("Content-Type", "video/mp4")
	c.Status(http.StatusOK)
	buf := make([]byte, 16384)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			_, _ = c.Writer.Write(buf[:n])
			c.Writer.Flush()
		}
		if rerr != nil {
			break
		}
	}
	_ = cmd.Wait()
}

func getStoryboardByID(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "File not found"})
		return
	}
	fileType := fmt.Sprintf("%v", info["type"])
	if fileType != "video" && fileType != "animated_image" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Not a video or animated file"})
		return
	}

	ffprobe := findExecutable("ffprobe", "ffprobe.exe")
	ffmpeg := findExecutable("ffmpeg", "ffmpeg.exe")
	if fileType == "video" && (ffprobe == "" || ffmpeg == "") {
		c.JSON(http.StatusNotImplemented, gin.H{"status": "error", "message": "FFmpeg not available"})
		return
	}

	filePath := info["path"].(string)
	mtime := fmt.Sprintf("%.0f", info["mtime"])
	fileHash := md5Hex(filePath + mtime)
	cacheDir := filepath.Join(config.GetThumbnailCacheDir(), fileHash)
	_ = os.MkdirAll(cacheDir, 0755)

	existing, _ := filepath.Glob(filepath.Join(cacheDir, "frame_*.jpg"))
	if len(existing) > 0 {
		urls := []string{}
		sortStrings(existing)
		for _, f := range existing {
			urls = append(urls, "/galleryout/storyboard_frame/"+fileHash+"/"+filepath.Base(f))
		}
		c.JSON(http.StatusOK, gin.H{"status": "success", "cached": true, "frames": urls})
		return
	}

	duration := 60.0
	if fileType == "video" && ffprobe != "" {
		out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", filePath).Output()
		if err == nil {
			if d, derr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); derr == nil && d > 0 {
				duration = d
			}
		}
	}

	frameCount := 11
	timestamps := []float64{}
	for i := 0; i < frameCount; i++ {
		t := (float64(i) + 0.5) / float64(frameCount) * duration
		timestamps = append(timestamps, t)
	}

	for i, t := range timestamps {
		outPath := filepath.Join(cacheDir, fmt.Sprintf("frame_%02d.jpg", i))
		if fileType == "video" {
			_ = exec.Command(ffmpeg, "-y", "-ss", fmt.Sprintf("%.3f", t), "-i", filePath, "-frames:v", "1", "-vf", "scale=-2:360:flags=fast_bilinear", "-q:v", "5", outPath).Run()
		} else {
			_ = exec.Command(ffmpeg, "-y", "-ss", fmt.Sprintf("%.3f", t), "-i", filePath, "-frames:v", "1", "-vf", "scale=-2:360:flags=fast_bilinear", "-q:v", "5", outPath).Run()
		}
	}

	frames, _ := filepath.Glob(filepath.Join(cacheDir, "frame_*.jpg"))
	sortStrings(frames)
	urls := []string{}
	for _, f := range frames {
		urls = append(urls, "/galleryout/storyboard_frame/"+fileHash+"/"+filepath.Base(f))
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "cached": false, "frames": urls})
}

func serveStoryboardFrame(c *gin.Context) {
	fileHash := c.Param("file_hash")
	filename := filepath.Base(c.Param("filename"))
	if fileHash == "" || filename == "" {
		c.Status(http.StatusNotFound)
		return
	}
	cacheDir := filepath.Join(config.GetThumbnailCacheDir(), fileHash)
	full := filepath.Join(cacheDir, filename)
	abs, err := ensureUnderBase(cacheDir, full)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.File(abs)
}

func uploadFiles(c *gin.Context) {
	folderKey := c.PostForm("folder_key")
	destDir, err := resolveFolderKeyToPath(folderKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid folder_key"})
		return
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Failed to create destination folder"})
		return
	}

	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid multipart form"})
		return
	}
	files := form.File["files"]
	if len(files) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "No files uploaded"})
		return
	}

	db := getDBOr500(c)
	if db == nil {
		return
	}

	okCount := 0
	failCount := 0
	for _, fh := range files {
		dst := filepath.Join(destDir, filepath.Base(fh.Filename))
		if err := c.SaveUploadedFile(fh, dst); err != nil {
			failCount++
			continue
		}
		_ = scanner.UpsertFile(dst, db)
		okCount++
	}

	if okCount > 0 && failCount == 0 {
		c.JSON(http.StatusOK, gin.H{"status": "success", "message": "Files uploaded."})
		return
	}
	if okCount > 0 && failCount > 0 {
		c.JSON(http.StatusMultiStatus, gin.H{"status": "partial_success", "message": "Some files failed to upload."})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Upload failed"})
}

type listParams struct {
	FolderKey      string
	Recursive      bool
	Scope          string
	Search         string
	WorkflowFiles  string
	WorkflowPrompt string
	StartDate      string
	EndDate        string
	Extensions     []string
	Prefixes       []string
	Favorites      bool
	NoWorkflow     bool
	SortBy         string
	SortOrder      string
	Offset         int
	Limit          int
}

func parseListParams(c *gin.Context) listParams {
	p := listParams{
		FolderKey:      c.Query("folder_key"),
		Recursive:      c.Query("recursive") == "true",
		Scope:          c.Query("scope"),
		Search:         c.Query("search"),
		WorkflowFiles:  c.Query("workflow_files"),
		WorkflowPrompt: c.Query("workflow_prompt"),
		StartDate:      c.Query("start_date"),
		EndDate:        c.Query("end_date"),
		Extensions:     c.QueryArray("extension"),
		Prefixes:       c.QueryArray("prefix"),
		Favorites:      c.Query("favorites") == "true",
		NoWorkflow:     c.Query("no_workflow") == "true",
		SortBy:         c.Query("sort_by"),
		SortOrder:      c.Query("sort_order"),
		Limit:          config.Cfg.PageSize,
	}
	if p.SortBy == "" {
		p.SortBy = "date"
	}
	if p.SortOrder == "" {
		p.SortOrder = "desc"
	}
	if off, err := strconv.Atoi(c.Query("offset")); err == nil && off >= 0 {
		p.Offset = off
	}
	if lim, err := strconv.Atoi(c.Query("limit")); err == nil && lim > 0 && lim <= 10000 {
		p.Limit = lim
	}
	return p
}

func buildFilesWhere(p listParams) (string, []interface{}, error) {
	where := " WHERE 1=1"
	args := []interface{}{}

	if p.Favorites {
		where += " AND is_favorite = 1"
	}
	if p.NoWorkflow {
		where += " AND has_workflow = 0"
	}
	if strings.TrimSpace(p.Search) != "" {
		where += " AND name LIKE ?"
		args = append(args, "%"+p.Search+"%")
	}
	if strings.TrimSpace(p.WorkflowFiles) != "" {
		parts := strings.Split(p.WorkflowFiles, ",")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			where += " AND workflow_files LIKE ?"
			args = append(args, "%"+part+"%")
		}
	}
	if strings.TrimSpace(p.WorkflowPrompt) != "" {
		parts := strings.Split(p.WorkflowPrompt, ",")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			where += " AND workflow_prompt LIKE ?"
			args = append(args, "%"+part+"%")
		}
	}
	if p.StartDate != "" {
		if ts, err := parseDateToUnix(p.StartDate); err == nil {
			where += " AND mtime >= ?"
			args = append(args, float64(ts))
		}
	}
	if p.EndDate != "" {
		if ts, err := parseDateToUnixEnd(p.EndDate); err == nil {
			where += " AND mtime <= ?"
			args = append(args, float64(ts))
		}
	}
	if len(p.Extensions) > 0 {
		where += " AND ("
		for i, ext := range p.Extensions {
			if i > 0 {
				where += " OR"
			}
			where += " LOWER(name) LIKE ?"
			args = append(args, "%."+strings.ToLower(ext))
		}
		where += ")"
	}
	if len(p.Prefixes) > 0 {
		where += " AND ("
		for i, pfx := range p.Prefixes {
			if i > 0 {
				where += " OR"
			}
			where += " name LIKE ?"
			args = append(args, pfx+"_%")
		}
		where += ")"
	}
	if p.Scope != "global" {
		folderPath, err := resolveFolderKeyToPath(p.FolderKey)
		if err != nil {
			return "", nil, err
		}
		folderPrefix := filepath.Clean(folderPath) + string(os.PathSeparator)
		where += " AND path LIKE ?"
		args = append(args, folderPrefix+"%")
		if !p.Recursive {
			where += " AND instr(substr(path, length(?)+1), ?) = 0"
			args = append(args, folderPrefix, string(os.PathSeparator))
		}
	}
	return where, args, nil
}

func buildFilesQuery(db *sql.DB, p listParams) (string, []interface{}, error) {
	where, args, err := buildFilesWhere(p)
	if err != nil {
		return "", nil, err
	}

	q := `
		SELECT id, path, mtime, name, type, duration, dimensions,
		       has_workflow, is_favorite, size, last_scanned
		FROM files
	` + where

	orderCol := "mtime"
	if p.SortBy == "name" {
		orderCol = "name"
	}
	orderDir := "DESC"
	if strings.ToLower(p.SortOrder) == "asc" {
		orderDir = "ASC"
	}
	q += " ORDER BY " + orderCol + " " + orderDir

	q += " LIMIT ? OFFSET ?"
	args = append(args, p.Limit, p.Offset)
	_ = db
	return q, args, nil
}

func parseDateToUnix(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

func parseDateToUnixEnd(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, err
	}
	return t.Add(23*time.Hour + 59*time.Minute + 59*time.Second).Unix(), nil
}

func loadMoreFiles(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	p := parseListParams(c)
	q, args, err := buildFilesQuery(db, p)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": err.Error()})
		return
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Failed to query files"})
		return
	}
	defer rows.Close()
	files := []map[string]interface{}{}
	for rows.Next() {
		var id, path, name, fileType, duration, dimensions string
		var mtime, lastScanned float64
		var hasWorkflow, isFavorite int
		var size int64
		if err := rows.Scan(&id, &path, &mtime, &name, &fileType, &duration, &dimensions, &hasWorkflow, &isFavorite, &size, &lastScanned); err != nil {
			continue
		}
		files = append(files, map[string]interface{}{
			"id":           id,
			"path":         path,
			"mtime":        mtime,
			"name":         name,
			"type":         fileType,
			"duration":     duration,
			"dimensions":   dimensions,
			"has_workflow": hasWorkflow == 1,
			"is_favorite":  isFavorite == 1,
			"size":         size,
			"last_scanned": lastScanned,
		})
	}
	c.JSON(http.StatusOK, gin.H{"files": files})
}

func prepareBatchZip(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	var req struct {
		FileIDs []string `json:"file_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.FileIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	jobID := md5Hex(fmt.Sprintf("%d-%d", time.Now().UnixNano(), len(req.FileIDs)))
	filename := jobID + ".zip"

	zipJobsMu.Lock()
	zipJobs[jobID] = &zipJob{Status: "processing", Filename: filename, CreatedAt: time.Now()}
	zipJobsMu.Unlock()

	go func() {
		zipDir := config.GetZipCacheDir()
		_ = os.MkdirAll(zipDir, 0755)
		zipPath := filepath.Join(zipDir, filename)
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, id := range req.FileIDs {
			info, err := lookupFileByID(db, id)
			if err != nil {
				continue
			}
			fp := info["path"].(string)
			name := filepath.Base(fp)
			fw, err := zw.Create(name)
			if err != nil {
				continue
			}
			f, err := os.Open(fp)
			if err != nil {
				continue
			}
			_, _ = io.Copy(fw, f)
			_ = f.Close()
		}
		_ = zw.Close()
		if err := os.WriteFile(zipPath, buf.Bytes(), 0644); err != nil {
			zipJobsMu.Lock()
			zipJobs[jobID].Status = "error"
			zipJobs[jobID].Error = err.Error()
			zipJobs[jobID].FinishedAt = time.Now()
			zipJobsMu.Unlock()
			return
		}
		zipJobsMu.Lock()
		zipJobs[jobID].Status = "ready"
		zipJobs[jobID].FinishedAt = time.Now()
		zipJobsMu.Unlock()
	}()

	c.JSON(http.StatusOK, gin.H{"status": "success", "job_id": jobID, "message": "ZIP job started."})
}

func checkZipStatus(c *gin.Context) {
	jobID := c.Param("job_id")
	zipJobsMu.Lock()
	job, ok := zipJobs[jobID]
	zipJobsMu.Unlock()
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"status": "not_found"})
		return
	}
	if job.Status == "ready" {
		c.JSON(http.StatusOK, gin.H{"status": "ready", "filename": job.Filename, "download_url": "/galleryout/serve_zip/" + job.Filename})
		return
	}
	if job.Status == "error" {
		c.JSON(http.StatusOK, gin.H{"status": "error", "message": job.Error})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "processing"})
}

func serveZipFile(c *gin.Context) {
	filename := filepath.Base(c.Param("filename"))
	zipPath := filepath.Join(config.GetZipCacheDir(), filename)
	abs, err := ensureUnderBase(config.GetZipCacheDir(), zipPath)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.FileAttachment(abs, filename)
}

func createFolderLegacy(c *gin.Context) {
	var req struct {
		ParentKey  string `json:"parent_key"`
		FolderName string `json:"folder_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.FolderName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	parentPath, err := resolveFolderKeyToPath(req.ParentKey)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "Folder not found"})
		return
	}
	newPath, err := safeJoin(parentPath, filepath.Base(req.FolderName))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid folder name"})
		return
	}
	if err := os.MkdirAll(newPath, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Failed to create folder"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "Folder created."})
}

func protectedFolderKeys() map[string]bool {
	return map[string]bool{
		"_root_": true,
	}
}

func renameFolderLegacy(c *gin.Context) {
	folderKey := c.Param("folder_key")
	if protectedFolderKeys()[folderKey] {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Protected folder"})
		return
	}
	var req struct {
		NewName string `json:"new_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.NewName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	oldPath, err := resolveFolderKeyToPath(folderKey)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "Folder not found"})
		return
	}
	parent := filepath.Dir(oldPath)
	newPath, err := safeJoin(parent, filepath.Base(req.NewName))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid name"})
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Failed to rename folder"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "Folder renamed."})
}

func deleteFolderLegacy(c *gin.Context) {
	folderKey := c.Param("folder_key")
	if protectedFolderKeys()[folderKey] {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Protected folder"})
		return
	}
	path, err := resolveFolderKeyToPath(folderKey)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "Folder not found"})
		return
	}
	if err := os.RemoveAll(path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Failed to delete folder"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "Folder deleted."})
}

func mountFolder(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	var req struct {
		LinkName   string `json:"link_name"`
		TargetPath string `json:"target_path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.LinkName) == "" || strings.TrimSpace(req.TargetPath) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	linkName := filepath.Base(req.LinkName)
	linkPath, err := safeJoin(config.Cfg.BaseOutputPath, linkName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid link_name"})
		return
	}
	target := req.TargetPath
	if runtime.GOOS == "windows" {
		c.JSON(http.StatusNotImplemented, gin.H{"status": "error", "message": "Windows mount not implemented"})
		return
	}
	if err := os.Symlink(target, linkPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Failed to create symlink"})
		return
	}
	_, _ = db.Exec("INSERT OR REPLACE INTO mounted_folders(path, target_source, created_at) VALUES(?,?,?)", linkName, target, float64(time.Now().Unix()))
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "Folder mounted."})
}

func unmountFolder(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	var req struct {
		FolderKey string `json:"folder_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.FolderKey) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	linkPath, err := resolveFolderKeyToPath(req.FolderKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid folder_key"})
		return
	}
	fi, err := os.Lstat(linkPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "Folder not found"})
		return
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Not a mount folder"})
		return
	}
	_ = os.Remove(linkPath)
	linkName := filepath.Base(linkPath)
	_, _ = db.Exec("DELETE FROM mounted_folders WHERE path = ?", linkName)
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "Folder unmounted."})
}

func browseFilesystem(c *gin.Context) {
	var req struct {
		Path string `json:"path"`
	}
	_ = c.ShouldBindJSON(&req)
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = "/"
	}
	if runtime.GOOS == "windows" {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "Windows browse not implemented"})
		return
	}
	if path == "Computer" {
		path = "/"
	}
	path = filepath.Clean(path)
	entries, err := os.ReadDir(path)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"current_path": path, "parent_path": filepath.Dir(path), "folders": []gin.H{}, "error": err.Error()})
		return
	}
	folders := []gin.H{}
	for _, e := range entries {
		if e.IsDir() {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			folders = append(folders, gin.H{"name": name, "path": filepath.Join(path, name)})
		}
	}
	c.JSON(http.StatusOK, gin.H{"current_path": path, "parent_path": filepath.Dir(path), "folders": folders, "error": ""})
}

func compareFiles(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	var req struct {
		IDA string `json:"id_a"`
		IDB string `json:"id_b"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.IDA == "" || req.IDB == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Missing file IDs"})
		return
	}

	stringify := func(v any) string {
		if v == nil {
			return "None"
		}
		if b, ok := v.(bool); ok {
			if b {
				return "True"
			}
			return "False"
		}
		if f, ok := v.(float64); ok {
			if !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f && f <= float64(math.MaxInt64) && f >= float64(math.MinInt64) {
				return strconv.FormatInt(int64(f), 10)
			}
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return fmt.Sprint(v)
	}

	getFlatParams := func(fileID string) map[string]string {
		info, err := lookupFileByID(db, fileID)
		if err != nil {
			return map[string]string{}
		}
		path, ok := info["path"].(string)
		if !ok || strings.TrimSpace(path) == "" {
			return map[string]string{}
		}
		wfJSON := scanner.ExtractWorkflow(path, "ui")
		if wfJSON == "" {
			return map[string]string{}
		}
		summary := generateNodeSummary(wfJSON)
		if len(summary) == 0 {
			return map[string]string{}
		}
		flat := map[string]string{}
		for _, node := range summary {
			for _, p := range node.Params {
				key := node.Type + " > " + p.Name
				flat[key] = stringify(p.Value)
			}
		}
		return flat
	}

	paramsA := getFlatParams(req.IDA)
	paramsB := getFlatParams(req.IDB)
	allKeySet := map[string]struct{}{}
	for k := range paramsA {
		allKeySet[k] = struct{}{}
	}
	for k := range paramsB {
		allKeySet[k] = struct{}{}
	}
	allKeys := make([]string, 0, len(allKeySet))
	for k := range allKeySet {
		allKeys = append(allKeys, k)
	}
	sort.Strings(allKeys)

	type diffRow struct {
		IsDiff bool   `json:"is_diff"`
		Key    string `json:"key"`
		ValA   string `json:"val_a"`
		ValB   string `json:"val_b"`
	}

	diffTable := make([]diffRow, 0, len(allKeys))
	for _, key := range allKeys {
		valA, ok := paramsA[key]
		if !ok {
			valA = "N/A"
		}
		valB, ok := paramsB[key]
		if !ok {
			valB = "N/A"
		}
		isDiff := strings.ToLower(valA) != strings.ToLower(valB)
		diffTable = append(diffTable, diffRow{IsDiff: isDiff, Key: key, ValA: valA, ValB: valB})
	}
	sort.Slice(diffTable, func(i, j int) bool {
		if diffTable[i].IsDiff != diffTable[j].IsDiff {
			return diffTable[i].IsDiff && !diffTable[j].IsDiff
		}
		return diffTable[i].Key < diffTable[j].Key
	})

	resp := gin.H{"status": "success", "diff": diffTable}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
	b := bytes.TrimRight(buf.Bytes(), "\n")
	c.Data(http.StatusOK, "application/json", b)
}

func legacySearchOptions(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	scope := c.Query("scope")
	folderKey := c.Query("folder_key")
	recursive := c.Query("recursive") == "true"

	where := " WHERE name LIKE '%.%'"
	args := []interface{}{}
	if scope != "global" {
		folderPath, err := resolveFolderKeyToPath(folderKey)
		if err == nil {
			folderPrefix := filepath.Clean(folderPath) + string(os.PathSeparator)
			where += " AND path LIKE ?"
			args = append(args, folderPrefix+"%")
			if !recursive {
				where += " AND instr(substr(path, length(?)+1), ?) = 0"
				args = append(args, folderPrefix, string(os.PathSeparator))
			}
		}
	}

	extRows, err := db.Query("SELECT DISTINCT LOWER(SUBSTR(name, INSTR(name, '.') + 1)) as ext FROM files"+where+" GROUP BY ext", args...)
	extensions := []string{}
	if err == nil {
		for extRows.Next() {
			var ext string
			if err := extRows.Scan(&ext); err == nil && isMediaExtension(ext) {
				extensions = append(extensions, ext)
			}
		}
		extRows.Close()
	}

	pfxRows, err := db.Query("SELECT DISTINCT SUBSTR(name, 1, INSTR(name, '_') - 1) as prefix FROM files"+where+" AND name LIKE '%_%' AND SUBSTR(name, 1, INSTR(name, '_') - 1) != '' GROUP BY prefix LIMIT 101", args...)
	prefixes := []string{}
	limitReached := false
	if err == nil {
		count := 0
		for pfxRows.Next() {
			var pfx string
			if err := pfxRows.Scan(&pfx); err == nil && isNumeric(pfx) {
				prefixes = append(prefixes, pfx)
				count++
				if count >= 100 {
					limitReached = true
					break
				}
			}
		}
		pfxRows.Close()
	}

	c.JSON(http.StatusOK, gin.H{"extensions": extensions, "prefixes": prefixes, "prefix_limit_reached": limitReached})
}

func moveBatch(c *gin.Context) {
	var req struct {
		FileIDs           []string `json:"file_ids"`
		DestinationFolder string   `json:"destination_folder"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.FileIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	destDir, err := resolveFolderKeyToPath(req.DestinationFolder)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "Destination folder not found"})
		return
	}
	db := getDBOr500(c)
	if db == nil {
		return
	}
	moved := 0
	failed := 0
	for _, id := range req.FileIDs {
		info, err := lookupFileByID(db, id)
		if err != nil {
			failed++
			continue
		}
		src := info["path"].(string)
		dst := filepath.Join(destDir, filepath.Base(src))
		if err := os.Rename(src, dst); err != nil {
			failed++
			continue
		}
		newID := scanner.GenerateFileID(dst)
		_, _ = db.Exec("UPDATE files SET id = ?, path = ?, name = ? WHERE id = ?", newID, dst, filepath.Base(dst), id)
		moved++
	}
	status := "success"
	if failed > 0 && moved > 0 {
		status = "partial_success"
	} else if failed > 0 && moved == 0 {
		status = "error"
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "message": "Move completed."})
}

func copyBatch(c *gin.Context) {
	var req struct {
		FileIDs           []string `json:"file_ids"`
		DestinationFolder string   `json:"destination_folder"`
		KeepFavorites     bool     `json:"keep_favorites"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.FileIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	destDir, err := resolveFolderKeyToPath(req.DestinationFolder)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "Destination folder not found"})
		return
	}
	db := getDBOr500(c)
	if db == nil {
		return
	}
	copied := 0
	failed := 0
	for _, id := range req.FileIDs {
		info, err := lookupFileByID(db, id)
		if err != nil {
			failed++
			continue
		}
		src := info["path"].(string)
		dst := filepath.Join(destDir, filepath.Base(src))
		if err := copyFile(src, dst); err != nil {
			failed++
			continue
		}
		newID := scanner.GenerateFileID(dst)
		fav := 0
		if req.KeepFavorites && info["is_favorite"].(bool) {
			fav = 1
		}
		_, _ = db.Exec(`
			INSERT OR REPLACE INTO files (id, path, mtime, name, type, duration, dimensions, has_workflow, is_favorite, size, last_scanned, workflow_files, workflow_prompt)
			SELECT ?, ?, mtime, ?, type, duration, dimensions, has_workflow, ?, size, ?, workflow_files, workflow_prompt
			FROM files WHERE id = ?
		`, newID, dst, filepath.Base(dst), fav, float64(time.Now().Unix()), id)
		copied++
	}
	status := "success"
	if failed > 0 && copied > 0 {
		status = "partial_success"
	} else if failed > 0 && copied == 0 {
		status = "error"
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "message": "Copy completed."})
}

func deleteBatchLegacy(c *gin.Context) {
	var req struct {
		FileIDs []string `json:"file_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.FileIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	db := getDBOr500(c)
	if db == nil {
		return
	}
	deleted := 0
	failed := 0
	for _, id := range req.FileIDs {
		if err := deleteFileByID(db, id); err != nil {
			failed++
			continue
		}
		deleted++
	}
	status := "success"
	if failed > 0 && deleted > 0 {
		status = "partial_success"
	} else if failed > 0 && deleted == 0 {
		status = "error"
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "message": "Delete completed."})
}

func deleteFileByID(db *sql.DB, fileID string) error {
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		return err
	}
	fp := info["path"].(string)
	if config.Cfg.DeleteTo != "" {
		trash := config.Cfg.DeleteTo
		_ = os.MkdirAll(trash, 0755)
		dst := filepath.Join(trash, filepath.Base(fp))
		if err := os.Rename(fp, dst); err != nil {
			if err2 := os.Remove(fp); err2 != nil {
				return err
			}
		}
	} else {
		if err := os.Remove(fp); err != nil {
			return err
		}
	}
	_, _ = db.Exec("DELETE FROM files WHERE id = ?", fileID)
	return nil
}

func favoriteBatchLegacy(c *gin.Context) {
	var req struct {
		FileIDs []string `json:"file_ids"`
		Status  bool     `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.FileIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	db := getDBOr500(c)
	if db == nil {
		return
	}
	val := 0
	if req.Status {
		val = 1
	}
	for _, id := range req.FileIDs {
		_, _ = db.Exec("UPDATE files SET is_favorite = ? WHERE id = ?", val, id)
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "Favorite updated."})
}

func toggleFavoriteLegacy(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "File not found"})
		return
	}
	cur := info["is_favorite"].(bool)
	val := 1
	if cur {
		val = 0
	}
	_, _ = db.Exec("UPDATE files SET is_favorite = ? WHERE id = ?", val, fileID)
	c.JSON(http.StatusOK, gin.H{"status": "success", "is_favorite": !cur})
}

func deleteFileLegacy(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	if err := deleteFileByID(db, fileID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "File deleted."})
}

func renameFileLegacy(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	fileID := c.Param("file_id")
	var req struct {
		NewName string `json:"new_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.NewName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "Invalid request"})
		return
	}
	info, err := lookupFileByID(db, fileID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "File not found"})
		return
	}
	oldPath := info["path"].(string)
	dir := filepath.Dir(oldPath)
	ext := filepath.Ext(oldPath)
	newFileName := filepath.Base(req.NewName) + ext
	newPath := filepath.Join(dir, newFileName)
	if _, err := os.Stat(newPath); err == nil {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "A file with that name already exists."})
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Failed to rename file"})
		return
	}
	normNewPath, err := scanner.NormalizePath(newPath)
	if err != nil {
		normNewPath = filepath.Clean(newPath)
	}
	newID := scanner.GenerateFileID(normNewPath)
	_, _ = db.Exec("UPDATE files SET id = ?, path = ?, name = ? WHERE id = ?", newID, normNewPath, newFileName, fileID)
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "File renamed.", "new_name": newFileName, "new_id": newID})
}

func rescanFolder(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	var req struct {
		FolderKey string `json:"folder_key"`
		Mode      string `json:"mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.FolderKey) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "No folder provided."})
		return
	}

	folders, err := buildDynamicFolderConfig(db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
		return
	}
	folderInfo, ok := folders[req.FolderKey]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "Folder not found."})
		return
	}
	folderPath, _ := folderInfo["path"].(string)
	folderName, _ := folderInfo["display_name"].(string)
	folderPath = filepath.Clean(folderPath)
	folderPathNorm, err := scanner.NormalizePath(folderPath)
	if err != nil {
		folderPathNorm = folderPath
	}
	prefixes := uniqueCleanPrefixes(folderPath, folderPathNorm)
	if wd, err := os.Getwd(); err == nil && wd != "" {
		if rel, err := filepath.Rel(wd, folderPathNorm); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			prefixes = uniqueCleanPrefixes(append(prefixes, rel)...)
		}
	}
	if folderName == "" {
		folderName = filepath.Base(folderPath)
	}

	mode := req.Mode
	if mode == "" {
		mode = "all"
	}

	lastByPath := map[string]float64{}
	lastByNorm := map[string]float64{}
	for _, prefix := range prefixes {
		like := prefix + string(os.PathSeparator) + "%"
		rows, err := db.Query("SELECT path, last_scanned FROM files WHERE path LIKE ?", like)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
			return
		}
		for rows.Next() {
			var p string
			var last sql.NullFloat64
			if err := rows.Scan(&p, &last); err != nil {
				continue
			}
			pClean := filepath.Clean(p)
			lastVal := float64(0)
			if last.Valid {
				lastVal = last.Float64
			}
			if pClean != "" {
				if cur, ok := lastByPath[pClean]; !ok || lastVal > cur {
					lastByPath[pClean] = lastVal
				}
				norm, err := scanner.NormalizePath(pClean)
				if err != nil {
					norm = pClean
				}
				if cur, ok := lastByNorm[norm]; !ok || lastVal > cur {
					lastByNorm[norm] = lastVal
				}
			}
		}
		_ = rows.Close()
	}

	walkRoot := folderPath
	if _, err := os.Stat(walkRoot); err != nil {
		walkRoot = folderPathNorm
	}

	files := []string{}
	visited := map[string]struct{}{}
	now := float64(time.Now().Unix())
	cutoff := now - 3600
	filepath.WalkDir(walkRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		name := d.Name()
		if name != "" && name[0] == '.' {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			switch name {
			case ".thumbnails_cache", ".sqlite_cache", ".zip_downloads":
				return filepath.SkipDir
			}
			return nil
		}
		norm, err := scanner.NormalizePath(p)
		if err != nil {
			norm = filepath.Clean(p)
		}
		visited[norm] = struct{}{}

		lastVal := float64(0)
		if v, ok := lastByNorm[norm]; ok {
			lastVal = v
		} else if v, ok := lastByPath[filepath.Clean(p)]; ok {
			lastVal = v
		}
		if mode == "recent" && lastVal >= cutoff {
			return nil
		}
		files = append(files, norm)
		return nil
	})

	dbPaths, err := listPathsUnderPrefixes(db, prefixes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": err.Error()})
		return
	}
	toDelete := computePrunePaths(dbPaths, visited)

	if len(files) == 0 && len(toDelete) == 0 {
		c.JSON(http.StatusOK, gin.H{"status": "success", "message": "No files needed rescanning.", "count": 0})
		return
	}

	jobID := uuidV4()
	job := &rescanJob{Status: "processing", Current: 0, Total: len(files) + len(toDelete), FolderKey: req.FolderKey, FolderName: folderName, CreatedAt: time.Now()}
	rescanJobsMu.Lock()
	rescanJobs[jobID] = job
	rescanJobsMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				rescanJobsMu.Lock()
				job.Status = "error"
				job.Error = fmt.Sprint(r)
				job.FinishedAt = time.Now()
				rescanJobsMu.Unlock()
			}
		}()

		for i, fp := range files {
			_ = scanner.UpsertFile(fp, db)
			rescanJobsMu.Lock()
			job.Current = i + 1
			rescanJobsMu.Unlock()
		}
		if len(toDelete) > 0 {
			delStmt, err := db.Prepare("DELETE FROM files WHERE path = ?")
			if err == nil {
				for i, dp := range toDelete {
					_, _ = delStmt.Exec(dp)
					rescanJobsMu.Lock()
					job.Current = len(files) + i + 1
					rescanJobsMu.Unlock()
				}
				_ = delStmt.Close()
			}
		}
		rescanJobsMu.Lock()
		job.Status = "done"
		job.FinishedAt = time.Now()
		rescanJobsMu.Unlock()
	}()

	c.JSON(http.StatusOK, gin.H{"status": "started", "job_id": jobID, "total": job.Total, "message": "Background process started."})
}

func checkRescanStatus(c *gin.Context) {
	jobID := c.Param("job_id")
	rescanJobsMu.Lock()
	job, ok := rescanJobs[jobID]
	rescanJobsMu.Unlock()
	if !ok {
		c.JSON(http.StatusOK, gin.H{"status": "not_found"})
		return
	}
	if job.Status == "done" {
		c.JSON(http.StatusOK, gin.H{"status": "done", "current": job.Current, "total": job.Total, "folder_key": job.FolderKey, "folder_name": job.FolderName})
		return
	}
	if job.Status == "error" {
		c.JSON(http.StatusOK, gin.H{"status": "error", "current": job.Current, "total": job.Total, "folder_key": job.FolderKey, "folder_name": job.FolderName, "error": job.Error})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "processing", "current": job.Current, "total": job.Total, "folder_key": job.FolderKey, "folder_name": job.FolderName})
}

func sortStrings(xs []string) {
	for i := 0; i < len(xs); i++ {
		for j := i + 1; j < len(xs); j++ {
			if xs[j] < xs[i] {
				xs[i], xs[j] = xs[j], xs[i]
			}
		}
	}
}

func getFiles(c *gin.Context) {
	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	rows, err := db.Query(`
		SELECT id, path, mtime, name, type, duration, dimensions, 
		       has_workflow, is_favorite, size, last_scanned
		FROM files
		ORDER BY mtime DESC
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to query files",
		})
		return
	}
	defer rows.Close()

	var files []map[string]interface{}
	for rows.Next() {
		var id, path, name, fileType, duration, dimensions string
		var mtime, lastScanned float64
		var hasWorkflow, isFavorite int
		var size int64

		if err := rows.Scan(&id, &path, &mtime, &name, &fileType, &duration, &dimensions,
			&hasWorkflow, &isFavorite, &size, &lastScanned); err != nil {
			continue
		}

		files = append(files, map[string]interface{}{
			"id":           id,
			"path":         path,
			"mtime":        mtime,
			"name":         name,
			"type":         fileType,
			"duration":     duration,
			"dimensions":   dimensions,
			"has_workflow": hasWorkflow == 1,
			"is_favorite":  isFavorite == 1,
			"size":         size,
			"last_scanned": lastScanned,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"files":   files,
		"total":   len(files),
	})
}

func getFile(c *gin.Context) {
	fileID := c.Param("id")
	if fileID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "File ID is required",
		})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	row := db.QueryRow(`
		SELECT id, path, mtime, name, type, duration, dimensions, 
		       has_workflow, is_favorite, size, last_scanned
		FROM files
		WHERE id = ?
	`, fileID)

	var id, path, name, fileType, duration, dimensions string
	var mtime, lastScanned float64
	var hasWorkflow, isFavorite int
	var size int64

	if err := row.Scan(&id, &path, &mtime, &name, &fileType, &duration, &dimensions,
		&hasWorkflow, &isFavorite, &size, &lastScanned); err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "File not found",
		})
		return
	}

	file := map[string]interface{}{
		"id":           id,
		"path":         path,
		"mtime":        mtime,
		"name":         name,
		"type":         fileType,
		"duration":     duration,
		"dimensions":   dimensions,
		"has_workflow": hasWorkflow == 1,
		"is_favorite":  isFavorite == 1,
		"size":         size,
		"last_scanned": lastScanned,
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"file":    file,
	})
}

func deleteFiles(c *gin.Context) {
	var request struct {
		FileIDs []string `json:"file_ids"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request format",
		})
		return
	}

	if len(request.FileIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "No file IDs provided",
		})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	deletedCount := 0
	failedFiles := []string{}

	for _, fileID := range request.FileIDs {
		var filePath string
		err := db.QueryRow("SELECT path FROM files WHERE id = ?", fileID).Scan(&filePath)
		if err != nil {
			failedFiles = append(failedFiles, fileID)
			continue
		}

		if err := os.Remove(filePath); err != nil {
			failedFiles = append(failedFiles, fileID)
			continue
		}

		_, err = db.Exec("DELETE FROM files WHERE id = ?", fileID)
		if err != nil {
			failedFiles = append(failedFiles, fileID)
			continue
		}

		deletedCount++
	}

	message := "Files deleted successfully"
	if len(failedFiles) > 0 {
		message = "Some files failed to delete"
	}

	c.JSON(http.StatusOK, gin.H{
		"success":       true,
		"message":       message,
		"deleted_count": deletedCount,
		"failed_files":  failedFiles,
	})
}

func toggleFavorite(c *gin.Context) {
	var request struct {
		FileIDs []string `json:"file_ids"`
		Status  bool     `json:"status"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request format",
		})
		return
	}

	if len(request.FileIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "No file IDs provided",
		})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	statusValue := 0
	if request.Status {
		statusValue = 1
	}

	updatedCount := 0
	for _, fileID := range request.FileIDs {
		_, err := db.Exec("UPDATE files SET is_favorite = ? WHERE id = ?", statusValue, fileID)
		if err == nil {
			updatedCount++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success":       true,
		"message":       "Favorite status toggled",
		"updated_count": updatedCount,
	})
}

func getFolders(c *gin.Context) {
	basePath := config.Cfg.BaseOutputPath
	folders := []map[string]interface{}{}

	if err := scanFolders(basePath, "", &folders); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to scan folders",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"folders": folders,
	})
}

func scanFolders(basePath, relativePath string, folders *[]map[string]interface{}) error {
	fullPath := filepath.Join(basePath, relativePath)

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name()[0] == '.' {
				continue
			}

			dirName := entry.Name()
			if dirName == ".thumbnails_cache" || dirName == ".sqlite_cache" || dirName == ".zip_downloads" {
				continue
			}

			newRelativePath := filepath.Join(relativePath, dirName)

			*folders = append(*folders, map[string]interface{}{
				"name":      dirName,
				"path":      newRelativePath,
				"full_path": filepath.Join(basePath, newRelativePath),
			})

			if err := scanFolders(basePath, newRelativePath, folders); err != nil {
				continue
			}
		}
	}

	return nil
}

func createFolder(c *gin.Context) {
	var request struct {
		ParentPath string `json:"parent_path"`
		FolderName string `json:"folder_name"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request format",
		})
		return
	}

	if request.FolderName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Folder name is required",
		})
		return
	}

	basePath := config.Cfg.BaseOutputPath
	newFolderPath := filepath.Join(basePath, request.ParentPath, request.FolderName)

	if err := os.MkdirAll(newFolderPath, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to create folder",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"message":     "Folder created successfully",
		"folder_path": filepath.Join(request.ParentPath, request.FolderName),
	})
}

func renameFolder(c *gin.Context) {
	var request struct {
		OldPath string `json:"old_path"`
		NewName string `json:"new_name"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request format",
		})
		return
	}

	if request.NewName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "New folder name is required",
		})
		return
	}

	basePath := config.Cfg.BaseOutputPath
	oldFolderPath := filepath.Join(basePath, request.OldPath)
	parentPath := filepath.Dir(oldFolderPath)
	newFolderPath := filepath.Join(parentPath, request.NewName)

	if err := os.Rename(oldFolderPath, newFolderPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to rename folder",
		})
		return
	}

	db := database.GetDB()
	if db != nil {
		oldPathPrefix := oldFolderPath + string(os.PathSeparator)
		newPathPrefix := newFolderPath + string(os.PathSeparator)

		_, err := db.Exec(`
			UPDATE files 
			SET path = REPLACE(path, ?, ?) 
			WHERE path LIKE ?
		`, oldPathPrefix, newPathPrefix, oldPathPrefix+"%")
		_ = err
	}

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"message":  "Folder renamed successfully",
		"old_path": request.OldPath,
		"new_path": filepath.Join(filepath.Dir(request.OldPath), request.NewName),
	})
}

func getWorkflow(c *gin.Context) {
	fileID := c.Param("id")
	if fileID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "File ID is required",
		})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	var filePath string
	err := db.QueryRow("SELECT path FROM files WHERE id = ?", fileID).Scan(&filePath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "File not found",
		})
		return
	}

	workflow := scanner.ExtractWorkflow(filePath, "ui")
	if workflow == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "No workflow found for this file",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"workflow": workflow,
	})
}

func searchFiles(c *gin.Context) {
	searchTerm := c.Query("q")
	extensions := c.QueryArray("extension")
	startDate := c.Query("start_date")
	endDate := c.Query("end_date")
	favorites := c.Query("favorites")
	noWorkflow := c.Query("no_workflow")

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	query := `
		SELECT id, path, mtime, name, type, duration, dimensions, 
		       has_workflow, is_favorite, size, last_scanned
		FROM files
		WHERE 1=1
	`
	args := []interface{}{}

	if searchTerm != "" {
		query += " AND (name LIKE ? OR workflow_prompt LIKE ?)"
		args = append(args, "%"+searchTerm+"%", "%"+searchTerm+"%")
	}

	if len(extensions) > 0 {
		query += " AND ("
		for i, ext := range extensions {
			if i > 0 {
				query += " OR"
			}
			query += " name LIKE ?"
			args = append(args, "%."+ext)
		}
		query += ")"
	}

	if startDate != "" {
		query += " AND mtime >= ?"
		args = append(args, startDate)
	}

	if endDate != "" {
		query += " AND mtime <= ?"
		args = append(args, endDate)
	}

	if favorites == "true" {
		query += " AND is_favorite = 1"
	}

	if noWorkflow == "true" {
		query += " AND has_workflow = 0"
	}

	query += " ORDER BY mtime DESC"

	rows, err := db.Query(query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to search files",
		})
		return
	}
	defer rows.Close()

	var files []map[string]interface{}
	for rows.Next() {
		var id, path, name, fileType, duration, dimensions string
		var mtime, lastScanned float64
		var hasWorkflow, isFavorite int
		var size int64

		if err := rows.Scan(&id, &path, &mtime, &name, &fileType, &duration, &dimensions,
			&hasWorkflow, &isFavorite, &size, &lastScanned); err != nil {
			continue
		}

		files = append(files, map[string]interface{}{
			"id":           id,
			"path":         path,
			"mtime":        mtime,
			"name":         name,
			"type":         fileType,
			"duration":     duration,
			"dimensions":   dimensions,
			"has_workflow": hasWorkflow == 1,
			"is_favorite":  isFavorite == 1,
			"size":         size,
			"last_scanned": lastScanned,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"files":   files,
		"total":   len(files),
	})
}

func getConfig(c *gin.Context) {
	ffmpegAvailable := false
	if p, err := exec.LookPath("ffmpeg"); err == nil && p != "" {
		ffmpegAvailable = true
	} else if p, err := exec.LookPath("ffmpeg.exe"); err == nil && p != "" {
		ffmpegAvailable = true
	}
	streamThresholdBytes := int64(config.Cfg.StreamThresholdMb) * 1024 * 1024
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"config": gin.H{
			"serverPort":           config.Cfg.ServerPort,
			"thumbnailWidth":       config.Cfg.ThumbnailWidth,
			"pageSize":             config.Cfg.PageSize,
			"ffmpegAvailable":      ffmpegAvailable,
			"streamThresholdBytes": streamThresholdBytes,
		},
	})
}

func searchOptions(c *gin.Context) {
	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	extensions := []string{}
	prefixes := []string{}
	prefixLimitReached := false

	rows, err := db.Query("SELECT DISTINCT LOWER(SUBSTR(name, INSTR(name, '.') + 1)) as ext FROM files WHERE name LIKE '%.%' GROUP BY ext")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var ext string
			if err := rows.Scan(&ext); err == nil {
				if isMediaExtension(ext) {
					extensions = append(extensions, ext)
				}
			}
		}
	}

	rows, err = db.Query("SELECT DISTINCT SUBSTR(name, 1, INSTR(name, '_') - 1) as prefix FROM files WHERE name LIKE '%_%' AND SUBSTR(name, 1, INSTR(name, '_') - 1) != '' GROUP BY prefix LIMIT 101")
	if err == nil {
		defer rows.Close()
		count := 0
		for rows.Next() {
			var prefix string
			if err := rows.Scan(&prefix); err == nil {
				if isNumeric(prefix) {
					prefixes = append(prefixes, prefix)
					count++
					if count >= 100 {
						prefixLimitReached = true
						break
					}
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"extensions":           extensions,
		"prefixes":             prefixes,
		"prefix_limit_reached": prefixLimitReached,
	})
}

func isMediaExtension(ext string) bool {
	mediaExtensions := map[string]bool{
		"png": true, "jpg": true, "jpeg": true, "webp": true, "gif": true,
		"mp4": true, "mov": true, "webm": true, "mkv": true, "avi": true,
		"mp3": true, "wav": true, "ogg": true, "flac": true, "m4a": true,
	}
	return mediaExtensions[ext]
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func moveFiles(c *gin.Context) {
	var request struct {
		FileIDs              []string `json:"file_ids"`
		DestinationFolderKey string   `json:"destination_folder"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request format",
		})
		return
	}

	if len(request.FileIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "No file IDs provided",
		})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	var destPath string
	err := db.QueryRow("SELECT path FROM mounted_folders WHERE path = ?", request.DestinationFolderKey).Scan(&destPath)
	if err != nil {
		destPath = config.Cfg.BaseOutputPath
	}

	movedCount := 0
	failedFiles := []string{}

	for _, fileID := range request.FileIDs {
		var filePath string
		err := db.QueryRow("SELECT path FROM files WHERE id = ?", fileID).Scan(&filePath)
		if err != nil {
			failedFiles = append(failedFiles, fileID)
			continue
		}

		filename := filepath.Base(filePath)
		newFilePath := filepath.Join(destPath, filename)

		if err := os.Rename(filePath, newFilePath); err != nil {
			failedFiles = append(failedFiles, fileID)
			continue
		}

		normNewPath, err := scanner.NormalizePath(newFilePath)
		if err != nil {
			normNewPath = filepath.Clean(newFilePath)
		}
		newFileID := scanner.GenerateFileID(normNewPath)
		_, err = db.Exec("UPDATE files SET id = ?, path = ? WHERE id = ?", newFileID, normNewPath, fileID)
		if err != nil {
			failedFiles = append(failedFiles, fileID)
			continue
		}

		movedCount++
	}

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      "Files moved successfully",
		"moved_count":  movedCount,
		"failed_files": failedFiles,
	})
}

func copyFiles(c *gin.Context) {
	var request struct {
		FileIDs              []string `json:"file_ids"`
		DestinationFolderKey string   `json:"destination_folder"`
		KeepFavorites        bool     `json:"keep_favorites"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request format",
		})
		return
	}

	if len(request.FileIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "No file IDs provided",
		})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	var destPath string
	err := db.QueryRow("SELECT path FROM mounted_folders WHERE path = ?", request.DestinationFolderKey).Scan(&destPath)
	if err != nil {
		destPath = config.Cfg.BaseOutputPath
	}

	copiedCount := 0
	failedFiles := []string{}

	for _, fileID := range request.FileIDs {
		var filePath, name, fileType, duration, dimensions string
		var mtime, lastScanned float64
		var hasWorkflow, isFavorite int
		var size int64

		err := db.QueryRow(`
			SELECT path, name, type, duration, dimensions, mtime, has_workflow, is_favorite, size, last_scanned
			FROM files WHERE id = ?
		`, fileID).Scan(&filePath, &name, &fileType, &duration, &dimensions, &mtime, &hasWorkflow, &isFavorite, &size, &lastScanned)
		if err != nil {
			failedFiles = append(failedFiles, fileID)
			continue
		}

		filename := filepath.Base(filePath)
		newFilePath := filepath.Join(destPath, filename)

		if err := copyFile(filePath, newFilePath); err != nil {
			failedFiles = append(failedFiles, fileID)
			continue
		}

		normNewPath, err := scanner.NormalizePath(newFilePath)
		if err != nil {
			normNewPath = filepath.Clean(newFilePath)
		}
		newFileID := scanner.GenerateFileID(normNewPath)
		favoriteValue := 0
		if request.KeepFavorites {
			favoriteValue = isFavorite
		}

		_, err = db.Exec(`
			INSERT INTO files (
				id, path, mtime, name, type, duration, dimensions, 
				has_workflow, is_favorite, size, last_scanned
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, newFileID, normNewPath, time.Now().Unix(), name, fileType, duration, dimensions, hasWorkflow, favoriteValue, size, time.Now().Unix())
		if err != nil {
			failedFiles = append(failedFiles, fileID)
			continue
		}

		copiedCount++
	}

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      "Files copied successfully",
		"copied_count": copiedCount,
		"failed_files": failedFiles,
	})
}

func renameFile(c *gin.Context) {
	var request struct {
		FileID  string `json:"file_id"`
		NewName string `json:"new_name"`
	}

	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "Invalid request format",
		})
		return
	}

	if request.NewName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "New name is required",
		})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	var filePath string
	err := db.QueryRow("SELECT path FROM files WHERE id = ?", request.FileID).Scan(&filePath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "File not found",
		})
		return
	}

	dir := filepath.Dir(filePath)
	ext := filepath.Ext(filePath)
	newFileName := request.NewName
	if ext != "" {
		newFileName += ext
	}
	newFilePath := filepath.Join(dir, newFileName)

	if err := os.Rename(filePath, newFilePath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to rename file",
		})
		return
	}

	newFileID := scanner.GenerateFileID(newFilePath)
	_, err = db.Exec("UPDATE files SET id = ?, path = ?, name = ? WHERE id = ?", newFileID, newFilePath, newFileName, request.FileID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to update database",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"message":  "File renamed successfully",
		"new_name": newFileName,
		"new_id":   newFileID,
	})
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

func serveInputFile(c *gin.Context) {
	path := c.Param("path")
	fullPath := filepath.Join(config.Cfg.BaseInputPath, path)
	c.File(fullPath)
}

func serveOutputFile(c *gin.Context) {
	path := c.Param("path")
	fullPath := filepath.Join(config.Cfg.BaseOutputPath, path)
	c.File(fullPath)
}

func serveThumbnail(c *gin.Context) {
	fileID := c.Param("path")
	if fileID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "File ID is required",
		})
		return
	}

	db := database.GetDB()
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Database not initialized",
		})
		return
	}

	var filePath, fileType string
	err := db.QueryRow("SELECT path, type FROM files WHERE id = ?", fileID).Scan(&filePath, &fileType)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "File not found",
		})
		return
	}

	thumbnailPath, err := scanner.GenerateThumbnail(filePath, fileType)
	if err != nil {
		log.Printf("thumbnail generation failed: id=%s type=%s path=%s err=%v", fileID, fileType, filePath, err)
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "Thumbnail generation failed",
		})
		return
	}

	c.File(thumbnailPath)
}
