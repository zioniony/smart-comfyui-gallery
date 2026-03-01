# Set the web directory, any .js file in that directory will be loaded by the frontend as a frontend extension
WEB_DIRECTORY = "./web"

# Empty mappings since this extension doesn't define any nodes
NODE_CLASS_MAPPINGS = {}
NODE_DISPLAY_NAME_MAPPINGS = {}

import base64
import ctypes
import json
import logging
from pathlib import Path

from aiohttp import web
from multidict import CIMultiDict
from server import PromptServer

LOGGER = logging.getLogger("smart_gallery")
EXT_ROOT = Path(__file__).resolve().parent
SO_PATH = EXT_ROOT / "smart_gallery.so"
ENV_PATH = EXT_ROOT / ".env"

_LIB = None


def _load_lib():
    global _LIB
    if _LIB is not None:
        return _LIB
    if not SO_PATH.exists():
        raise FileNotFoundError(f"missing {SO_PATH}")

    lib = ctypes.CDLL(str(SO_PATH))
    lib.Init.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
    lib.Init.restype = ctypes.c_void_p
    lib.HandleRequest.argtypes = [ctypes.c_char_p]
    lib.HandleRequest.restype = ctypes.c_void_p
    lib.FreeCString.argtypes = [ctypes.c_void_p]
    lib.FreeCString.restype = None

    env_path = str(ENV_PATH).encode("utf-8") if ENV_PATH.exists() else b""
    err_ptr = lib.Init(str(EXT_ROOT).encode("utf-8"), env_path)
    if err_ptr:
        err_msg = ctypes.string_at(err_ptr).decode("utf-8", "replace")
        lib.FreeCString(err_ptr)
        raise RuntimeError(err_msg)

    _LIB = lib
    return lib


_HOP_BY_HOP_HEADERS = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
    "content-length",
}


def _build_headers(raw):
    headers = CIMultiDict()
    for k, vs in (raw or {}).items():
        if not k:
            continue
        if k.lower() in _HOP_BY_HOP_HEADERS:
            continue
        if isinstance(vs, str):
            headers.add(k, vs)
            continue
        for v in vs or []:
            if v is None:
                continue
            headers.add(k, str(v))
    return headers


async def _proxy_to_so(request: web.Request) -> web.StreamResponse:
    try:
        lib = _load_lib()
    except Exception as e:
        LOGGER.exception("SmartGallery .so init failed: %s", e)
        raise web.HTTPServiceUnavailable(text="SmartGallery backend unavailable")

    body = await request.read()
    hdrs = {}
    for k, v in request.headers.items():
        hdrs.setdefault(k, []).append(v)

    payload = {
        "method": request.method,
        "path": request.path,
        "raw_query": request.rel_url.query_string,
        "headers": hdrs,
        "body_base64": base64.b64encode(body).decode("ascii"),
    }

    req_bytes = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    resp_ptr = lib.HandleRequest(req_bytes)
    if not resp_ptr:
        raise web.HTTPInternalServerError(text="SmartGallery backend failed")

    try:
        resp_json = ctypes.string_at(resp_ptr).decode("utf-8", "replace")
    finally:
        lib.FreeCString(resp_ptr)

    try:
        resp = json.loads(resp_json)
    except json.JSONDecodeError:
        raise web.HTTPInternalServerError(text="SmartGallery invalid response")

    status = int(resp.get("status") or 500)
    mode = resp.get("mode") or "bytes"
    headers = _build_headers(resp.get("headers"))

    if mode == "file":
        if status == 304 or request.method == "HEAD":
            return web.Response(status=status, headers=headers)
        file_path = resp.get("file_path")
        if not file_path:
            return web.Response(status=status, headers=headers)
        try:
            return web.FileResponse(path=file_path, status=status, headers=headers)
        except Exception:
            return web.Response(status=404, text="File not found")

    body_b64 = resp.get("body_base64") or ""
    try:
        out = base64.b64decode(body_b64) if body_b64 else b""
    except Exception:
        out = b""
    return web.Response(body=out, status=status, headers=headers)


@PromptServer.instance.routes.route("*", "/galleryout")
async def _smart_gallery_galleryout_root(request: web.Request) -> web.StreamResponse:
    return await _proxy_to_so(request)


@PromptServer.instance.routes.route("*", "/galleryout/{tail:.*}")
async def _smart_gallery_galleryout_all(request: web.Request) -> web.StreamResponse:
    return await _proxy_to_so(request)
