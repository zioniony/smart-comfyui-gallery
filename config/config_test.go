package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_UsesENV_FILE(t *testing.T) {
	oldPort, hadPort := os.LookupEnv("SERVER_PORT")
	_ = os.Unsetenv("SERVER_PORT")
	t.Cleanup(func() {
		if hadPort {
			_ = os.Setenv("SERVER_PORT", oldPort)
			return
		}
		_ = os.Unsetenv("SERVER_PORT")
	})

	envPath := filepath.Join(t.TempDir(), ".env.test")
	if err := os.WriteFile(envPath, []byte("SERVER_PORT=1234\n"), 0644); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	t.Setenv("ENV_FILE", envPath)

	if err := Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if Cfg.ServerPort != 1234 {
		t.Fatalf("Cfg.ServerPort = %d, want %d", Cfg.ServerPort, 1234)
	}
}

func TestLoad_ENVWinsOverENV_FILE(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env.test")
	if err := os.WriteFile(envPath, []byte("SERVER_PORT=1234\n"), 0644); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	t.Setenv("ENV_FILE", envPath)
	t.Setenv("SERVER_PORT", "9999")

	if err := Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if Cfg.ServerPort != 9999 {
		t.Fatalf("Cfg.ServerPort = %d, want %d", Cfg.ServerPort, 9999)
	}
}
