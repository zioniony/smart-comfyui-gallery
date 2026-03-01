package scanner

import (
	"os"
	"testing"
)

func TestExtractWorkflow_RawScan_MP3(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "*.mp3")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	path := f.Name()

	ui := `{"nodes":[]}`
	api := `{"1":{"class_type":"KSampler"}}`
	_, _ = f.WriteString("aaa")
	_, _ = f.WriteString(ui)
	_, _ = f.WriteString("bbb")
	_, _ = f.WriteString(api)
	_, _ = f.WriteString("ccc")
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	gotUI := ExtractWorkflow(path, "ui")
	if gotUI != ui {
		t.Fatalf("ui mismatch: got %q want %q", gotUI, ui)
	}

	gotAPI := ExtractWorkflow(path, "api")
	if gotAPI != api {
		t.Fatalf("api mismatch: got %q want %q", gotAPI, api)
	}
}
