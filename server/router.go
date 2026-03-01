package server

import (
	"path/filepath"
	"sync"

	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
)

var rootDirMu sync.RWMutex
var rootDir = "."

func SetRootDir(dir string) {
	if dir == "" {
		return
	}
	rootDirMu.Lock()
	rootDir = dir
	rootDirMu.Unlock()
}

func getRootDir() string {
	rootDirMu.RLock()
	defer rootDirMu.RUnlock()
	return rootDir
}

type RouterOptions struct {
	RootDir       string
	DisableLogger bool
}

func NewRouter(opts RouterOptions) *gin.Engine {
	root := opts.RootDir
	if root == "" {
		root = "."
	}
	SetRootDir(root)

	var r *gin.Engine
	if opts.DisableLogger {
		r = gin.New()
		r.Use(gin.Recovery())
	} else {
		r = gin.Default()
	}
	r.Use(static.Serve("/static", static.LocalFile(filepath.Join(root, "static"), true)))
	r.Use(static.Serve("/galleryout/static", static.LocalFile(filepath.Join(root, "static"), true)))
	configureRoutes(r)
	return r
}
