import asyncio
import json
import os
import urllib.error
import urllib.parse
import urllib.request

import decky

DEFAULT_URL = "http://127.0.0.1:8737"
SETTINGS_FILE = os.path.join(decky.DECKY_PLUGIN_SETTINGS_DIR, "settings.json")


def load_settings() -> dict:
    try:
        with open(SETTINGS_FILE) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_settings(settings: dict) -> None:
    os.makedirs(os.path.dirname(SETTINGS_FILE), exist_ok=True)
    tmp = SETTINGS_FILE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(settings, f)
    os.replace(tmp, SETTINGS_FILE)


def normalize_url(url: str) -> str:
    """Accepts the proxy's base URL or the store URL set in Decky."""
    url = url.strip().rstrip("/")
    if url.endswith("/plugins"):
        url = url[: -len("/plugins")]
    parts = urllib.parse.urlsplit(url)
    if parts.scheme not in ("http", "https") or not parts.netloc:
        raise ValueError("Enter an http:// or https:// URL")
    return url


def post_refresh(base: str) -> dict:
    # Talk to the proxy directly, never through an HTTP proxy from the environment.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    req = urllib.request.Request(base + "/_proxy/refresh", method="POST")
    with opener.open(req, timeout=120) as resp:
        return json.load(resp)


class Plugin:
    async def get_proxy_url(self) -> str:
        return load_settings().get("proxy_url", DEFAULT_URL)

    async def set_proxy_url(self, url: str) -> dict:
        try:
            url = normalize_url(url)
        except ValueError as e:
            return {"ok": False, "message": str(e), "url": url}
        settings = load_settings()
        settings["proxy_url"] = url
        save_settings(settings)
        return {"ok": True, "message": "", "url": url}

    async def refresh(self) -> dict:
        base = await self.get_proxy_url()
        try:
            status = await asyncio.to_thread(post_refresh, base)
        except urllib.error.HTTPError as e:
            return {"ok": False, "message": f"{base} answered HTTP {e.code}"}
        except (urllib.error.URLError, OSError, ValueError) as e:
            reason = getattr(e, "reason", e)
            return {"ok": False, "message": f"Can't reach {base}: {reason}"}
        plugins = len(status.get("plugins") or [])
        errors = status.get("errors") or []
        message = f"{plugins} plugin{'s' if plugins != 1 else ''} from your sources"
        if errors:
            decky.logger.warning("refresh errors: %s", errors)
            message += f", {len(errors)} error{'s' if len(errors) != 1 else ''}: {errors[0]}"
        return {"ok": not errors, "message": message}

    async def _main(self):
        decky.logger.info("Store Proxy loaded")

    async def _unload(self):
        pass
