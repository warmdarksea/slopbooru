#!/usr/bin/env python3
"""pixai-tagger-v1.0 as a long-lived worker for imgrec.

    pixai_worker.py --model DIR [--device auto|cpu|cuda] [--batch 16]
                    [--threshold-general X] [--threshold-character X]

Loads torch and the model once, prints {"ready": true}, then answers
requests until stdin closes:

    in:  {"id": 7, "path": "/abs/image.png"}
    out: {"id": 7, "image": "/abs/image.png", "model": "pixai",
          "success": true, "fields": {...}, "detail": {...}}

one json line each, in any order. The contribution is exactly what
src/py-pipeline/tagger/pixai_tag.py emits (preprocessing and post-
processing are copied from it), so imgrec's parsers and store read it
unchanged.

Batches are made of whatever is waiting: take one request, then whatever
else arrives within 20 ms, up to --batch. A single image is answered at
once; a flood fills the batches. Decoding the next batch overlaps the
forward pass (it starts the moment a request arrives), and an
out-of-memory batch halves itself.
"""

import argparse
import json
import os
import queue
import sys
import threading
from concurrent.futures import ThreadPoolExecutor

MODEL_NAME = "pixai"
RATING_NAMES = {"rating:g": "safe", "rating:s": "sensitive",
                "rating:q": "questionable", "rating:e": "explicit"}
GATHER = 0.02  # seconds to wait for more requests before running a batch


def load_image(path):
    import PIL.Image as Image
    import torchvision.transforms.functional as vF

    img = Image.open(path)
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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True)
    ap.add_argument("--device", default="auto")
    ap.add_argument("--batch", type=int, default=16)
    ap.add_argument("--workers", type=int, default=0)
    ap.add_argument("--threshold-general", type=float, default=None)
    ap.add_argument("--threshold-character", type=float, default=None)
    args = ap.parse_args()

    import torch
    import torchvision.transforms.functional as vF
    from transformers import AutoModel

    device = args.device
    if device == "auto":
        device = "cuda" if torch.cuda.is_available() else "cpu"
    dtype = torch.float16 if device == "cuda" else torch.float32
    model = AutoModel.from_pretrained(
        args.model, trust_remote_code=True, dtype=dtype, local_files_only=True
    ).to(device).eval()
    cfg = model.config

    # thresholds: the model's calibrated per-category values, raised (never
    # lowered) by the optional floors
    if getattr(cfg, "tags_best_threshold", None) is not None:
        thresholds = torch.tensor(cfg.tags_best_threshold, dtype=torch.float32)
    else:
        thresholds = torch.cat([torch.full([n], cfg.category_best_threshold[c])
                                for c, n in cfg.tags_split])
    floors = {"character": args.threshold_character, "copyright": args.threshold_character,
              "general": args.threshold_general}
    spans, start = {}, 0
    for category, count in cfg.tags_split:
        spans[category] = (start, start + count)
        if floors.get(category) is not None:
            thresholds[start:start + count] = thresholds[start:start + count].clamp(floors[category], 1)
        start += count

    def hits(probs, category):
        lo, hi = spans[category]
        p = probs[lo:hi]
        idx = (p > thresholds[lo:hi]).nonzero().flatten()
        idx = idx[p[idx].argsort(descending=True)]
        return [(cfg.tags[lo + i], p[i].item()) for i in idx.tolist()]

    def contribution(path, probs):
        lo, hi = spans["rating"]
        rating_scores = {RATING_NAMES.get(cfg.tags[lo + i], cfg.tags[lo + i]):
                         round(probs[lo + i].item(), 4) for i in range(hi - lo)}
        rating = max(rating_scores, key=rating_scores.get)
        general, character, copyright_ = hits(probs, "general"), hits(probs, "character"), hits(probs, "copyright")
        return {
            "image": path, "model": MODEL_NAME, "success": True,
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

    def prepare(path):
        return vF.normalize(rescale_pad(load_image(path), cfg.img_size), [0.5] * 3, [0.5] * 3)

    out_lock = threading.Lock()

    def emit(obj):
        line = json.dumps(obj, ensure_ascii=False)
        with out_lock:
            sys.stdout.write(line + "\n")
            sys.stdout.flush()

    size = args.batch

    def infer(batch):
        nonlocal size
        try:
            with torch.inference_mode():
                return model(torch.stack(batch).to(device, dtype)).float().sigmoid().cpu()
        except torch.OutOfMemoryError:
            if len(batch) == 1:
                raise
            if device == "cuda":
                torch.cuda.empty_cache()
            size = max(1, len(batch) // 2)
            print(f"pixai: out of memory at batch {len(batch)}, now {size}", file=sys.stderr)
            return torch.cat([infer(batch[:size]), infer(batch[size:])])

    requests = queue.Queue()
    # decoding starts the moment a request arrives, so it always runs ahead
    # of the forward pass, whatever the batch boundaries
    pool = ThreadPoolExecutor(max_workers=args.workers or min(8, os.cpu_count() or 4))

    def read():
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue
            try:
                req = json.loads(line)
                requests.put((req["id"], req["path"], pool.submit(prepare, req["path"])))
            except (ValueError, KeyError, TypeError) as e:
                print(f"pixai: bad request {line[:200]!r}: {e}", file=sys.stderr)
        requests.put(None)  # end of input

    threading.Thread(target=read, daemon=True).start()
    emit({"ready": True})

    done = False
    while not done:
        first = requests.get()
        if first is None:
            break
        batch = [first]
        while len(batch) < size:
            try:
                nxt = requests.get(timeout=GATHER)
            except queue.Empty:
                break
            if nxt is None:
                done = True
                break
            batch.append(nxt)
        tensors, kept = [], []
        for rid, path, fut in batch:
            try:
                tensors.append(fut.result())
                kept.append((rid, path))
            except Exception as e:  # unreadable or corrupt image
                emit({"id": rid, "image": path, "model": MODEL_NAME, "success": False,
                      "error": f"{type(e).__name__}: {e}"})
        if not tensors:
            continue
        try:
            probs = infer(tensors)
        except Exception as e:
            for rid, path in kept:
                emit({"id": rid, "image": path, "model": MODEL_NAME, "success": False,
                      "error": f"{type(e).__name__}: {e}"})
            continue
        for i, (rid, path) in enumerate(kept):
            emit({"id": rid, **contribution(path, probs[i])})


if __name__ == "__main__":
    main()
