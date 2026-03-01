package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"smart-comfyui-gallery/config"
	"smart-comfyui-gallery/server"
	"syscall"
	"time"
)

func main() {
	log.Println("Starting Smart ComfyUI Gallery...")

	flag.Parse()

	r, cleanup, err := server.InitApp(server.AppOptions{RootDir: ".", ScanOnStart: true})
	if err != nil {
		log.Fatalf("Failed to initialize app: %v", err)
	}
	defer func() { _ = cleanup() }()

	// 启动服务器
	serverAddr := fmt.Sprintf(":%d", config.Cfg.ServerPort)
	log.Printf("Starting server on %s", serverAddr)

	srv := &http.Server{
		Addr:              serverAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("Shutting down: %v", sig)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
