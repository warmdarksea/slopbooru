# slopbooru

A small booru for browsing what a fleet of image models said about a
directory of pictures: tags, characters, ratings, descriptions, OCR'd text
with its boxes, safety verdicts, each value with its model and confidence.
It reads the sqlite db written by an image recognition pipeline and serves
it as plain html. Search, facets and thumbnails included, and no JavaScript
framework or external assets.

One file per platform with nothing to install: static binaries for Linux
(any distro, any glibc or none) and Windows 10+, on amd64 and arm64.

## Running it

```
slopbooru --db results.db --images PICTURES_DIR [--host 127.0.0.1] [--port 8800]
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
(what *that* model tagged), `field:description`.

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
  the hash cache, and the http handler on a generated fixture.
- `go run ./cmd/slopcheck -bin ./slopbooru`: end to end against a real
  binary. It builds a fixture db and pictures, moves and renames the
  pictures, starts the server, and checks every file by hash, every
  thumbnail, the routes, and the hash cache across restarts. Add
  `-wrap wine -winpaths` to check the Windows build under wine.
