.PHONY: all fmt test build so plugin run clean

GO ?= go
BIN_DIR := bin
BIN_NAME := smart-comfyui-gallery
BIN_PATH := $(BIN_DIR)/$(BIN_NAME)
SO_NAME := smart_gallery.so
TEST_DATA_OUTPUT_DIR := test_data/output
ENV_TEST := .env.example

all: fmt test build plugin

fmt:
	$(GO) fmt ./...
	gofmt -w ./config ./database ./scanner ./server ./cshared ./web

test:
	ENV_FILE=$(ENV_TEST) $(GO) test ./...

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_PATH) .

so:
	CGO_CFLAGS='-Os -ffunction-sections -fdata-sections' CGO_LDFLAGS='-Wl,--gc-sections' $(GO) build -trimpath -buildvcs=false -buildmode=c-shared -ldflags='-s -w -extldflags "-Wl,--strip-all -Wl,--gc-sections"' -o $(SO_NAME) ./cshared
	@command -v strip >/dev/null 2>&1 && strip --strip-unneeded $(SO_NAME) || true

plugin: so

run:
	$(GO) run .

clean:
	rm -rf $(BIN_PATH)
	rm -f $(SO_NAME) smart_gallery.h
	rm -rf $(TEST_DATA_OUTPUT_DIR)/.thumbnails_cache
	rm -rf $(TEST_DATA_OUTPUT_DIR)/.sqlite_cache
	rm -rf $(TEST_DATA_OUTPUT_DIR)/.zip_downloads
