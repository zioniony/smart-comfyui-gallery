package scanner

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"smart-comfyui-gallery/config"
	"testing"
)

func TestGenerateThumbnail_Image(t *testing.T) {
	tmpDir := t.TempDir()
	config.Cfg.BaseSmartGalleryPath = tmpDir
	config.Cfg.ThumbnailWidth = 64

	srcPath := filepath.Join(tmpDir, "src.png")
	f, err := os.Create(srcPath)
	if err != nil {
		t.Fatalf("create src: %v", err)
	}

	img := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 100, A: 255})
		}
	}

	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		t.Fatalf("encode png: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close src: %v", err)
	}

	thumbPath, err := GenerateThumbnail(srcPath, "image")
	if err != nil {
		t.Fatalf("GenerateThumbnail: %v", err)
	}

	info, err := os.Stat(thumbPath)
	if err != nil {
		t.Fatalf("stat thumb: %v", err)
	}
	if info.Size() <= 0 {
		t.Fatalf("thumb size should be > 0, got %d", info.Size())
	}
}
