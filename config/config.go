package config

import (
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	BaseOutputPath       string
	BaseInputPath        string
	BaseSmartGalleryPath string
	FfprobeManualPath    string
	ServerPort           int
	ThumbnailWidth       int
	WebpAnimatedFps      float64
	PageSize             int
	BatchSize            int
	StreamThresholdMb    int
	MaxParallelWorkers   int
	SecretKey            string
	DeleteTo             string
}

var Cfg Config

func Load() error {
	if envPath := os.Getenv("ENV_FILE"); envPath != "" {
		return loadWithEnv(envPath)
	}
	return loadWithEnv("")
}

func LoadFrom(envPath string) error {
	return loadWithEnv(envPath)
}

func loadWithEnv(envPath string) error {
	if envPath != "" {
		if err := godotenv.Load(envPath); err != nil {
			log.Printf("Failed to load %s, using environment variables", envPath)
		}
	} else {
		if err := godotenv.Load(); err != nil {
			log.Println("No .env file found, using environment variables")
		}
	}

	baseOutputDefault := filepath.Join("test_data", "output")
	baseInputDefault := filepath.Join("test_data", "input")
	baseSmartGalleryDefault := baseOutputDefault

	// 加载配置
	Cfg = Config{
		BaseOutputPath:       getEnv("BASE_OUTPUT_PATH", baseOutputDefault),
		BaseInputPath:        getEnv("BASE_INPUT_PATH", baseInputDefault),
		BaseSmartGalleryPath: getEnv("BASE_SMARTGALLERY_PATH", getEnv("BASE_OUTPUT_PATH", baseSmartGalleryDefault)),
		FfprobeManualPath:    getEnv("FFPROBE_MANUAL_PATH", "ffprobe"),
		ServerPort:           getEnvAsInt("SERVER_PORT", 8189),
		ThumbnailWidth:       getEnvAsInt("THUMBNAIL_WIDTH", 300),
		WebpAnimatedFps:      getEnvAsFloat("WEBP_ANIMATED_FPS", 16.0),
		PageSize:             getEnvAsInt("PAGE_SIZE", 100),
		BatchSize:            getEnvAsInt("BATCH_SIZE", 500),
		StreamThresholdMb:    getEnvAsInt("STREAM_THRESHOLD_MB", 20),
		MaxParallelWorkers:   getEnvAsInt("MAX_PARALLEL_WORKERS", 0),
		SecretKey:            getEnv("SECRET_KEY", ""),
		DeleteTo:             getEnv("DELETE_TO", ""),
	}

	// 验证配置
	if Cfg.BaseOutputPath == "" {
		log.Println("WARNING: BASE_OUTPUT_PATH not set, using default")
	}

	// 确保必要的目录存在
	ensureDirectories()

	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getEnvAsFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatValue, err := strconv.ParseFloat(value, 64); err == nil {
			return floatValue
		}
	}
	return defaultValue
}

func ensureDirectories() {
	// 确保智能图库目录存在
	if err := os.MkdirAll(Cfg.BaseSmartGalleryPath, 0755); err != nil {
		log.Printf("Failed to create directory: %v", err)
	}

	// 确保缩略图缓存目录存在
	thumbnailCacheDir := GetThumbnailCacheDir()
	if err := os.MkdirAll(thumbnailCacheDir, 0755); err != nil {
		log.Printf("Failed to create thumbnail cache directory: %v", err)
	}

	// 确保SQLite缓存目录存在
	sqliteCacheDir := GetSQLiteCacheDir()
	if err := os.MkdirAll(sqliteCacheDir, 0755); err != nil {
		log.Printf("Failed to create SQLite cache directory: %v", err)
	}

	// 确保ZIP下载缓存目录存在
	zipCacheDir := GetZipCacheDir()
	if err := os.MkdirAll(zipCacheDir, 0755); err != nil {
		log.Printf("Failed to create ZIP cache directory: %v", err)
	}
}

// GetThumbnailCacheDir 返回缩略图缓存目录路径
func GetThumbnailCacheDir() string {
	return filepath.Join(Cfg.BaseSmartGalleryPath, ".thumbnails_cache")
}

// GetSQLiteCacheDir 返回SQLite缓存目录路径
func GetSQLiteCacheDir() string {
	return filepath.Join(Cfg.BaseSmartGalleryPath, ".sqlite_cache")
}

// GetDatabaseFile 返回数据库文件路径
func GetDatabaseFile() string {
	return filepath.Join(GetSQLiteCacheDir(), "gallery_cache.sqlite")
}

// GetZipCacheDir 返回ZIP下载缓存目录路径
func GetZipCacheDir() string {
	return filepath.Join(Cfg.BaseSmartGalleryPath, ".zip_downloads")
}
