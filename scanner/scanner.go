package scanner

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"smart-comfyui-gallery/config"
	"smart-comfyui-gallery/database"
	"strings"
	"time"

	"github.com/disintegration/imaging"
)

// ScanDirectory 扫描目录并将文件信息存储到数据库
func ScanDirectory() error {
	db := database.GetDB()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}

	// 扫描基础输出目录
	return scanDir(config.Cfg.BaseOutputPath, db)
}

// scanDir 递归扫描目录
func scanDir(dirPath string, db *sql.DB) error {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		fullPath := filepath.Join(dirPath, entry.Name())

		// 跳过隐藏文件和目录
		if entry.Name()[0] == '.' {
			continue
		}

		// 跳过系统目录
		if entry.IsDir() {
			dirName := entry.Name()
			if dirName == ".thumbnails_cache" || dirName == ".sqlite_cache" || dirName == ".zip_downloads" {
				continue
			}
			// 递归扫描子目录
			if err := scanDir(fullPath, db); err != nil {
				log.Printf("Error scanning directory %s: %v", fullPath, err)
			}
			continue
		}

		// 处理文件
		if err := processFile(fullPath, db); err != nil {
			log.Printf("Error processing file %s: %v", fullPath, err)
		}
	}

	return nil
}

func normalizePath(filePath string) (string, error) {
	if filePath == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(filePath)
	if err != nil {
		abs = filePath
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != "" {
		return filepath.Clean(real), nil
	}
	return abs, nil
}

func NormalizePath(filePath string) (string, error) {
	return normalizePath(filePath)
}

// processFile 处理单个文件
func processFile(filePath string, db *sql.DB) error {
	normPath, err := normalizePath(filePath)
	if err != nil {
		return err
	}
	// 获取文件信息
	fileInfo, err := os.Stat(normPath)
	if err != nil {
		return err
	}

	// 计算文件ID
	fileID := generateFileID(normPath)

	// 检查文件是否已经在数据库中
	var existingMtime float64
	err = db.QueryRow("SELECT mtime FROM files WHERE path = ? LIMIT 1", normPath).Scan(&existingMtime)
	if err == nil {
		// 文件已存在，检查修改时间
		if float64(fileInfo.ModTime().Unix()) <= existingMtime {
			// 文件未修改，跳过
			return nil
		}
	}

	// 分析文件元数据
	metadata := analyzeFileMetadata(normPath)

	// 提取工作流信息
	workflowFiles, workflowPrompt := extractWorkflowInfo(normPath)

	// 执行SQL语句
	mtime := float64(fileInfo.ModTime().Unix())
	lastScanned := float64(time.Now().Unix())
	size := fileInfo.Size()
	hasWorkflow := 0
	if metadata.HasWorkflow {
		hasWorkflow = 1
	}

	_, err = db.Exec(`
		INSERT INTO files (
			id, path, mtime, name, type, duration, dimensions, 
			has_workflow, is_favorite, size, last_scanned, workflow_files, workflow_prompt
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			id = excluded.id,
			mtime = excluded.mtime,
			name = excluded.name,
			type = excluded.type,
			duration = excluded.duration,
			dimensions = excluded.dimensions,
			has_workflow = excluded.has_workflow,
			is_favorite = excluded.is_favorite,
			size = excluded.size,
			last_scanned = excluded.last_scanned,
			workflow_files = excluded.workflow_files,
			workflow_prompt = excluded.workflow_prompt
	`, fileID, normPath, mtime, fileInfo.Name(), metadata.Type, metadata.Duration, metadata.Dimensions,
		hasWorkflow, 0, size, lastScanned, workflowFiles, workflowPrompt)
	return err
}

func UpsertFile(filePath string, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("database not initialized")
	}
	return processFile(filePath, db)
}

// generateFileID 生成文件ID
func generateFileID(filePath string) string {
	hash := md5.New()
	hash.Write([]byte(filePath))
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// FileMetadata 文件元数据
type FileMetadata struct {
	Type        string
	Duration    string
	Dimensions  string
	HasWorkflow bool
}

// analyzeFileMetadata 分析文件元数据
func analyzeFileMetadata(filePath string) FileMetadata {
	ext := strings.ToLower(filepath.Ext(filePath))
	metadata := FileMetadata{
		Type:        "unknown",
		Duration:    "",
		Dimensions:  "",
		HasWorkflow: false,
	}

	// 根据文件扩展名确定类型
	switch ext {
	case ".png", ".jpg", ".jpeg", ".bmp", ".tiff", ".tif":
		metadata.Type = "image"
		metadata.Dimensions = getImageDimensions(filePath)
	case ".gif":
		metadata.Type = "animated_image"
		metadata.Dimensions = getImageDimensions(filePath)
	case ".webp":
		if isWebPAnimated(filePath) {
			metadata.Type = "animated_image"
		} else {
			metadata.Type = "image"
		}
		metadata.Dimensions = getImageDimensions(filePath)
	case ".mp4", ".webm", ".mov", ".mkv", ".avi", ".m4v", ".wmv", ".flv":
		metadata.Type = "video"
		// TODO: Implement video metadata extraction
	case ".mp3", ".wav", ".ogg", ".flac", ".m4a":
		metadata.Type = "audio"
		// TODO: Implement audio metadata extraction
	}

	// 检查是否有工作流信息
	metadata.HasWorkflow = hasWorkflow(filePath)

	return metadata
}

// getImageDimensions 获取图像尺寸
func getImageDimensions(filePath string) string {
	file, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer file.Close()

	img, _, err := image.Decode(file)
	if err != nil {
		return ""
	}

	bounds := img.Bounds()
	return fmt.Sprintf("%dx%d", bounds.Dx(), bounds.Dy())
}

// isWebPAnimated 检查WebP是否为动画
func isWebPAnimated(filePath string) bool {
	// TODO: Implement WebP animation detection
	return false
}

// hasWorkflow 检查文件是否包含工作流信息
func hasWorkflow(filePath string) bool {
	// 检查文件是否包含工作流信息
	workflow := extractWorkflow(filePath, "ui")
	return workflow != ""
}

// ExtractWorkflow 从文件中提取工作流（对外暴露的函数）
func ExtractWorkflow(filePath, targetType string) string {
	return extractWorkflow(filePath, targetType)
}

// extractWorkflow 从文件中提取工作流
func extractWorkflow(filePath, targetType string) string {
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".png":
		return extractWorkflowFromImageFile(filePath, targetType, extractPNGWorkflowCandidates)
	case ".jpg", ".jpeg":
		return extractWorkflowFromImageFile(filePath, targetType, nil)
	case ".webp":
		return extractWorkflowFromImageFile(filePath, targetType, nil)
	case ".mp4", ".mkv", ".webm", ".mov", ".avi":
		return extractWorkflowFromVideo(filePath, targetType)
	default:
		return extractWorkflowFromRawFile(filePath, targetType)
	}
}

func extractWorkflowFromImageFile(filePath, targetType string, candidatesFn func(string) []string) string {
	found := map[string]string{}

	analyze := func(jsonStr string) {
		wf, wfType := validateAndGetWorkflow(jsonStr)
		if wf == "" || wfType == "" {
			return
		}
		if _, ok := found[wfType]; !ok {
			found[wfType] = wf
		}
	}

	if candidatesFn != nil {
		for _, cand := range candidatesFn(filePath) {
			analyze(cand)
			if v, ok := found[targetType]; ok {
				return v
			}
		}
	}

	if len(found) == 0 {
		content, err := os.ReadFile(filePath)
		if err == nil {
			for _, cand := range scanBytesForJSONCandidates(content) {
				analyze(cand)
				if v, ok := found[targetType]; ok {
					return v
				}
			}
		}
	}

	if v, ok := found[targetType]; ok {
		return v
	}
	for _, v := range found {
		return v
	}
	return ""
}

func extractWorkflowFromRawFile(filePath, targetType string) string {
	found := map[string]string{}

	analyze := func(jsonStr string) {
		wf, wfType := validateAndGetWorkflow(jsonStr)
		if wf == "" || wfType == "" {
			return
		}
		if _, ok := found[wfType]; !ok {
			found[wfType] = wf
		}
	}

	content, err := os.ReadFile(filePath)
	if err == nil {
		for _, cand := range scanBytesForJSONCandidates(content) {
			analyze(cand)
			if v, ok := found[targetType]; ok {
				return v
			}
		}
	}

	if v, ok := found[targetType]; ok {
		return v
	}
	for _, v := range found {
		return v
	}
	return ""
}

func validateAndGetWorkflow(jsonString string) (string, string) {
	jsonString = strings.TrimSpace(jsonString)
	if jsonString == "" {
		return "", ""
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonString), &top); err != nil {
		return "", ""
	}

	var workflowRaw json.RawMessage
	if v, ok := top["workflow"]; ok && len(v) > 0 {
		workflowRaw = v
	} else if v, ok := top["prompt"]; ok && len(v) > 0 {
		workflowRaw = v
	} else {
		workflowRaw = json.RawMessage(jsonString)
	}

	var wfMap map[string]json.RawMessage
	if err := json.Unmarshal(workflowRaw, &wfMap); err != nil {
		return "", ""
	}

	if _, ok := wfMap["nodes"]; ok {
		return string(workflowRaw), "ui"
	}

	for _, v := range wfMap {
		var vm map[string]json.RawMessage
		if json.Unmarshal(v, &vm) == nil {
			if _, ok2 := vm["class_type"]; ok2 {
				return string(workflowRaw), "api"
			}
		}
	}

	return "", ""
}

func scanBytesForJSONCandidates(content []byte) []string {
	stream := bytes.ToValidUTF8(content, []byte{})
	s := string(stream)
	out := make([]string, 0, 4)

	startPos := 0
	for {
		if startPos >= len(s) {
			break
		}
		first := strings.IndexByte(s[startPos:], '{')
		if first == -1 {
			break
		}
		start := startPos + first
		open := 0
		for i := start; i < len(s); i++ {
			switch s[i] {
			case '{':
				open++
			case '}':
				open--
				if open == 0 {
					cand := s[start : i+1]
					var tmp interface{}
					if json.Unmarshal([]byte(cand), &tmp) == nil {
						out = append(out, cand)
					}
					startPos = i + 1
					goto next
				}
			}
		}
		break
	next:
	}

	return out
}

func extractPNGWorkflowCandidates(filePath string) []string {
	file, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer file.Close()

	header := make([]byte, 8)
	_, err = io.ReadFull(file, header)
	if err != nil {
		return nil
	}

	if string(header) != "\x89PNG\r\n\x1a\n" {
		return nil
	}

	candidates := make([]string, 0, 2)
	for {
		lengthBytes := make([]byte, 4)
		_, err = io.ReadFull(file, lengthBytes)
		if err != nil {
			break
		}
		length := int(binary.BigEndian.Uint32(lengthBytes))

		typeBytes := make([]byte, 4)
		_, err = io.ReadFull(file, typeBytes)
		if err != nil {
			break
		}
		chunkType := string(typeBytes)

		data := make([]byte, length)
		_, err = io.ReadFull(file, data)
		if err != nil {
			break
		}

		switch chunkType {
		case "tEXt":
			parts := bytes.SplitN(data, []byte{0x00}, 2)
			if len(parts) == 2 {
				keyword := string(parts[0])
				text := string(parts[1])
				if keyword == "workflow" || keyword == "prompt" || strings.Contains(keyword, "workflow") || strings.Contains(keyword, "Workflow") {
					candidates = append(candidates, text)
				}
			}
		case "zTXt":
			parts := bytes.SplitN(data, []byte{0x00}, 2)
			if len(parts) == 2 && len(parts[1]) >= 1 {
				keyword := string(parts[0])
				if keyword == "workflow" || keyword == "prompt" || strings.Contains(keyword, "workflow") || strings.Contains(keyword, "Workflow") {
					comp := parts[1][1:]
					zr, err := zlib.NewReader(bytes.NewReader(comp))
					if err == nil {
						b, _ := io.ReadAll(zr)
						_ = zr.Close()
						if len(b) > 0 {
							candidates = append(candidates, string(b))
						}
					}
				}
			}
		case "iTXt":
			n1 := bytes.IndexByte(data, 0x00)
			if n1 != -1 && n1+2 < len(data) {
				keyword := string(data[:n1])
				compFlag := data[n1+1]
				compMethod := data[n1+2]
				pos := n1 + 3
				n2 := bytes.IndexByte(data[pos:], 0x00)
				if n2 != -1 {
					pos += n2 + 1
					n3 := bytes.IndexByte(data[pos:], 0x00)
					if n3 != -1 {
						pos += n3 + 1
						textBytes := data[pos:]
						if keyword == "workflow" || keyword == "prompt" || strings.Contains(keyword, "workflow") || strings.Contains(keyword, "Workflow") {
							if compFlag == 1 && compMethod == 0 {
								zr, err := zlib.NewReader(bytes.NewReader(textBytes))
								if err == nil {
									b, _ := io.ReadAll(zr)
									_ = zr.Close()
									if len(b) > 0 {
										candidates = append(candidates, string(b))
									}
								}
							} else {
								candidates = append(candidates, string(textBytes))
							}
						}
					}
				}
			}
		}

		_, err = file.Seek(4, io.SeekCurrent)
		if err != nil {
			break
		}
	}

	return candidates
}

// extractWorkflowFromVideo 从视频文件中提取工作流
func extractWorkflowFromVideo(filePath, targetType string) string {
	found := map[string]string{}
	analyze := func(jsonStr string) {
		wf, wfType := validateAndGetWorkflow(jsonStr)
		if wf == "" || wfType == "" {
			return
		}
		if _, ok := found[wfType]; !ok {
			found[wfType] = wf
		}
	}

	// 使用ffprobe提取视频元数据
	ffprobePath := config.Cfg.FfprobeManualPath
	if ffprobePath == "" {
		// 尝试在PATH中查找ffprobe
		ffprobePath = "ffprobe"
	}

	// 构建ffprobe命令
	cmd := exec.Command(ffprobePath, "-v", "quiet", "-print_format", "json", "-show_format", filePath)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	// 解析ffprobe输出
	var result struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return ""
	}

	// 搜索工作流信息
	for _, value := range result.Format.Tags {
		if value == "" {
			continue
		}
		for _, cand := range scanBytesForJSONCandidates([]byte(value)) {
			analyze(cand)
			if v, ok := found[targetType]; ok {
				return v
			}
		}
	}

	if v, ok := found[targetType]; ok {
		return v
	}
	for _, v := range found {
		return v
	}
	return ""
}

// extractWorkflowInfo 提取工作流信息
func extractWorkflowInfo(filePath string) (string, string) {
	workflow := extractWorkflow(filePath, "api")
	if workflow == "" {
		return "", ""
	}

	// 提取工作流文件和提示词
	workflowFiles := extractWorkflowFiles(workflow)
	workflowPrompt := extractWorkflowPrompt(workflow)

	return workflowFiles, workflowPrompt
}

// extractWorkflowFiles 从工作流中提取文件信息
func extractWorkflowFiles(workflowJSON string) string {
	// TODO: Implement workflow files extraction
	return ""
}

// extractWorkflowPrompt 从工作流中提取提示词
func extractWorkflowPrompt(workflowJSON string) string {
	// TODO: Implement workflow prompt extraction
	return ""
}

// GenerateThumbnail 生成文件缩略图
func GenerateThumbnail(filePath string, fileType string) (string, error) {
	// 计算文件哈希值
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return "", err
	}

	fileHash := generateFileID(filePath + fmt.Sprintf("%d", fileInfo.ModTime().Unix()))
	thumbnailDir := config.GetThumbnailCacheDir()

	// 检查是否已存在缩略图
	extension := ".jpg"
	if fileType == "animated_image" {
		extension = ".gif"
	}

	thumbnailPath := filepath.Join(thumbnailDir, fileHash+extension)
	if _, err := os.Stat(thumbnailPath); err == nil {
		return thumbnailPath, nil
	}

	// 确保缩略图目录存在
	if err := os.MkdirAll(thumbnailDir, 0755); err != nil {
		return "", err
	}

	// 生成缩略图
	switch fileType {
	case "video":
		ffprobePath := config.Cfg.FfprobeManualPath
		ffmpegName := "ffmpeg"
		if strings.HasSuffix(strings.ToLower(ffprobePath), "ffprobe.exe") {
			ffmpegName = "ffmpeg.exe"
		}

		ffmpegPath := ffmpegName
		if ffprobePath != "" {
			candidate := filepath.Join(filepath.Dir(ffprobePath), ffmpegName)
			if _, err := os.Stat(candidate); err == nil {
				ffmpegPath = candidate
			}
		}

		vf := fmt.Sprintf("scale=%d:-1", config.Cfg.ThumbnailWidth)
		cmd := exec.Command(ffmpegPath, "-y", "-i", filePath, "-ss", "00:00:00", "-vframes", "1", "-vf", vf, "-q:v", "2", thumbnailPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("ffmpeg thumbnail failed: %w: %s", err, strings.TrimSpace(string(output)))
		}

		if _, err := os.Stat(thumbnailPath); err != nil {
			return "", err
		}
	case "image", "animated_image":
		src, err := imaging.Open(filePath)
		if err != nil {
			return "", err
		}

		thumbnail := imaging.Resize(src, config.Cfg.ThumbnailWidth, 0, imaging.Lanczos)
		if err := imaging.Save(thumbnail, thumbnailPath); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported thumbnail type: %s", fileType)
	}

	return thumbnailPath, nil
}

// GenerateFileID 生成文件ID
func GenerateFileID(filePath string) string {
	norm, err := normalizePath(filePath)
	if err != nil {
		return generateFileID(filePath)
	}
	return generateFileID(norm)
}
