// 本文件是“API 层集成测试”：用 httptest 启动完整 Gin Router，
// 逐个调用 static/index.html 中使用到的所有 /galleryout 接口，确保前后端契约不被回归破坏。
package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"smart-comfyui-gallery/config"
	"smart-comfyui-gallery/database"
	"smart-comfyui-gallery/scanner"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// projectRootDir 返回仓库根目录（通过当前测试文件位置推导）。
func projectRootDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Dir(filepath.Dir(file)))
}

// doRequest 发送一条 HTTP 请求到 router，并返回 recorder 便于断言 status/header/body。
func doRequest(t *testing.T, r http.Handler, method, path string, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// doJSON 发送 JSON 请求体。
func doJSON(t *testing.T, r http.Handler, method, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	return doRequest(t, r, method, path, map[string]string{"Content-Type": "application/json"}, b)
}

// decodeJSON 将响应 body 解成 map，便于快速取 status/job_id 等字段。
func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json unmarshal: %v body=%q", err, string(b))
	}
	return m
}

// requireStatus 断言 HTTP 状态码一致（失败时打印 body）。
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status=%d want=%d body=%q", w.Code, want, w.Body.String())
	}
}

// getFileRowByName 从 SQLite files 表按文件名取一条记录（供按需获取 file_id）。
func getFileRowByName(t *testing.T, db *sql.DB, name string) (id string, path string) {
	t.Helper()
	if err := db.QueryRow("SELECT id, path FROM files WHERE name = ? LIMIT 1", name).Scan(&id, &path); err != nil {
		t.Fatalf("query file by name %q: %v", name, err)
	}
	return id, path
}

func requireDirExists(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("expected dir exists %q: %v", p, err)
	}
	if !fi.IsDir() {
		t.Fatalf("expected dir %q", p)
	}
}

func requireFileExists(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("expected file exists %q: %v", p, err)
	}
	if fi.IsDir() {
		t.Fatalf("expected file %q", p)
	}
}

func requireNotExists(t *testing.T, p string) {
	t.Helper()
	_, err := os.Stat(p)
	if err == nil {
		t.Fatalf("expected not exists %q", p)
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected not exists %q: %v", p, err)
	}
}

func requireSymlinkTo(t *testing.T, linkPath, target string) {
	t.Helper()
	fi, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("lstat link %q: %v", linkPath, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected symlink %q", linkPath)
	}
	got, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink %q: %v", linkPath, err)
	}
	if filepath.Clean(got) != filepath.Clean(target) {
		t.Fatalf("symlink target=%q want=%q", got, target)
	}
}

// waitForZipJob 轮询 ZIP 后台任务，直到 ready/error（避免测试依赖固定 sleep）。
func waitForZipJob(t *testing.T, r http.Handler, jobID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := doRequest(t, r, http.MethodGet, "/galleryout/check_zip_status/"+jobID, nil, nil)
		requireStatus(t, w, http.StatusOK)
		m := decodeJSON(t, w.Body.Bytes())
		if s, _ := m["status"].(string); s == "ready" || s == "error" {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("zip job timeout job_id=%s last=%q", jobID, w.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForRescanJob 轮询 rescan 后台任务，直到 done/error/not_found（避免测试依赖固定 sleep）。
func waitForRescanJob(t *testing.T, r http.Handler, jobID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := doRequest(t, r, http.MethodGet, "/galleryout/check_rescan_status/"+jobID, nil, nil)
		requireStatus(t, w, http.StatusOK)
		m := decodeJSON(t, w.Body.Bytes())
		if s, _ := m["status"].(string); s == "done" || s == "error" || s == "not_found" {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("rescan job timeout job_id=%s last=%q", jobID, w.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAPI_Integration_CoversIndexHTML(t *testing.T) {
	gin.SetMode(gin.TestMode)

	root := projectRootDir(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// 关键点：必须切到项目根目录再加载 .env.example。
	// 因为 .env.example 里 BASE_OUTPUT_PATH/BASE_INPUT_PATH 是相对路径，
	// 若以包目录(server/)为工作目录，会把 sqlite/测试资源解析到错误的位置。
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	t.Setenv("ENV_FILE", filepath.Join(root, ".env.example"))
	if err := config.Load(); err != nil {
		t.Fatalf("config load: %v", err)
	}
	if err := database.Init(); err != nil {
		t.Fatalf("database init: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	db := database.GetDB()
	if db == nil {
		t.Fatalf("db nil")
	}

	outputAbs, err := filepath.Abs(config.Cfg.BaseOutputPath)
	if err != nil {
		t.Fatalf("abs output: %v", err)
	}

	// 依赖 test_data/output 下已有的媒体文件作为“真实样本”，补齐 DB 记录。
	// 这里主动 DELETE 再 Upsert，避免复用旧 DB 状态导致不稳定（如文件已被上一次测试删除/改名）。
	seed := []string{
		filepath.Join(outputAbs, "flux_dev_example.png"),
		filepath.Join(outputAbs, "ComfyUI_00010_.mp4"),
		filepath.Join(outputAbs, "SD", "SD1.5-Controlnet-Canny-comfyui-wiki.52a4f479.png"),
		filepath.Join(outputAbs, "ace-step-v1-a2a.mp3"),
	}
	for _, p := range seed {
		if _, err := os.Stat(p); err == nil {
			_, _ = db.Exec("DELETE FROM files WHERE path = ?", p)
			_ = scanner.UpsertFile(p, db)
		}
	}

	var cleanupPaths []string
	// 清理：删除本测试创建的文件/目录，并同步清理 DB（避免污染后续测试或本地运行环境）。
	t.Cleanup(func() {
		for _, p := range cleanupPaths {
			_ = os.RemoveAll(p)
			clean := filepath.Clean(p)
			_, _ = db.Exec("DELETE FROM files WHERE path = ? OR path LIKE ?", clean, clean+string(os.PathSeparator)+"%")
		}
	})

	// 使用生产 router（包含静态资源、模板渲染、以及全部 /galleryout 端点）。
	r := NewRouter(RouterOptions{RootDir: root, DisableLogger: true})

	// suffix 用于确保测试生成的文件/目录名唯一，避免并行或重复运行互相干扰。
	suffix := strings.ReplaceAll(strings.ReplaceAll(time.Now().Format("20060102_150405.000000000"), ":", ""), ".", "")

	t.Run("view_root", func(t *testing.T) {
		// 前端入口页：/galleryout/view/_root_ 应返回 HTML 页面。
		w := doRequest(t, r, http.MethodGet, "/galleryout/view/_root_", nil, nil)
		requireStatus(t, w, http.StatusOK)
		if !strings.Contains(w.Body.String(), "<!DOCTYPE html>") {
			t.Fatalf("unexpected body: %q", w.Body.String()[:min(200, len(w.Body.String()))])
		}
	})

	t.Run("folder_crud", func(t *testing.T) {
		// 覆盖：create_folder / rename_folder / delete_folder
		name := "itest_folder_" + suffix
		w := doJSON(t, r, http.MethodPost, "/galleryout/create_folder", map[string]any{
			"parent_key":  "_root_",
			"folder_name": name,
		})
		requireStatus(t, w, http.StatusOK)
		oldDir := filepath.Join(outputAbs, name)
		requireDirExists(t, oldDir)

		key := pathToFolderKey(name)
		newName := name + "_renamed"
		w2 := doJSON(t, r, http.MethodPost, "/galleryout/rename_folder/"+key, map[string]any{
			"new_name": newName,
		})
		requireStatus(t, w2, http.StatusOK)
		newDir := filepath.Join(outputAbs, newName)
		requireNotExists(t, oldDir)
		requireDirExists(t, newDir)

		newKey := pathToFolderKey(newName)
		w3 := doRequest(t, r, http.MethodPost, "/galleryout/delete_folder/"+newKey, map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
		requireStatus(t, w3, http.StatusOK)
		requireNotExists(t, newDir)
	})

	t.Run("browse_filesystem", func(t *testing.T) {
		// 覆盖：/galleryout/api/browse_filesystem（用于挂载面板的文件系统浏览）
		w := doJSON(t, r, http.MethodPost, "/galleryout/api/browse_filesystem", map[string]any{
			"path": outputAbs,
		})
		requireStatus(t, w, http.StatusOK)
		m := decodeJSON(t, w.Body.Bytes())
		if got, _ := m["current_path"].(string); got == "" {
			t.Fatalf("missing current_path: %q", w.Body.String())
		}
		folders, ok := m["folders"].([]any)
		if !ok {
			t.Fatalf("missing folders: %q", w.Body.String())
		}
		foundSD := false
		for _, it := range folders {
			obj, _ := it.(map[string]any)
			if name, _ := obj["name"].(string); name == "SD" {
				foundSD = true
				break
			}
		}
		if !foundSD {
			t.Fatalf("expected folders include SD: %q", w.Body.String())
		}
	})

	t.Run("mount_unmount", func(t *testing.T) {
		// 覆盖：mount_folder / unmount_folder
		target := t.TempDir()
		linkName := "itest_mount_" + suffix
		linkPath := filepath.Join(outputAbs, linkName)
		_ = os.Remove(linkPath)

		w := doJSON(t, r, http.MethodPost, "/galleryout/mount_folder", map[string]any{
			"link_name":   linkName,
			"target_path": target,
		})
		requireStatus(t, w, http.StatusOK)
		requireSymlinkTo(t, linkPath, target)

		w2 := doJSON(t, r, http.MethodPost, "/galleryout/unmount_folder", map[string]any{
			"folder_key": pathToFolderKey(linkName),
		})
		requireStatus(t, w2, http.StatusOK)
		requireNotExists(t, linkPath)
	})

	t.Run("load_more_and_search_options", func(t *testing.T) {
		// 覆盖：/galleryout/load_more（列表分页） + /galleryout/api/search_options（筛选项）
		w := doRequest(t, r, http.MethodGet, "/galleryout/load_more?offset=0&limit=5&folder_key=_root_", nil, nil)
		requireStatus(t, w, http.StatusOK)
		m := decodeJSON(t, w.Body.Bytes())
		files, ok := m["files"].([]any)
		if !ok || len(files) == 0 {
			t.Fatalf("expected files: %q", w.Body.String())
		}

		w2 := doRequest(t, r, http.MethodGet, "/galleryout/api/search_options?scope=all&folder_key=_root_&recursive=true", nil, nil)
		requireStatus(t, w2, http.StatusOK)
		m2 := decodeJSON(t, w2.Body.Bytes())
		if _, ok := m2["extensions"]; !ok {
			t.Fatalf("missing extensions: %q", w2.Body.String())
		}
	})

	t.Run("favorite_toggle_and_batch", func(t *testing.T) {
		// 覆盖：toggle_favorite + favorite_batch
		id, _ := getFileRowByName(t, db, "flux_dev_example.png")

		w := doRequest(t, r, http.MethodPost, "/galleryout/toggle_favorite/"+id, nil, nil)
		requireStatus(t, w, http.StatusOK)
		w2 := doRequest(t, r, http.MethodPost, "/galleryout/toggle_favorite/"+id, nil, nil)
		requireStatus(t, w2, http.StatusOK)

		w3 := doJSON(t, r, http.MethodPost, "/galleryout/favorite_batch", map[string]any{
			"file_ids": []string{id},
			"status":   true,
		})
		requireStatus(t, w3, http.StatusOK)

		w4 := doJSON(t, r, http.MethodPost, "/galleryout/favorite_batch", map[string]any{
			"file_ids": []string{id},
			"status":   false,
		})
		requireStatus(t, w4, http.StatusOK)
	})

	t.Run("rename_delete_and_batches", func(t *testing.T) {
		// 覆盖：rename_file / delete / copy_batch / move_batch / delete_batch
		workFolder := filepath.Join(outputAbs, "itest_work_"+suffix)
		destFolder := filepath.Join(outputAbs, "itest_dest_"+suffix)
		cleanupPaths = append(cleanupPaths, workFolder, destFolder)
		if err := os.MkdirAll(workFolder, 0755); err != nil {
			t.Fatalf("mkdir work: %v", err)
		}
		if err := os.MkdirAll(destFolder, 0755); err != nil {
			t.Fatalf("mkdir dest: %v", err)
		}

		srcInput := filepath.Join(root, "test_data", "input", "flux_dev_example.png")
		renamePath := filepath.Join(workFolder, "rename_src_"+suffix+".png")
		b, err := os.ReadFile(srcInput)
		if err != nil {
			t.Fatalf("read src: %v", err)
		}
		if err := os.WriteFile(renamePath, b, 0644); err != nil {
			t.Fatalf("write rename src: %v", err)
		}
		requireFileExists(t, renamePath)
		if err := scanner.UpsertFile(renamePath, db); err != nil {
			t.Fatalf("upsert rename src: %v", err)
		}
		renameID := scanner.GenerateFileID(renamePath)

		w := doJSON(t, r, http.MethodPost, "/galleryout/rename_file/"+renameID, map[string]any{
			"new_name": "renamed_" + suffix,
		})
		requireStatus(t, w, http.StatusOK)
		m := decodeJSON(t, w.Body.Bytes())
		newID, _ := m["new_id"].(string)
		if newID == "" {
			t.Fatalf("missing new_id: %q", w.Body.String())
		}
		newName, _ := m["new_name"].(string)
		if newName == "" {
			t.Fatalf("missing new_name: %q", w.Body.String())
		}
		newPath := filepath.Join(workFolder, newName)
		requireNotExists(t, renamePath)
		requireFileExists(t, newPath)

		w2 := doRequest(t, r, http.MethodPost, "/galleryout/delete/"+newID, nil, nil)
		requireStatus(t, w2, http.StatusOK)
		requireNotExists(t, newPath)

		copySrc := filepath.Join(workFolder, "copy_src_"+suffix+".png")
		moveSrc := filepath.Join(workFolder, "move_src_"+suffix+".png")
		if err := os.WriteFile(copySrc, b, 0644); err != nil {
			t.Fatalf("write copy src: %v", err)
		}
		if err := os.WriteFile(moveSrc, b, 0644); err != nil {
			t.Fatalf("write move src: %v", err)
		}
		if err := scanner.UpsertFile(copySrc, db); err != nil {
			t.Fatalf("upsert copy src: %v", err)
		}
		if err := scanner.UpsertFile(moveSrc, db); err != nil {
			t.Fatalf("upsert move src: %v", err)
		}

		copyID := scanner.GenerateFileID(copySrc)
		moveID := scanner.GenerateFileID(moveSrc)

		destKey := pathToFolderKey(filepath.Base(destFolder))

		w3 := doJSON(t, r, http.MethodPost, "/galleryout/copy_batch", map[string]any{
			"file_ids":           []string{copyID},
			"destination_folder": destKey,
			"keep_favorites":     false,
		})
		requireStatus(t, w3, http.StatusOK)

		copyDestPath := filepath.Join(destFolder, filepath.Base(copySrc))
		requireFileExists(t, copySrc)
		requireFileExists(t, copyDestPath)
		srcB, _ := os.ReadFile(copySrc)
		dstB, _ := os.ReadFile(copyDestPath)
		if !bytes.Equal(srcB, dstB) {
			t.Fatalf("copy content mismatch src=%q dst=%q", copySrc, copyDestPath)
		}
		copyDestID := scanner.GenerateFileID(copyDestPath)

		w4 := doJSON(t, r, http.MethodPost, "/galleryout/move_batch", map[string]any{
			"file_ids":           []string{moveID},
			"destination_folder": destKey,
		})
		requireStatus(t, w4, http.StatusOK)

		moveDestPath := filepath.Join(destFolder, filepath.Base(moveSrc))
		requireNotExists(t, moveSrc)
		requireFileExists(t, moveDestPath)
		moveDestID := scanner.GenerateFileID(moveDestPath)

		w5 := doJSON(t, r, http.MethodPost, "/galleryout/delete_batch", map[string]any{
			"file_ids": []string{copyID, copyDestID, moveDestID},
		})
		requireStatus(t, w5, http.StatusOK)
		requireNotExists(t, copySrc)
		requireNotExists(t, copyDestPath)
		requireNotExists(t, moveDestPath)
	})

	t.Run("workflow_metadata_node_summary", func(t *testing.T) {
		// 覆盖：check_metadata / workflow / node_summary
		// 这里构造一个“包含工作流 JSON 的 mp3 文件”，用于稳定验证 workflow 相关接口。
		ui := `{"nodes":[]}`
		api := `{"1":{"class_type":"KSampler"}}`
		p := filepath.Join(outputAbs, "itest_wf_"+suffix+".mp3")
		cleanupPaths = append(cleanupPaths, p)
		if err := os.WriteFile(p, []byte("aaa"+ui+"bbb"+api+"ccc"), 0644); err != nil {
			t.Fatalf("write wf file: %v", err)
		}
		if err := scanner.UpsertFile(p, db); err != nil {
			t.Fatalf("upsert wf: %v", err)
		}
		id := scanner.GenerateFileID(p)

		w := doRequest(t, r, http.MethodGet, "/galleryout/check_metadata/"+id, nil, nil)
		requireStatus(t, w, http.StatusOK)

		w2 := doRequest(t, r, http.MethodGet, "/galleryout/workflow/"+id, nil, nil)
		requireStatus(t, w2, http.StatusOK)
		if strings.TrimSpace(w2.Body.String()) != ui {
			t.Fatalf("workflow mismatch got=%q want=%q", strings.TrimSpace(w2.Body.String()), ui)
		}

		w3 := doRequest(t, r, http.MethodGet, "/galleryout/node_summary/"+id, nil, nil)
		requireStatus(t, w3, http.StatusOK)
		m := decodeJSON(t, w3.Body.Bytes())
		if s, _ := m["status"].(string); s != "success" {
			t.Fatalf("node_summary status=%q body=%q", s, w3.Body.String())
		}
	})

	t.Run("upload", func(t *testing.T) {
		// 覆盖：/galleryout/upload（multipart + folder_key）
		workFolder := "itest_upload_" + suffix
		workKey := pathToFolderKey(workFolder)
		w := doJSON(t, r, http.MethodPost, "/galleryout/create_folder", map[string]any{
			"parent_key":  "_root_",
			"folder_name": workFolder,
		})
		requireStatus(t, w, http.StatusOK)

		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("folder_key", workKey)
		fw, err := mw.CreateFormFile("files", "upload_test_"+suffix+".txt")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		_, _ = fw.Write([]byte("hello"))
		_ = mw.Close()

		w2 := doRequest(t, r, http.MethodPost, "/galleryout/upload", map[string]string{"Content-Type": mw.FormDataContentType()}, buf.Bytes())
		if w2.Code != http.StatusOK && w2.Code != http.StatusMultiStatus {
			t.Fatalf("status=%d body=%q", w2.Code, w2.Body.String())
		}
		uploadedPath := filepath.Join(outputAbs, workFolder, "upload_test_"+suffix+".txt")
		requireFileExists(t, uploadedPath)
		got, err := os.ReadFile(uploadedPath)
		if err != nil {
			t.Fatalf("read uploaded: %v", err)
		}
		if string(got) != "hello" {
			t.Fatalf("uploaded content=%q", string(got))
		}

		w3 := doRequest(t, r, http.MethodPost, "/galleryout/delete_folder/"+workKey, map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
		requireStatus(t, w3, http.StatusOK)
		requireNotExists(t, filepath.Join(outputAbs, workFolder))
		_, _ = db.Exec("DELETE FROM files WHERE path LIKE ?", filepath.Join(outputAbs, workFolder)+string(os.PathSeparator)+"%")
	})

	t.Run("rescan", func(t *testing.T) {
		// 覆盖：rescan_folder + check_rescan_status（后台 job）
		folderName := "itest_rescan_" + suffix
		folderKey := pathToFolderKey(folderName)
		w := doJSON(t, r, http.MethodPost, "/galleryout/create_folder", map[string]any{
			"parent_key":  "_root_",
			"folder_name": folderName,
		})
		requireStatus(t, w, http.StatusOK)

		fp := filepath.Join(outputAbs, folderName, "f_"+suffix+".txt")
		if err := os.WriteFile(fp, []byte("x"), 0644); err != nil {
			t.Fatalf("write rescan file: %v", err)
		}
		requireFileExists(t, fp)
		if err := scanner.UpsertFile(fp, db); err != nil {
			t.Fatalf("upsert rescan file: %v", err)
		}

		w2 := doJSON(t, r, http.MethodPost, "/galleryout/rescan_folder", map[string]any{
			"folder_key": folderKey,
			"mode":       "all",
		})
		requireStatus(t, w2, http.StatusOK)
		m := decodeJSON(t, w2.Body.Bytes())
		jobID, _ := m["job_id"].(string)
		if jobID == "" {
			if s, _ := m["status"].(string); s == "success" {
				return
			}
			t.Fatalf("missing job_id: %q", w2.Body.String())
		}
		st := waitForRescanJob(t, r, jobID)
		if s, _ := st["status"].(string); s != "done" && s != "error" {
			t.Fatalf("unexpected rescan status: %v", st)
		}

		w3 := doRequest(t, r, http.MethodPost, "/galleryout/delete_folder/"+folderKey, map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
		requireStatus(t, w3, http.StatusOK)
		requireNotExists(t, filepath.Join(outputAbs, folderName))
		_, _ = db.Exec("DELETE FROM files WHERE path LIKE ?", filepath.Join(outputAbs, folderName)+string(os.PathSeparator)+"%")
	})

	t.Run("compare_files", func(t *testing.T) {
		// 覆盖：/galleryout/api/compare_files
		idA, _ := getFileRowByName(t, db, "flux_dev_example.png")
		idB, _ := getFileRowByName(t, db, "SD1.5-Controlnet-Canny-comfyui-wiki.52a4f479.png")
		w := doJSON(t, r, http.MethodPost, "/galleryout/api/compare_files", map[string]any{
			"id_a": idA,
			"id_b": idB,
		})
		requireStatus(t, w, http.StatusOK)
		m := decodeJSON(t, w.Body.Bytes())
		if s, _ := m["status"].(string); s != "success" {
			t.Fatalf("compare status=%q body=%q", s, w.Body.String())
		}
	})

	t.Run("zip_job", func(t *testing.T) {
		// 覆盖：prepare_batch_zip + check_zip_status（后台 job），并验证 ready 后可下载 zip。
		id, _ := getFileRowByName(t, db, "flux_dev_example.png")
		w := doJSON(t, r, http.MethodPost, "/galleryout/prepare_batch_zip", map[string]any{
			"file_ids": []string{id},
		})
		requireStatus(t, w, http.StatusOK)
		m := decodeJSON(t, w.Body.Bytes())
		jobID, _ := m["job_id"].(string)
		if jobID == "" {
			t.Fatalf("missing job_id: %q", w.Body.String())
		}
		st := waitForZipJob(t, r, jobID)
		if s, _ := st["status"].(string); s == "ready" {
			filename, _ := st["filename"].(string)
			if filename == "" {
				t.Fatalf("missing filename: %v", st)
			}
			zipPath := filepath.Join(config.GetZipCacheDir(), filename)
			requireFileExists(t, zipPath)
			downloadURL, _ := st["download_url"].(string)
			if downloadURL == "" {
				t.Fatalf("missing download_url: %v", st)
			}
			w2 := doRequest(t, r, http.MethodGet, downloadURL, nil, nil)
			requireStatus(t, w2, http.StatusOK)
			if w2.Body.Len() == 0 {
				t.Fatalf("downloaded zip empty")
			}
		}
	})

	t.Run("storyboard", func(t *testing.T) {
		// 覆盖：/galleryout/storyboard（依赖 ffprobe/ffmpeg；不可用时预期 501）
		id, _ := getFileRowByName(t, db, "ComfyUI_00010_.mp4")
		ffprobe, _ := exec.LookPath("ffprobe")
		ffmpeg, _ := exec.LookPath("ffmpeg")
		w := doRequest(t, r, http.MethodGet, "/galleryout/storyboard/"+id, nil, nil)
		if ffprobe == "" || ffmpeg == "" {
			requireStatus(t, w, http.StatusNotImplemented)
			return
		}
		requireStatus(t, w, http.StatusOK)
		m := decodeJSON(t, w.Body.Bytes())
		if s, _ := m["status"].(string); s != "success" {
			t.Fatalf("storyboard status=%q body=%q", s, w.Body.String())
		}
	})
}

// min 用于失败信息截断展示（避免打印超长 HTML）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
