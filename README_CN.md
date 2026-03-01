# Smart ComfyUI Gallery (Go)

SmartGallery for ComfyUI 的 Golang 重构/派生实现：把 ComfyUI 的 output 目录变成快速、可搜索、移动端友好的本地 Web Gallery，并尽可能为每个图片/视频关联其生成工作流。

## 这是什么

SmartGallery 的目标是让 “输出目录” 变成 “可检索的创作记忆”：

- 全离线、本地运行：无云端、无跟踪，ComfyUI 不运行也能浏览与检索
- 工作流可追溯：查看/复制/下载生成时的工作流与关键参数，用于复现与迭代
- 面向高频迭代：快速筛选、批量管理与对比，适配桌面与手机

## 核心特性

以下特性描述参考并改写自上游项目 README，用于快速说明本项目定位：

- 搜索与过滤：按关键词、模型/LoRA、文件类型、日期范围等维度查找输出
- 工作流访问：对 PNG/JPG/WebP/WebM/MP4 等文件查看节点摘要，复制或下载工作流 JSON
- 文件管理：多选删除、移动、复制、批量重扫；支持新建/重命名文件夹
- 移动端优先：同时兼顾桌面/平板/手机的交互与性能
- 对比模式：图片/视频并排对比（缩放/旋转/参数差异等）
- 视频概览：用帧网格快速分析视频内容
- 外部文件夹挂载：把外置硬盘或网络路径挂到 Gallery 根目录统一管理
- 自动刷新：检测到新文件时自动更新（Auto-Watch）
- 跨平台：Windows/Linux/macOS/Docker

上游项目（Python 实现）与完整文档：

- https://github.com/biagiomaf/smart-comfyui-gallery

## 运行方式

本项目支持两种运行方式：

- 独立运行：启动一个 Go Web 服务（默认端口 `8189`）
- 作为 ComfyUI 插件运行（推荐）：不启动外部服务，把 Go 编译成 `smart_gallery.so`，由 ComfyUI 进程内路由调用

## 前置依赖

- Go（本项目 `go.mod` 指定版本，建议使用兼容版本）
- Linux 下需要能编译 `github.com/mattn/go-sqlite3`：通常要求系统具备 C 编译链（如 `gcc`）

## 配置

项目根目录提供示例 [.env.example](./.env.example)，常用字段：
- `BASE_OUTPUT_PATH`：ComfyUI output 目录
- `BASE_INPUT_PATH`：ComfyUI input 目录
- `SERVER_PORT`：独立运行时监听端口（默认 `8189`）
- 复制示例配置到 `.env`, 然后编辑其中的内容
```bash
cp .env.example .env
# edit .env by your own
```

## 快速开始：独立运行（外部服务）

1. 修改 `.env`，确保 `BASE_OUTPUT_PATH` / `BASE_INPUT_PATH` 指向本机 ComfyUI 目录
2. 启动：

```bash
make run
```

3. 访问：

- `http://localhost:8189/galleryout/view/_root_`

## 快速开始：作为 ComfyUI 插件运行（推荐）

该模式不会启动 `8189` 外部服务。ComfyUI 通过插件注入的 `/galleryout/*` 路由，把请求转发给 `smart_gallery.so` 在进程内处理。

### 1) 安装到 ComfyUI

把本项目放到 ComfyUI 的 `custom_nodes` 下（复制、软链均可）：

- `$COMFYUI_ROOT/custom_nodes/smart-comfyui-gallery-go/`

### 2) 构建插件 .so

在本项目根目录执行：

```bash
make plugin
```

产物：

- `smart_gallery.so`
- `smart_gallery.h`

它们会出现在插件目录（也就是 ComfyUI 能直接加载到的目录）。

### 3) 重启并验证

重启 ComfyUI 后验证：

- 直接打开 `http://localhost:8188/galleryout/view/_root_`
- 或在 ComfyUI 侧边栏点击 “Gallery”

## 常用命令

```bash
make fmt
make test
make build
make so
make plugin
make run
```

## 排错

- 访问 `/galleryout/...` 提示后端不可用：检查 `smart_gallery.so` 是否存在于插件目录，并确认 ComfyUI 已重启加载插件
- `.so` 编译失败：通常是缺少 C 编译链导致 `go-sqlite3` 无法编译
- 侧边栏页面异常：先直接访问 `http://localhost:8188/galleryout/view/_root_` 验证后端与页面是否正常，再检查前端扩展是否加载

## 项目来源与致谢

本项目为 SmartGallery for ComfyUI 的 Golang 重构/派生实现，上游项目仓库：

- https://github.com/biagiomaf/smart-comfyui-gallery

致谢与声明：

- 感谢原项目 SmartGallery for ComfyUI 及其作者（GitHub：[@biagiomaf](https://github.com/biagiomaf)）的开源贡献与持续维护
- 本项目为非官方派生实现，目标是用 Go 复刻/适配原项目的核心体验与能力
- 使用与分发请遵循原项目的 License 与相关声明；如二者存在差异，以原项目为准
