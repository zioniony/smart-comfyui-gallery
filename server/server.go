package server

import (
	"net/http"
	"smart-comfyui-gallery/config"
	"smart-comfyui-gallery/database"
	"smart-comfyui-gallery/scanner"

	"github.com/gin-gonic/gin"
)

type AppOptions struct {
	RootDir        string
	EnvFile        string
	GinMode        string
	DisableLogger  bool
	ScanOnStart    bool
	ScanAsyncStart bool
}

func InitApp(opts AppOptions) (*gin.Engine, func() error, error) {
	if opts.GinMode != "" {
		gin.SetMode(opts.GinMode)
	}

	var err error
	if opts.EnvFile != "" {
		err = config.LoadFrom(opts.EnvFile)
	} else {
		err = config.Load()
	}
	if err != nil {
		return nil, nil, err
	}

	if err := database.Init(); err != nil {
		return nil, nil, err
	}

	if opts.ScanOnStart {
		if opts.ScanAsyncStart {
			go func() { _ = scanner.ScanDirectory() }()
		} else {
			_ = scanner.ScanDirectory()
		}
	}

	r := NewRouter(RouterOptions{
		RootDir:       opts.RootDir,
		DisableLogger: opts.DisableLogger,
	})
	return r, database.Close, nil
}

// configureRoutes 配置路由
func configureRoutes(r *gin.Engine) {
	// 主页面
	r.GET("/galleryout", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/galleryout/view/_root_")
	})
	r.GET("/galleryout/", func(c *gin.Context) {
		c.Redirect(302, "/galleryout/view/_root_")
	})
	r.GET("/galleryout/view/:folder_key", renderIndexView)

	// API 路由组
	api := r.Group("/api")
	{
		// 文件相关路由
		api.GET("/files", getFiles)
		api.GET("/files/:id", getFile)
		api.POST("/files/delete", deleteFiles)
		api.POST("/files/favorite", toggleFavorite)
		api.POST("/files/move", moveFiles)
		api.POST("/files/copy", copyFiles)
		api.POST("/files/rename", renameFile)

		// 文件夹相关路由
		api.GET("/folders", getFolders)
		api.POST("/folders/create", createFolder)
		api.POST("/folders/rename", renameFolder)

		// 工作流相关路由
		api.GET("/workflow/:id", getWorkflow)

		// 搜索相关路由
		api.GET("/search", searchFiles)
		api.GET("/search_options", searchOptions)

		// 配置相关路由
		api.GET("/config", getConfig)
	}

	// 静态文件服务
	r.GET("/galleryout/input_file/*path", serveInputFile)
	r.GET("/galleryout/output_file/*path", serveOutputFile)
	r.GET("/galleryout/thumbnail/:path", serveThumbnail)

	gallery := r.Group("/galleryout")
	{
		gallery.GET("/sync_status/:folder_key", syncStatus)

		gallery.GET("/file/:file_id", serveFileByID)
		gallery.GET("/download/:file_id", downloadFileByID)
		gallery.GET("/workflow/:file_id", downloadWorkflowByID)
		gallery.GET("/node_summary/:file_id", nodeSummaryByID)
		gallery.GET("/check_metadata/:file_id", checkMetadataByID)

		gallery.GET("/stream/:file_id", streamVideoByID)

		gallery.GET("/storyboard/:file_id", getStoryboardByID)
		gallery.GET("/storyboard_frame/:file_hash/:filename", serveStoryboardFrame)

		gallery.POST("/upload", uploadFiles)
		gallery.GET("/load_more", loadMoreFiles)

		gallery.POST("/prepare_batch_zip", prepareBatchZip)
		gallery.GET("/check_zip_status/:job_id", checkZipStatus)
		gallery.GET("/serve_zip/:filename", serveZipFile)

		gallery.POST("/create_folder", createFolderLegacy)
		gallery.POST("/rename_folder/:folder_key", renameFolderLegacy)
		gallery.POST("/delete_folder/:folder_key", deleteFolderLegacy)

		gallery.POST("/mount_folder", mountFolder)
		gallery.POST("/unmount_folder", unmountFolder)

		gallery.POST("/rescan_folder", rescanFolder)
		gallery.GET("/check_rescan_status/:job_id", checkRescanStatus)

		gallery.POST("/move_batch", moveBatch)
		gallery.POST("/copy_batch", copyBatch)
		gallery.POST("/delete_batch", deleteBatchLegacy)
		gallery.POST("/favorite_batch", favoriteBatchLegacy)

		gallery.POST("/toggle_favorite/:file_id", toggleFavoriteLegacy)
		gallery.POST("/delete/:file_id", deleteFileLegacy)
		gallery.POST("/rename_file/:file_id", renameFileLegacy)

		legacyAPI := gallery.Group("/api")
		{
			legacyAPI.GET("/search_options", legacySearchOptions)
			legacyAPI.POST("/browse_filesystem", browseFilesystem)
			legacyAPI.POST("/compare_files", compareFiles)
		}
	}

	// 根路径重定向到galleryout
	r.GET("/", func(c *gin.Context) {
		c.Redirect(302, "/galleryout")
	})
}
