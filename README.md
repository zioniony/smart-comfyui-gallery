# Smart ComfyUI Gallery (Go)

A Golang rewrite/fork of SmartGallery for ComfyUI: it turns your ComfyUI output folder into a fast, searchable, mobile-friendly local web gallery, and tries to keep each image/video linked to its generating workflow.

## What is this

SmartGallery’s goal is to turn an “output folder” into a “searchable memory of your creative process”:

- Fully local & offline: no cloud, no tracking; browse/search even when ComfyUI is not running
- Workflow traceability: view/copy/download the workflow and key parameters used to generate a file
- Built for iteration: fast filtering, batch operations, and comparisons; works well on desktop and mobile

## Core features

The feature list below is adapted from the upstream README to describe this project’s positioning:

- Search & Filter: find outputs by keywords, model/LoRA, file type, date range, and more
- Full workflow access: node summary and workflow JSON for PNG/JPG/WebP/WebM/MP4 outputs
- File management: multi-select delete/move/copy/bulk rescan; create/rename folders
- Mobile-first UX: optimized for desktop/tablet/phone
- Compare mode: side-by-side comparison for images/videos (zoom/rotate/parameter diff, etc.)
- Video overview: analyze videos with a frame grid
- External folder linking: mount external drives or network paths into the gallery root
- Auto-watch: refresh automatically when new files are detected
- Cross-platform: Windows/Linux/macOS/Docker

Upstream project (Python implementation) and full documentation:

- https://github.com/biagiomaf/smart-comfyui-gallery

## Run modes

This project supports two modes:

- Standalone server: runs a Go web server (default port `8189`)
- ComfyUI plugin (recommended): no external server; build `smart_gallery.so` and let ComfyUI route requests in-process

## Prerequisites

- Go (use a version compatible with this repository’s `go.mod`)
- On Linux, building `github.com/mattn/go-sqlite3` typically requires a C toolchain (e.g. `gcc`)

## Configuration

The repository provides an example env file [.env.example](./.env.example). Common fields:

- `BASE_OUTPUT_PATH`: ComfyUI output directory
- `BASE_INPUT_PATH`: ComfyUI input directory
- `SERVER_PORT`: listen port for standalone mode (default `8189`)

Copy the example file to `.env` and edit it:

```bash
cp .env.example .env
# edit .env by your own
```

## Quick start: standalone server

1. Edit `.env` and make sure `BASE_OUTPUT_PATH` / `BASE_INPUT_PATH` point to your ComfyUI folders
2. Start:

```bash
make run
```

3. Open:

- `http://localhost:8189/galleryout/view/_root_`

## Quick start: ComfyUI plugin (recommended)

This mode does not start the external `8189` server. ComfyUI registers `/galleryout/*` routes and forwards requests to `smart_gallery.so` in-process.

### 1) Install into ComfyUI

Place this project under ComfyUI’s `custom_nodes` (copy or symlink):

- `$COMFYUI_ROOT/custom_nodes/smart-comfyui-gallery-go/`

### 2) Build the plugin .so

From this repository root:

```bash
make plugin
```

Artifacts:

- `smart_gallery.so`
- `smart_gallery.h`

They will appear in the plugin directory (so ComfyUI can load them directly).

### 3) Restart and verify

After restarting ComfyUI:

- Open `http://localhost:8188/galleryout/view/_root_`
- Or click “Gallery” in the ComfyUI sidebar

## Common commands

```bash
make fmt
make test
make build
make so
make plugin
make run
```

## Troubleshooting

- `/galleryout/...` says backend unavailable: ensure `smart_gallery.so` exists in the plugin directory and restart ComfyUI
- `.so` build fails: typically missing C toolchain required by `go-sqlite3`
- Sidebar page looks broken: first open `http://localhost:8188/galleryout/view/_root_` to validate backend and UI, then check whether the frontend extension is loaded

## Upstream and credits

This repository is a Golang rewrite/fork of SmartGallery for ComfyUI. Upstream project:

- https://github.com/biagiomaf/smart-comfyui-gallery

Thanks and notice:

- Thanks to the upstream SmartGallery for ComfyUI project and its author (GitHub: [@biagiomaf](https://github.com/biagiomaf)) for the open-source work and continued maintenance
- This is an unofficial fork/rewrite aiming to replicate/adapt the upstream experience in Go
- Usage and redistribution must follow the upstream project’s license and statements; if anything differs, defer to the upstream project
