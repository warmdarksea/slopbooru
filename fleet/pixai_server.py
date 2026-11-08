#!/usr/bin/env python3
"""pixai-tagger-v1.0 behind the OpenAI chat-completions API.

    pixai_server.py --model DIR (--uds PATH | --port N) [--host 127.0.0.1]
                    [--device auto|cpu|cuda] [--batch 16] [--name pixai]
                    [--threshold-general X] [--threshold-character X]

Loads torch and the model, then listens; GET /v1/models answers once it
can tag (gpu-timeshare's ready check). POST /v1/chat/completions takes the
first image in the messages, as a data: URL (a picture travels in the
request, never as a path), and answers a chat completion whose message
content is the tagger's contribution as JSON:

    {"model": "pixai", "success": true,
     "fields": {"tags": [...], "subjects": [{"name", "probability"}], "source": [...], "rating": "safe"},
     "detail": {"rating": {...}, "scores": {"tag": {...}, "subject": {...}, "source": {...}, "rating": {...}}}}

-- what src/py-pipeline/tagger/pixai_tag.py emitted, so slopbooru's parser
reads it unchanged. Two extensions to the request body, both optional:
threshold_general and threshold_character raise the model's calibrated
thresholds (never lower them); they are part of the request, so of the
cache key.

Requests in flight are batched: one request, then whatever else arrives
within 20 ms, up to --batch. Decoding runs ahead of the forward pass on a
thread pool; an out-of-memory batch halves itself. A picture that doesn't
decode is a 400 (the same picture fails the same way every time); a
failed forward pass is a 500 (worth retrying).

No dependencies beyond the engine's (torch, torchvision, transformers,
Pillow): the server is the standard library's.
"""

import argparse
import base64
import binascii
import io
import json
import os
import queue
import socketserver
import sys
import threading
import time
import uuid
from concurrent.futures import Future, ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

RATING_NAMES = {"rating:g": "safe", "rating:s": "sensitive",
                "rating:q": "questionable", "rating:e": "explicit"}
GATHER = 0.02  # seconds to wait for more requests before running a batch


class BadImage(Exception):
    pass


def decode_image(data):
    import PIL.Image as Image
    import torchvision.transforms.functional as vF

    try:
        img = Image.open(io.BytesIO(data))
        img.load()
    except Exception as e:
        raise BadImage(f"{type(e).__name__}: {e}") from e
    if img.mode != "RGB":  # composite transparency onto white, like upstream
        img = img.convert("RGBA")
        canvas = Image.new("RGBA", img.size, (255, 255, 255))
        canvas.alpha_composite(img)
        img = canvas.convert("RGB")
    return vF.to_tensor(img)


def rescale_pad(image, output_size):
    import torchvision.transforms.functional as vF

    h, w = image.shape[-2:]
    if h != output_size or w != output_size:
        r = min(output_size / h, output_size / w)
        new_h, new_w = int(h * r), int(w * r)
        ph, pw = output_size - new_h, output_size - new_w
        image = vF.resize(image, [new_h, new_w])
        image = vF.pad(image, [pw // 2, ph // 2, pw - pw // 2, ph - ph // 2], 0)
    return image


def image_bytes(body):
    """The first image in a chat request, as bytes (data: URLs only)."""
    for m in body.get("messages") or []:
        content = m.get("content") if isinstance(m, dict) else None
        if not isinstance(content, list):
            continue
        for part in content:
            if not isinstance(part, dict) or part.get("type") != "image_url":
                continue
            url = (part.get("image_url") or {}).get("url", "")
            if not url.startswith("data:"):
                raise BadImage("the image must be a data: URL")
            head, _, payload = url.partition(",")
            if not head.endswith(";base64"):
                raise BadImage("the data: URL must be base64")
            try:
                return base64.b64decode(payload, validate=True)
            except binascii.Error as e:
                raise BadImage(f"bad base64: {e}") from e
    raise BadImage("no image_url in the messages")


class Tagger:
    def __init__(self, args):
        import torch
        from transformers import AutoModel

        self.torch = torch
        device = args.device
        if device == "auto":
            device = "cuda" if torch.cuda.is_available() else "cpu"
        self.device = device
        self.dtype = torch.float16 if device == "cuda" else torch.float32
        self.model = AutoModel.from_pretrained(
            args.model, trust_remote_code=True, dtype=self.dtype, local_files_only=True
        ).to(device).eval()
        cfg = self.cfg = self.model.config
        if getattr(cfg, "tags_best_threshold", None) is not None:
            self.base = torch.tensor(cfg.tags_best_threshold, dtype=torch.float32)
        else:
            self.base = torch.cat([torch.full([n], cfg.category_best_threshold[c]) for c, n in cfg.tags_split])
        self.spans, start = {}, 0
        for category, count in cfg.tags_split:
            self.spans[category] = (start, start + count)
            start += count
        self.defaults = (args.threshold_general, args.threshold_character)
        self.thresholds = {}
        self.size = args.batch
        self.pool = ThreadPoolExecutor(max_workers=args.workers or min(8, os.cpu_count() or 4))
        self.requests = queue.Queue()
        threading.Thread(target=self.loop, daemon=True).start()

    def threshold(self, general, character):
        """The calibrated thresholds, raised (never lowered) by the floors."""
        key = (general, character)
        if key not in self.thresholds:
            t = self.base.clone()
            floors = {"character": character, "copyright": character, "general": general}
            for category, (lo, hi) in self.spans.items():
                if floors.get(category) is not None:
                    t[lo:hi] = t[lo:hi].clamp(floors[category], 1)
            self.thresholds[key] = t
        return self.thresholds[key]

    def prepare(self, data):
        return self.torch_normalize(rescale_pad(decode_image(data), self.cfg.img_size))

    def torch_normalize(self, t):
        import torchvision.transforms.functional as vF
        return vF.normalize(t, [0.5] * 3, [0.5] * 3)

    def contribution(self, probs, thresholds):
        cfg = self.cfg

        def hits(category):
            lo, hi = self.spans[category]
            p = probs[lo:hi]
            idx = (p > thresholds[lo:hi]).nonzero().flatten()
            idx = idx[p[idx].argsort(descending=True)]
            return [(cfg.tags[lo + i], p[i].item()) for i in idx.tolist()]

        lo, hi = self.spans["rating"]
        rating_scores = {RATING_NAMES.get(cfg.tags[lo + i], cfg.tags[lo + i]):
                         round(probs[lo + i].item(), 4) for i in range(hi - lo)}
        rating = max(rating_scores, key=rating_scores.get)
        general, character, copyright_ = hits("general"), hits("character"), hits("copyright")
        return {
            "success": True,
            "fields": {
                "tags": [t for t, _ in general],
                "subjects": [{"name": t, "probability": round(p, 3)} for t, p in character],
                "source": [t for t, _ in copyright_],
                "rating": rating,
            },
            "detail": {"rating": rating_scores, "scores": {
                "tag": {t: round(p, 4) for t, p in general},
                "subject": {t: round(p, 4) for t, p in character},
                "source": {t: round(p, 4) for t, p in copyright_},
                "rating": rating_scores,
            }},
        }

    def tag(self, data, general=None, character=None):
        """Tags one picture: blocks until its batch has run."""
        general = self.defaults[0] if general is None else general
        character = self.defaults[1] if character is None else character
        result = Future()
        # decoding starts now, so it runs ahead of the forward pass
        self.requests.put((self.pool.submit(self.prepare, data), (general, character), result))
        return result.result()

    def infer(self, batch):
        torch = self.torch
        try:
            with torch.inference_mode():
                return self.model(torch.stack(batch).to(self.device, self.dtype)).float().sigmoid().cpu()
        except torch.OutOfMemoryError:
            if len(batch) == 1:
                raise
            if self.device == "cuda":
                torch.cuda.empty_cache()
            self.size = max(1, len(batch) // 2)
            print(f"pixai: out of memory at batch {len(batch)}, now {self.size}", file=sys.stderr)
            return torch.cat([self.infer(batch[:self.size]), self.infer(batch[self.size:])])

    def loop(self):
        while True:
            batch = [self.requests.get()]
            while len(batch) < self.size:
                try:
                    batch.append(self.requests.get(timeout=GATHER))
                except queue.Empty:
                    break
            tensors, kept = [], []
            for prepared, floors, result in batch:
                try:
                    tensors.append(prepared.result())
                    kept.append((floors, result))
                except Exception as e:  # undecodable: the caller's 400
                    result.set_exception(e)
            if not tensors:
                continue
            try:
                probs = self.infer(tensors)
            except Exception as e:
                for _, result in kept:
                    result.set_exception(RuntimeError(f"{type(e).__name__}: {e}"))
                continue
            for i, (floors, result) in enumerate(kept):
                try:
                    result.set_result(self.contribution(probs[i], self.threshold(*floors)))
                except Exception as e:
                    result.set_exception(RuntimeError(f"{type(e).__name__}: {e}"))


def handler(tagger, name):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        server_version = "pixai-server"

        def address_string(self):  # a unix socket has no address
            return self.client_address[0] if isinstance(self.client_address, tuple) else "unix"

        def log_message(self, fmt, *a):
            pass  # one line per picture is noise; errors are reported below

        def reply(self, status, obj):
            data = json.dumps(obj, ensure_ascii=False).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def error(self, status, message, typ):
            self.reply(status, {"error": {"message": message, "type": typ, "param": None, "code": None}})

        def do_GET(self):
            if self.path in ("/v1/models", "/health"):
                self.reply(200, {"object": "list", "data": [
                    {"id": name, "object": "model", "created": 0, "owned_by": "pixai-labs"}]})
            else:
                self.error(404, f"no route {self.path}", "invalid_request_error")

        def do_POST(self):
            if self.path != "/v1/chat/completions":
                self.error(404, f"no route {self.path}", "invalid_request_error")
                return
            try:
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n))
                data = image_bytes(body)
                floors = []
                for k in ("threshold_general", "threshold_character"):
                    v = body.get(k)
                    if v is not None and not isinstance(v, (int, float)):
                        raise BadImage(f"{k} must be a number")
                    floors.append(v)
            except (ValueError, BadImage) as e:
                self.error(400, str(e), "invalid_request_error")
                return
            t = time.time()
            try:
                c = tagger.tag(data, *floors)
            except BadImage as e:
                self.error(400, f"the picture doesn't decode: {e}", "invalid_request_error")
                return
            except Exception as e:
                print(f"pixai: {e}", file=sys.stderr)
                self.error(500, str(e), "server_error")
                return
            content = json.dumps({"model": name, **c}, ensure_ascii=False)
            self.reply(200, {
                "id": "chatcmpl-" + uuid.uuid4().hex, "object": "chat.completion",
                "created": int(t), "model": body.get("model") or name, "system_fingerprint": None,
                "choices": [{"index": 0, "finish_reason": "stop",
                             "message": {"role": "assistant", "content": content}}],
                "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
            })

    return Handler


class UnixHTTPServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def get_request(self):
        request, _ = super().get_request()
        return request, ("unix", 0)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--model", required=True, help="the pixai-tagger checkout")
    where = ap.add_mutually_exclusive_group(required=True)
    where.add_argument("--uds", help="listen on this unix socket")
    where.add_argument("--port", type=int, help="listen on this TCP port")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--name", default="pixai", help="the model name it answers to")
    ap.add_argument("--device", default="auto")
    ap.add_argument("--batch", type=int, default=16)
    ap.add_argument("--workers", type=int, default=0)
    ap.add_argument("--threshold-general", type=float, default=None)
    ap.add_argument("--threshold-character", type=float, default=None)
    args = ap.parse_args()

    tagger = Tagger(args)
    h = handler(tagger, args.name)
    if args.uds:
        try:
            os.unlink(args.uds)
        except FileNotFoundError:
            pass
        server = UnixHTTPServer(args.uds, h)
    else:
        server = ThreadingHTTPServer((args.host, args.port), h)
        server.daemon_threads = True
    print(f"pixai: tagging on {tagger.device}, listening on {args.uds or f'{args.host}:{args.port}'}",
          file=sys.stderr, flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
