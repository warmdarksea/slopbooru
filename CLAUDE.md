# slopbooru

One Go module (`module slopbooru`), one binary, two jobs:

- `slopbooru [serve]`: the booru, a browser over the recognition sqlite db
  (`internal/booru`).
- `slopbooru classify`: asks models about pictures through any
  OpenAI-compatible server and writes the answers into that db
  (`internal/classify`, classify.toml built in).

Layout: `cmd/slopbooru` only picks one of the two. `internal/index` finds
pictures by content (both jobs use it), `internal/sqliteuri` opens dbs on
any OS, `internal/cli` is the brand,
version and log lines (`-X slopbooru/internal/cli.Brand=...`), the rest
of `internal/` is classify's helpers (store writer, reply parsers,
images). `test/` is the end-to-end tester (`test/slopcheck`) and the
fixture it and the booru's tests share. Unit tests sit beside their
package, as Go wants. Nothing but docs and nix files in the root.

Static Go, cgo off, released as plain binaries for linux/windows on
amd64/arm64 (`nix-build -A release`). Both jobs run everywhere: classify is
an HTTP client.

`fleet/` is the models the built-in classify.toml names, as gpu-timeshare
models (gpu-timeshare is its own repository: the queue, scheduler and cache
that used to be imgrec here). Nix-only: the engine is a virtualenv of cuda
wheels that only nix keeps reproducible. nornpkgs (the user's NixOS config)
packages the booru, gpu-timeshare and the fleet.

## The split (the user's design)

- **serving** a model is the fleet's (and gpu-timeshare's): weights,
  engine, vllm's settings, its share of the card, the pixai server;
- **asking** it is classify.toml's: prompt, system prompt, schema, params,
  max_pixels (downscaling happens here, before sending), kind (how the
  answer is read), needs (llamaguard reads gemma's description).

A picture travels in the request, as a data: URL: never a path.

## nix

`default.nix` takes `system`, `pkgs` and `fetchHuggingFace` (nornpkgs'
`lib/models.nix` fetcher, already bound to a pkgs). The defaults pin the
nixpkgs and nornpkgs revisions the engine and the models were first built
with: **the engine (`vllm-env`) and the weights must keep their store
paths** -- the weights' path is in gpu-timeshare's cache keys and the
store's provenances, and a path that moves means a 75 GB re-download.
Check `nix eval --raw -f . vllm-env.outPath` (bkhbyhhz...-vllm-env) and
`fleet.weights.<m>.outPath` after touching `fleet/` or the pins.

- `fleet/engine.nix`: every runtime fix the wheels need on nix, each found
  on a real card and commented with what broke without it (relaxWheels,
  dropStrays, hummingNvrtc, the nvidia lib dirs on LD_LIBRARY_PATH, CUDA_*
  unset, no double patching of ptxas). Don't "simplify" them away. After a
  lock bump, `nix-build -A fleet.engine.vllm-env --keep-going` lists every
  wheel relaxWheels needs, in one pass. `fleet/engine/pyproject.toml` keeps
  its llm-qwen name on purpose (renaming relocks and rebuilds for nothing).
- `fleet/default.nix`: per model, the vllm flags (`settings`, hashed into
  the fingerprint, so into gpu-timeshare's cache key) and `cost`/`seqs`.
- Big model downloads: prime the store with a resumable download (curl -C -
  per file, `nix store add --mode nar`) -- never tofu-fetch multi-GB models.
- Files nix builds from must be git-added (flake-style source filtering in
  callers); `default.nix`'s `src` fileset names what the Go build reads
  (go.mod, go.sum, cmd/, internal/, test/: everything embedded lives there).

## invariants

- **The schema exists twice here** (`test/fixture/schema*.sql` for the
  booru's tests, `internal/store/schema*.sql` for the writer) and once more
  in llm-qwen's `src/py-pipeline/schema*.sql`. Change them together.
- classify's own tables (`classify_*`) sit beside the store's in the same
  db; the booru reads none of them. `classify_answers` keeps every answer
  verbatim by (picture, question, context): it is what `--reparse`
  rebuilds rows from, and what makes a re-run skip what's answered.
- The question is the model's whole config entry (plus the backend url):
  changing any of it asks again, and the store keeps the new rows beside
  the old (provenance = model + mode + params).
- gemma's schema needs `confidence` before `description` (sorted keys):
  in the other order it degenerates into ", " strings (on the card,
  2026-11-08). classify sends schemas with sorted keys regardless.
- The booru serves files only by image id, never by a request path; sort
  orders are fixed fragments picked by key.
- Booru edits (`--edit`) are a provenance (`model = 'manual'`); removals are
  manual rows at confidence 0, read through `visible`.

## tests

`go test ./...` (classify against a fake OpenAI backend in both modes; no
GPU), `nix-build -A tests.all` (unit, static, linux and windows-under-wine
slopcheck). slopcheck crawls ~600 pages of each fixture db: every page must
render whole with its sidebar.

On the real card: `gpu-timeshare serve --config $(nix-build -A fleet.timeshare-toml)`
(gpu-timeshare, with the whole GPU), then `slopbooru classify` against it.
