# slopbooru

A small booru for browsing what a fleet of image models said about a
directory of pictures: tags, characters, ratings, descriptions, OCR'd text
with its boxes, and safety verdicts. Each value shows its model and
confidence.

Pages are plain html, laid out like a booru:
- tags, characters, sources, moods and hazards on a sidebar to the left,
  thumbnails beside it;
- on an image's page, the picture fitted to the screen (click for full
  size) with the models' descriptions underneath.

There's no JavaScript framework and no external assets.

It also asks the models: `slopbooru classify` sends every picture to every
configured model, through any server that speaks the OpenAI API, and
writes the answers into the db the booru reads (see [Classifying](#classifying)).

One file per platform with nothing to install: static binaries for Linux
(any distro, any glibc or none) and Windows 10+, on amd64 and arm64.

## Running it

```
slopbooru [serve] --db results.db --images PICTURES_DIR [--host 127.0.0.1] [--port 8800]
```

then open <http://127.0.0.1:8800>. On Windows it's `slopbooru.exe` with
the same options, run from a terminal (`cmd` or PowerShell).

- `--db`: the `results.db`.
- `--images`: the folder with the pictures. It is searched recursively and
  pictures are matched **by content** (sha256), so the folder can be laid
  out and named however you like. It doesn't have to match the machine
  that made the db.
- `--host`: the address to listen on. `127.0.0.1` is this machine only;
  `0.0.0.0` makes it reachable from the rest of your network.
- `--port`: the port to listen on.

Every start hashes the whole picture folder. Files it has already seen,
with the same folder, name, size and modification time, are skipped. So
the first start on a big folder takes a while and later ones are quick.
That cache and the thumbnails live in `<db>.thumbs/` (`--thumbs` to move
it). It is safe to delete at any time.

Without `--images` it looks for each picture at the path recorded in the db.

Search box: `marisa` (prefix match over every value), `rating:safe`,
`tag:solo`, `subject:"maid girl"`, `"a phrase"`, `model:pixai tag:marisa`
(what *that* model tagged), `field:description`. The `+` beside a value in
the sidebar adds it to the current search.

## Editing by hand

Start it with `--edit` and the pages gain controls:

- **add** a value to an image: a tag, character, source, rating, or a
  description of your own. Spaces in a tag become underscores, like the
  rest of the tag vocabulary.
- **remove** any value with the × beside it. It disappears from that image
  everywhere (the page, search, the sidebar counts), whichever model said
  it. What the model said is kept in the db, so **restore** brings it back.
- **upload** pictures (header link). They are saved into the `--images`
  folder under their own names and added to the db untagged. A picture
  already in the db is recognised by content and not added twice.
- **delete image**: removes it and everything said about it from the db.
  The file itself stays on disk.

Your edits are stored in the db as their own source, `manual`, next to
the models'. They're listed on `/stats` and the provenance pages like any
other source.

Editing is off by default. With `--edit`, anyone who can reach the
address and port can edit, so keep `--host 127.0.0.1` unless you trust
the network. The db file must be writable.

## Classifying

```
slopbooru classify --images PICTURES_DIR [--db results.db] [--config classify.toml] [--models a,b]
```

This asks each configured model about every picture in the folder
(searched recursively, found by content) and writes what they said into
the db. The db defaults to `PICTURES_DIR/results.db`.

**What gets asked.** The models, and what each is asked, are in
[`classify.toml`](classify.toml), built in:
- seven models for anime-ish pictures: tagger, two JSON describers, two
  captioners, OCR, and a safety verdict;
- one backend, `http://127.0.0.1:8100/v1`.

A `--config` file is read on top of it. An entry with the same name
replaces the built-in one whole, and `enabled = false` drops a model. A
model entry holds:
- its prompt and optional system prompt;
- a JSON schema, for answers that are structured;
- any request parameters;
- `max_pixels`: bigger pictures are downscaled before sending;
- its `kind`, which says how its answer is read.

`slopbooru classify --print-config` prints the built-in file.

**Backends.** A backend is anything with the OpenAI API:
- [gpu-timeshare](https://github.com/warmdarksea/gpu-timeshare) running
  this repository's fleet (below);
- one vllm or llama.cpp server;
- a hosted API.

```toml
[backends.timeshare]
url = "https://library:8100/v1"
token_env = "SLOPBOORU_TOKEN"     # or token = "...", token_file = "..."
cacert = "timeshare.crt"          # a self-signed server's certificate
mode = "batch"                    # the Batch API; "requests" for one request per picture
```

In batch mode, each model's pictures go up as one or more batches. The
server decides the order, which is what lets gpu-timeshare answer one
model at a time instead of reloading models back and forth. Batches
survive an interrupted run: the next run collects them first.

**Answers are kept.** Every answer is stored verbatim in the db, keyed by
the picture and the question, where the question is the model's whole
config entry. As a result:
- a picture that already has an answer to the current question isn't asked again;
- a stopped run resumes where it stopped;
- a new picture costs one request per model;
- an edited prompt asks everything again, and the new answers are stored
  beside the old ones;
- `--reparse` rebuilds the db's rows from the kept answers without asking anyone.

A model that reads another model's answer, like llama-guard reading a
description, is asked after that model.

## The fleet

[`fleet/`](fleet/) is the models the built-in config names, as
gpu-timeshare models. It's nix-only: the engine is a virtualenv of about
200 cuda wheels, patched at build time, which only nix keeps reproducible.
It contains:
- the engine, vllm;
- the weights, pinned by revision and hash, about 75 GB in all;
- each model's vllm settings and share of the card;
- `fleet/pixai_server.py`, which puts pixai behind the OpenAI API.

```
nix-build -A fleet.timeshare-toml     # a timeshare.toml running every model as a child process
timeshare serve --config result       # (with gpu-timeshare), then: slopbooru classify ...
```

On NixOS, nornpkgs runs gpu-timeshare as a service with these models,
each in its own sandboxed unit.

The split: everything about *serving* a model lives in `fleet/`, and
everything about *asking* it lives in `classify.toml`.

## Building

With nix:

```
nix-build -A slopbooru       # this machine
nix-build -A release         # all four binaries + SHA256SUMS
nix-build -A tests.all       # every test, the windows one under wine
nix-shell                    # go, gopls, sqlite, wine
```

Without nix: Go 1.26+, then

```
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" .
```

## Tests

- `go test ./...`: the formatting helpers, query parsing, EXIF orientation,
  classify against a fake OpenAI backend (batch and requests modes, resume,
  reparse, a guard reading a describer, downscaling, failures), the reply
  parsers and the store writer,
  the hash cache, and the http handler on a generated fixture.
- `go run ./cmd/slopcheck -bin ./slopbooru`: end to end against a real
  binary. It builds a fixture db and pictures, moves and renames the
  pictures, starts the server, and checks every file by hash, every
  thumbnail, the routes, and the hash cache across restarts, then crawls
  ~600 pages of each fixture db (with and without FTS5) for errors. Add
  `-wrap wine -winpaths` to check the Windows build under wine.
