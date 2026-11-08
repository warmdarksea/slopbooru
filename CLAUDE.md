# slopbooru

Two programs, one Go module (`module slopbooru`):

- **slopbooru**, the frontend (repository root): a booru over the
  recognition sqlite db. Static Go, cgo off, released as plain binaries for
  linux/windows on amd64/arm64 (`nix-build -A release`).
- **imgrec**, the backend (`imgrec/`): the pipeline that fills that db --
  seven vision models on one GPU behind a scheduler and an HTTP API.
  **Nix-only** (`nix-build -A imgrec`): its engine is a virtualenv of cuda
  wheels that only nix keeps reproducible. Design and the user's decisions
  in `imgrec/DESIGN.md` -- read it before changing imgrec.

Both came from the user's llm-qwen repository (history kept, merged here
2026-11-08). llm-qwen keeps the python pipeline (`src/py-pipeline`) and the
python booru, and checks Go-vs-python store parity with `imgrec/cmd/pyparity`.
nornpkgs (the user's NixOS config) packages both programs from this
repository's `default.nix` and runs imgrec as `services.imgrec`.

## nix

`default.nix` takes `system`, `pkgs` and `fetchHuggingFace` (nornpkgs'
`lib/models.nix` fetcher, already bound to a pkgs). The defaults pin the
nixpkgs and nornpkgs revisions the engine and the models were first built
with: **the engine (`vllm-env`), `models.json`, the specs and the pixai
worker must keep their store paths** -- they are in the cache keys (or the
store's provenances), and a model path that moves means a 75 GB re-download.
Check with `nix eval --raw -f . vllm-env.outPath` and `imgrec.manifest.outPath`
after touching `imgrec/nix` or the pins.

- `imgrec/nix/engine.nix`: every runtime fix the wheels need on nix, each
  found on a real card and commented with what broke without it (relaxWheels,
  dropStrays, hummingNvrtc, the nvidia lib dirs on LD_LIBRARY_PATH, CUDA_*
  unset, no double patching of ptxas). Don't "simplify" them away. After a
  lock bump, `nix-build -A imgrec.engine --keep-going` lists every wheel
  relaxWheels needs, in one pass. `imgrec/engine/pyproject.toml` keeps its
  llm-qwen name on purpose (renaming relocks and rebuilds for nothing).
- Big model downloads: prime the store with a resumable download (curl -C -
  per file, `nix store add --mode nar`) -- never tofu-fetch multi-GB models.
- Files nix builds from must be git-added (flake-style source filtering in
  callers); `default.nix`'s `src` fileset names what the Go build reads.

## invariants

- **The schema exists three times**: `internal/fixture/schema*.sql`
  (booru), `imgrec/internal/store/schema*.sql`, and llm-qwen's
  `src/py-pipeline/schema*.sql`. Change them together.
- imgrec's store writer and parsers stay **row-for-row identical** to the
  python pipeline's (llm-qwen's `imgrec-pyparity` check); `internal/pyjson`
  reproduces python's json/str bytes for that.
- imgrec's jobs table has one definition, `pipeline.Schema`; every command
  that opens the db uses it.
- Tenancy: ask returns an answer **only for a picture that client has sent**
  (`client_images`); `TestServe` asserts it. Tokens are stored as sha256.
- The booru serves files only by image id, never by a request path; sort
  orders are fixed fragments picked by key.
- Booru edits (`--edit`) are a provenance (`model = 'manual'`); removals are
  manual rows at confidence 0, read through `visible`.

## tests

`go test ./...` (stub runners stand in for vllm and the tagger; no GPU),
`nix-build -A tests.all` (unit, static, linux and windows-under-wine
slopcheck). slopcheck crawls ~600 pages of each fixture db: every page must
render whole with its sidebar. imgrec's Go tests also run inside
`nix-build -A imgrec.bin`.

On the real card: `imgrec run` / `imgrec serve` with the bundled
`nix-build -A imgrec`; it must have the whole GPU (stop other LLM servers).
