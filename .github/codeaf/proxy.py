#!/usr/bin/env python3
"""Local pricing proxy between CodeAF and synthetic.new.

CodeAF prices calls from `usage.cost` on each response, or failing that from an
OpenRouter-shaped `/models` catalog. synthetic.new provides neither in a form
CodeAF can read (its catalog prices are strings like "$0.0000005"). This proxy:

* serves GET /models from models.json, in OpenRouter's shape, so CodeAF sees
  only the allowed models and their real prices;
* forwards POST /chat/completions to synthetic.new, rewriting the model id to
  the upstream one and adding `usage.cost` (USD) to the response;
* refuses any model not in models.json, and refuses calls once the per-run or
  per-day spend cap is reached (HTTP 402), whatever CodeAF itself decides;
* appends one JSON line per call to the ledger, which the workflow reports.

Stdlib only. Configuration is by environment variable:
  SYNTHETIC_API_KEY   upstream key; if unset, read from the first line of stdin
                      (preferred: keeps the key out of /proc/<pid>/environ)
  PROXY_PORT          listen port on 127.0.0.1 (default 8787)
  PROXY_MODELS        models.json path (default: next to this file)
  PROXY_LEDGER        JSONL ledger path (default ./codeaf-ledger.jsonl)
  PROXY_RUN_CAP_USD   per-run cap (default 5)
  PROXY_DAY_CAP_USD   per-day cap (default 20)
  PROXY_DAY_SPENT_USD spend already recorded today by earlier runs (default 0)
  PROXY_UPSTREAM      upstream base (default https://api.synthetic.new/openai/v1)
"""

import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
UPSTREAM = os.environ.get("PROXY_UPSTREAM", "https://api.synthetic.new/openai/v1").rstrip("/")
API_KEY = os.environ.get("SYNTHETIC_API_KEY", "")
LEDGER = os.environ.get("PROXY_LEDGER", "codeaf-ledger.jsonl")
RUN_CAP = float(os.environ.get("PROXY_RUN_CAP_USD", "5"))
DAY_CAP = float(os.environ.get("PROXY_DAY_CAP_USD", "20"))
DAY_SPENT = float(os.environ.get("PROXY_DAY_SPENT_USD", "0") or 0)

with open(os.environ.get("PROXY_MODELS", os.path.join(HERE, "models.json"))) as f:
    MODELS = {m["id"]: m for m in json.load(f)["models"]}
UPSTREAM_TO_ID = {m["upstream"]: m["id"] for m in MODELS.values()}

lock = threading.Lock()
run_spent = 0.0


def per_token(usd_per_million):
    return "%.12f" % (usd_per_million / 1e6)


def catalog():
    rows = []
    for m in MODELS.values():
        rows.append({
            "id": m["id"],
            "canonical_slug": m["id"],
            "name": m["name"],
            "context_length": m["context_length"],
            "created": 1760000000,
            "architecture": {
                "input_modalities": m["input_modalities"],
                "output_modalities": ["text"],
            },
            "pricing": {
                "prompt": per_token(m["input"]),
                "completion": per_token(m["output"]),
                "request": "0",
                "input_cache_read": per_token(m["cache_read"]),
            },
            "supported_parameters": [
                "tools", "tool_choice", "reasoning", "include_reasoning",
                "response_format", "structured_outputs", "temperature",
                "top_p", "max_tokens", "stop", "seed",
            ],
            "reasoning": {
                "mandatory": False,
                "default_enabled": True,
                "supported_efforts": m["efforts"],
                "default_effort": m["efforts"][0],
            },
            "hugging_face_id": m["upstream"].removeprefix("hf:"),
        })
    return {"data": rows}


def resolve(model):
    """Map whatever CodeAF sends to a models.json row, or None."""
    model = (model or "").strip()
    if model.startswith("~"):
        model = model[1:]
    for suffix in (":low", ":medium", ":high", ":xhigh", ":max", ":none"):
        if model.endswith(suffix):
            model = model[: -len(suffix)]
    if model in MODELS:
        return MODELS[model]
    if model in UPSTREAM_TO_ID:
        return MODELS[UPSTREAM_TO_ID[model]]
    lowered = model.lower()
    for m in MODELS.values():
        if lowered in (m["id"].lower(), m["upstream"].lower(), m["upstream"].lower().removeprefix("hf:")):
            return m
    return None


EFFORT_ORDER = ["none", "minimal", "low", "medium", "high", "xhigh", "max"]


def translate_reasoning(body, m):
    """OpenRouter's {"reasoning": {...}} becomes OpenAI's reasoning_effort,
    clamped to the nearest effort the model supports (rounding up)."""
    reasoning = body.pop("reasoning", None)
    if not isinstance(reasoning, dict):
        return
    if reasoning.get("enabled") is False:
        wanted = "none"  # becomes the model's lowest effort if it cannot switch off
    else:
        wanted = reasoning.get("effort") or ""
    if not wanted:
        return
    supported = m["efforts"]
    if wanted in supported:
        body["reasoning_effort"] = wanted
        return
    rank = EFFORT_ORDER.index(wanted) if wanted in EFFORT_ORDER else EFFORT_ORDER.index("high")
    for effort in EFFORT_ORDER[rank:]:
        if effort in supported:
            body["reasoning_effort"] = effort
            return
    body["reasoning_effort"] = supported[-1]


def cost_of(m, usage):
    prompt = int(usage.get("prompt_tokens") or 0)
    completion = int(usage.get("completion_tokens") or 0)
    details = usage.get("prompt_tokens_details") or {}
    cached = min(int(details.get("cached_tokens") or 0), prompt)
    return ((prompt - cached) * m["input"] + cached * m["cache_read"] + completion * m["output"]) / 1e6


def record(m, usage, cost, stream, status):
    global run_spent
    with lock:
        run_spent += cost
        line = {
            "ts": time.time(), "model": m["id"], "upstream": m["upstream"],
            "prompt_tokens": usage.get("prompt_tokens", 0),
            "cached_tokens": (usage.get("prompt_tokens_details") or {}).get("cached_tokens", 0),
            "completion_tokens": usage.get("completion_tokens", 0),
            "cost_usd": round(cost, 8), "stream": stream, "status": status,
        }
        with open(LEDGER, "a") as f:
            f.write(json.dumps(line) + "\n")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("proxy: " + (fmt % args) + "\n")

    def send_json(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def error(self, status, message):
        self.send_json(status, {"error": {"message": message, "code": status}})

    def do_GET(self):
        path = self.path.split("?", 1)[0].rstrip("/")
        if path.endswith("/models"):
            return self.send_json(200, catalog())
        if path.endswith("/healthz"):
            return self.send_json(200, {"ok": True, "run_spent_usd": run_spent})
        self.error(404, "not served by codeaf proxy: " + path)

    def do_POST(self):
        path = self.path.split("?", 1)[0].rstrip("/")
        if not path.endswith("/chat/completions"):
            return self.error(404, "not served by codeaf proxy: " + path)
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        m = resolve(body.get("model"))
        if m is None:
            return self.error(400, "model %r is not allowed; allowed: %s" % (body.get("model"), ", ".join(MODELS)))
        with lock:
            if run_spent >= RUN_CAP:
                return self.error(402, "per-run spend cap $%.2f reached ($%.4f spent)" % (RUN_CAP, run_spent))
            if DAY_SPENT + run_spent >= DAY_CAP:
                return self.error(402, "daily spend cap $%.2f reached ($%.4f spent)" % (DAY_CAP, DAY_SPENT + run_spent))
        body["model"] = m["upstream"]
        translate_reasoning(body, m)
        # OpenRouter-only fields synthetic.new does not understand.
        for key in ("provider", "transforms", "route", "models", "usage", "plugins", "include_reasoning"):
            body.pop(key, None)
        stream = bool(body.get("stream"))
        if stream:
            body.setdefault("stream_options", {})["include_usage"] = True
        request = urllib.request.Request(
            UPSTREAM + "/chat/completions",
            data=json.dumps(body).encode(),
            headers={"Authorization": "Bearer " + API_KEY, "Content-Type": "application/json",
                     "Accept": "text/event-stream" if stream else "application/json"},
            method="POST",
        )
        try:
            upstream = urllib.request.urlopen(request, timeout=900)
        except urllib.error.HTTPError as err:
            data = err.read()
            self.log_message("upstream %s for %s: %s", err.code, m["upstream"], data[:300])
            self.send_response(err.code)
            self.send_header("Content-Type", err.headers.get("Content-Type", "application/json"))
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        except Exception as err:  # network failure: let CodeAF retry
            return self.error(502, "upstream unreachable: %s" % err)
        with upstream:
            if not stream:
                payload = json.loads(upstream.read())
                usage = payload.get("usage") or {}
                cost = cost_of(m, usage)
                usage["cost"] = cost
                payload["usage"] = usage
                payload["model"] = m["id"]
                record(m, usage, cost, False, 200)
                return self.send_json(200, payload)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            usage_seen = {}
            for raw in upstream:
                line = raw.decode("utf-8", "replace").rstrip("\r\n")
                if line.startswith("data:") and line[5:].strip() not in ("", "[DONE]"):
                    try:
                        chunk = json.loads(line[5:].strip())
                        chunk["model"] = m["id"]
                        if chunk.get("usage"):
                            usage_seen = chunk["usage"]
                            usage_seen["cost"] = cost_of(m, usage_seen)
                        line = "data: " + json.dumps(chunk)
                    except ValueError:
                        pass
                self.write_chunk((line + "\n").encode())
            self.write_chunk(b"")
            record(m, usage_seen, usage_seen.get("cost", 0.0), True, 200)

    def write_chunk(self, data):
        self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data))
        self.wfile.flush()


def main():
    global API_KEY
    if not API_KEY:
        API_KEY = sys.stdin.readline().strip()
    if not API_KEY:
        sys.exit("SYNTHETIC_API_KEY is not set")
    port = int(os.environ.get("PROXY_PORT", "8787"))
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    sys.stderr.write("codeaf proxy on 127.0.0.1:%d -> %s (run cap $%.2f, day cap $%.2f, day spent $%.4f)\n"
                     % (port, UPSTREAM, RUN_CAP, DAY_CAP, DAY_SPENT))
    server.serve_forever()


if __name__ == "__main__":
    main()
