package database

import (
	"database/sql"
	"log"
	"smart-comfyui-gallery/config"

	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB

// Init 初始化数据库连接
func Init() error {
	var err error
	db, err = sql.Open("sqlite3", config.GetDatabaseFile())
	if err != nil {
		return err
	}

	// 测试连接
	if err = db.Ping(); err != nil {
		return err
	}

	// 启用 WAL 模式
	if _, err = db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		log.Printf("Warning: Failed to set WAL mode: %v", err)
	}

	// 设置同步模式
	if _, err = db.Exec("PRAGMA synchronous=NORMAL;"); err != nil {
		log.Printf("Warning: Failed to set synchronous mode: %v", err)
	}

	// 创建表
	if err = createTables(); err != nil {
		return err
	}

	return nil
}

// createTables 创建数据库表
func createTables() error {
	// 创建文件表
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS files (
			id TEXT PRIMARY KEY, 
			path TEXT NOT NULL UNIQUE, 
			mtime REAL NOT NULL,
			name TEXT NOT NULL, 
			type TEXT, 
			duration TEXT, 
			dimensions TEXT,
			has_workflow INTEGER, 
			is_favorite INTEGER DEFAULT 0, 
			size INTEGER DEFAULT 0,
			last_scanned REAL DEFAULT 0,
			workflow_files TEXT DEFAULT '',
			workflow_prompt TEXT DEFAULT ''
		)
	`)
	if err != nil {
		return err
	}

	// 创建挂载文件夹表
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS mounted_folders (
			path TEXT PRIMARY KEY,
			target_source TEXT,
			created_at REAL
		)
	`)
	if err != nil {
		return err
	}

	return nil
}

// GetDB 返回数据库连接
func GetDB() *sql.DB {
	return db
}

// Close 关闭数据库连接
func Close() error {
	if db != nil {
		return db.Close()
	}
	return nil
}
