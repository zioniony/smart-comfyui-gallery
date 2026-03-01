package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"smart-comfyui-gallery/config"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

var indexTemplateOnce sync.Once
var indexTemplate *template.Template
var indexTemplateErr error

func tojson(v interface{}) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return template.JS("null")
	}
	return template.JS(string(b))
}

func getIndexTemplate() (*template.Template, error) {
	indexTemplateOnce.Do(func() {
		srcBytes, err := os.ReadFile(filepath.Join(getRootDir(), "static", "index.html"))
		if err != nil {
			indexTemplateErr = err
			return
		}
		src := string(srcBytes)
		if i := strings.Index(src, "<!DOCTYPE html>"); i >= 0 {
			src = src[i:]
		}
		indexTemplate, indexTemplateErr = template.New("index.html").Funcs(template.FuncMap{
			"tojson":    tojson,
			"contains":  strings.Contains,
			"htmlSafe":  func(s string) template.HTML { return template.HTML(s) },
			"jsSafe":    func(s string) template.JS { return template.JS(s) },
			"urlEscape": template.URLQueryEscaper,
		}).Parse(src)
	})
	return indexTemplate, indexTemplateErr
}

func normalizePathForTemplate(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

func buildDynamicFolderConfig(db *sql.DB) (map[string]map[string]interface{}, error) {
	baseAbs, err := filepath.Abs(config.Cfg.BaseOutputPath)
	if err != nil {
		return nil, err
	}
	baseAbs = filepath.Clean(baseAbs)
	baseNorm := normalizePathForTemplate(baseAbs)

	watched := map[string]bool{}

	folders := map[string]map[string]interface{}{}
	root := map[string]interface{}{
		"display_name":          "Main",
		"path":                  baseNorm,
		"real_path":             baseNorm,
		"relative_path":         "",
		"parent":                nil,
		"children":              []string{},
		"mtime":                 float64(time.Now().Unix()),
		"is_watched":            false,
		"is_explicitly_watched": false,
		"is_mount":              false,
	}
	if rec, ok := watched[baseNorm]; ok {
		root["is_explicitly_watched"] = true
		root["is_watched"] = true
		_ = rec
	}
	folders["_root_"] = root

	var walkDir func(absDir, relDir string) error
	walkDir = func(absDir, relDir string) error {
		items, err := os.ReadDir(absDir)
		if err != nil {
			return nil
		}
		for _, it := range items {
			if !it.IsDir() {
				continue
			}
			name := it.Name()
			if strings.HasPrefix(name, ".") || name == "__pycache__" || name == "webp_cache" || name == "hash_cache" || name == "thumbnails" {
				continue
			}
			nextRel := name
			if relDir != "" {
				nextRel = relDir + string(filepath.Separator) + name
			}
			nextAbs := filepath.Join(absDir, name)

			relSlash := filepath.ToSlash(nextRel)
			key := pathToFolderKey(relSlash)
			parentKey := "_root_"
			if relDir != "" {
				parentKey = pathToFolderKey(filepath.ToSlash(relDir))
			}

			displayName := filepath.Base(nextAbs)
			fullNorm := normalizePathForTemplate(filepath.Clean(nextAbs))
			realP := nextAbs
			if rp, err := filepath.EvalSymlinks(nextAbs); err == nil {
				realP = rp
			}
			realNorm := normalizePathForTemplate(filepath.Clean(realP))

			info, _ := it.Info()
			mtime := float64(time.Now().Unix())
			if info != nil {
				mtime = float64(info.ModTime().Unix())
			}

			isMount := false
			if !strings.Contains(relSlash, "/") {
				if fi, err := os.Lstat(nextAbs); err == nil && (fi.Mode()&os.ModeSymlink) != 0 {
					isMount = true
				}
			}

			explicitWatched := false
			isWatched := false
			if rec, ok := watched[fullNorm]; ok {
				explicitWatched = true
				isWatched = true
				_ = rec
			} else {
				for wPath, wRec := range watched {
					if !wRec {
						continue
					}
					if strings.HasPrefix(fullNorm, wPath+"/") {
						isWatched = true
						break
					}
				}
			}

			folders[key] = map[string]interface{}{
				"display_name":          displayName,
				"path":                  fullNorm,
				"real_path":             realNorm,
				"relative_path":         relSlash,
				"parent":                parentKey,
				"children":              []string{},
				"mtime":                 mtime,
				"is_watched":            isWatched,
				"is_explicitly_watched": explicitWatched,
				"is_mount":              isMount,
			}

			_ = walkDir(nextAbs, nextRel)
		}
		return nil
	}

	_ = walkDir(baseAbs, "")

	for key, info := range folders {
		if key == "_root_" {
			continue
		}
		parent, ok := info["parent"].(string)
		if !ok || parent == "" {
			parent = "_root_"
		}
		if p, ok := folders[parent]; ok {
			children, _ := p["children"].([]string)
			p["children"] = append(children, key)
		}
	}

	return folders, nil
}

func buildBreadcrumbs(folders map[string]map[string]interface{}, currentKey string) ([]map[string]interface{}, []string) {
	out := []map[string]interface{}{}
	ancestors := []string{}
	seen := map[string]bool{}

	key := currentKey
	for {
		if key == "" {
			key = "_root_"
		}
		if seen[key] {
			break
		}
		seen[key] = true

		info, ok := folders[key]
		if !ok {
			break
		}
		display, _ := info["display_name"].(string)
		out = append(out, map[string]interface{}{
			"key":          key,
			"display_name": display,
		})
		ancestors = append(ancestors, key)

		parent, _ := info["parent"].(string)
		if parent == "" || parent == "<nil>" {
			break
		}
		if parent == "_root_" {
			if key != "_root_" {
				key = "_root_"
				continue
			}
			break
		}
		key = parent
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	for i, j := 0, len(ancestors)-1; i < j; i, j = i+1, j-1 {
		ancestors[i], ancestors[j] = ancestors[j], ancestors[i]
	}
	for i := range out {
		out[i]["is_last"] = i == len(out)-1
	}
	return out, ancestors
}

func computeSearchOptions(db *sql.DB, scope, folderKey string, recursive bool) ([]string, []string, bool) {
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

	extensions := []string{}
	extRows, err := db.Query("SELECT DISTINCT LOWER(SUBSTR(name, INSTR(name, '.') + 1)) as ext FROM files"+where+" GROUP BY ext", args...)
	if err == nil {
		for extRows.Next() {
			var ext string
			if err := extRows.Scan(&ext); err == nil && isMediaExtension(ext) {
				extensions = append(extensions, ext)
			}
		}
		extRows.Close()
	}

	prefixes := []string{}
	limitReached := false
	pfxRows, err := db.Query("SELECT DISTINCT SUBSTR(name, 1, INSTR(name, '_') - 1) as prefix FROM files"+where+" AND name LIKE '%_%' AND SUBSTR(name, 1, INSTR(name, '_') - 1) != '' GROUP BY prefix LIMIT 101", args...)
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
	return extensions, prefixes, limitReached
}

func countAllDBFiles(db *sql.DB) int {
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM files").Scan(&total)
	return total
}

func countFolderFiles(db *sql.DB, folderKey string, recursive bool) int {
	p := listParams{
		FolderKey: folderKey,
		Scope:     "local",
		Recursive: recursive,
		Limit:     1,
	}
	where, args, err := buildFilesWhere(p)
	if err != nil {
		return 0
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM files"+where, args...).Scan(&total)
	return total
}

func activeFiltersCount(args map[string]string, scope string, recursive bool) int {
	count := 0
	if strings.TrimSpace(args["search"]) != "" {
		count++
	}
	if strings.TrimSpace(args["workflow_files"]) != "" {
		count++
	}
	if strings.TrimSpace(args["workflow_prompt"]) != "" {
		count++
	}
	if strings.TrimSpace(args["start_date"]) != "" {
		count++
	}
	if strings.TrimSpace(args["end_date"]) != "" {
		count++
	}
	if args["favorites"] == "true" {
		count++
	}
	if args["no_workflow"] == "true" {
		count++
	}
	if args["has_extensions"] == "true" {
		count++
	}
	if args["has_prefixes"] == "true" {
		count++
	}
	if scope == "global" {
		count++
	} else if recursive {
		count++
	}
	return count
}

func isFfprobeAvailable() bool {
	if config.Cfg.FfprobeManualPath != "" {
		if _, err := os.Stat(config.Cfg.FfprobeManualPath); err == nil {
			return true
		}
	}
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

func getIndexArgs(c *gin.Context) (map[string]string, []string, []string) {
	args := map[string]string{
		"search":          c.Query("search"),
		"workflow_files":  c.Query("workflow_files"),
		"workflow_prompt": c.Query("workflow_prompt"),
		"start_date":      c.Query("start_date"),
		"end_date":        c.Query("end_date"),
		"favorites":       c.Query("favorites"),
		"no_workflow":     c.Query("no_workflow"),
		"recursive":       c.Query("recursive"),
	}
	exts := c.QueryArray("extension")
	pfxs := c.QueryArray("prefix")
	args["has_extensions"] = "false"
	if len(exts) > 0 {
		args["has_extensions"] = "true"
	}
	args["has_prefixes"] = "false"
	if len(pfxs) > 0 {
		args["has_prefixes"] = "true"
	}
	return args, exts, pfxs
}

func renderIndexView(c *gin.Context) {
	db := getDBOr500(c)
	if db == nil {
		return
	}
	folderKey := c.Param("folder_key")
	if folderKey == "" {
		folderKey = "_root_"
	}

	folders, _ := buildDynamicFolderConfig(db)
	if _, ok := folders[folderKey]; !ok {
		folderKey = "_root_"
	}
	currentFolderInfo := folders[folderKey]

	scope := c.DefaultQuery("scope", "local")
	recursive := c.Query("recursive") == "true"
	sortBy := c.DefaultQuery("sort_by", "date")
	sortOrder := c.DefaultQuery("sort_order", "desc")

	argsMap, selectedExts, selectedPfxs := getIndexArgs(c)

	files := []map[string]interface{}{}
	totalFiles := 0
	totalFolderFiles := 0

	isGlobalSearch := scope == "global"

	p := listParams{
		FolderKey:      folderKey,
		Scope:          scope,
		Recursive:      recursive,
		Search:         argsMap["search"],
		WorkflowFiles:  argsMap["workflow_files"],
		WorkflowPrompt: argsMap["workflow_prompt"],
		StartDate:      argsMap["start_date"],
		EndDate:        argsMap["end_date"],
		Extensions:     selectedExts,
		Prefixes:       selectedPfxs,
		Favorites:      argsMap["favorites"] == "true",
		NoWorkflow:     argsMap["no_workflow"] == "true",
		SortBy:         sortBy,
		SortOrder:      sortOrder,
		Limit:          config.Cfg.PageSize,
		Offset:         0,
	}

	where, whereArgs, err := buildFilesWhere(p)
	if err == nil {
		_ = db.QueryRow("SELECT COUNT(*) FROM files"+where, whereArgs...).Scan(&totalFiles)
	}

	q, qArgs, err := buildFilesQuery(db, p)
	if err == nil {
		rows, err := db.Query(q, qArgs...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id, path, name, fileType, duration, dimensions sql.NullString
				var mtime, lastScanned sql.NullFloat64
				var hasWorkflow, isFavorite sql.NullInt64
				var size sql.NullInt64
				if err := rows.Scan(&id, &path, &mtime, &name, &fileType, &duration, &dimensions, &hasWorkflow, &isFavorite, &size, &lastScanned); err != nil {
					continue
				}
				files = append(files, map[string]interface{}{
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
				})
			}
		}
	}

	totalFolderFiles = countFolderFiles(db, folderKey, recursive)

	if totalFolderFiles == 0 && !isGlobalSearch {
		totalFolderFiles = totalFiles
	}

	breadcrumbs, ancestorKeys := buildBreadcrumbs(folders, folderKey)
	availableExts := []string{}
	availablePfxs := []string{}
	prefixLimit := false
	availableExts, availablePfxs, prefixLimit = computeSearchOptions(db, scope, folderKey, recursive)

	activeCount := activeFiltersCount(argsMap, scope, recursive)

	streamThresholdBytes := int64(config.Cfg.StreamThresholdMb) * 1024 * 1024

	data := map[string]interface{}{
		"app_logo_b64":          "/galleryout/static/assets/logo.png",
		"app_version":           "1.55",
		"github_url":            "https://github.com/EdwardDali/SmartGallery",
		"update_available":      false,
		"remote_version":        "",
		"folders":               folders,
		"current_folder_info":   currentFolderInfo,
		"current_folder_key":    folderKey,
		"breadcrumbs":           breadcrumbs,
		"ancestor_keys":         ancestorKeys,
		"protected_folder_keys": []string{"_root_"},
		"files":                 files,
		"total_files":           totalFiles,
		"total_folder_files":    totalFolderFiles,
		"total_db_files":        countAllDBFiles(db),
		"is_global_search":      isGlobalSearch,
		"active_filters_count":  activeCount,
		"current_scope":         scope,
		"ffmpeg_available":      isFfprobeAvailable(),
		"stream_threshold":      streamThresholdBytes,
		"current_sort_by":       sortBy,
		"current_sort_order":    sortOrder,
		"selected_extensions":   selectedExts,
		"selected_prefixes":     selectedPfxs,
		"available_extensions":  availableExts,
		"available_prefixes":    availablePfxs,
		"prefix_limit_reached":  prefixLimit,
		"request": map[string]interface{}{
			"args": argsMap,
		},
	}

	tpl, err := getIndexTemplate()
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
}
