-- llm-qwen image recognition store.
--
-- two ideas carry the whole schema:
--
--   images are content-addressed. the id is a plain integer, but identity is
--   sha256 of the bytes, so the same picture found under three paths is one
--   image with three rows in image_paths.
--
--   every statement a model made is one row in annotations, keyed by a
--   provenance: "which model, built from exactly which nix store paths".
--   so a tag is a (provenance, value, confidence) triple, a description is a
--   (provenance, value, confidence) triple, and anything can be traced back
--   to the closure that produced it -- down to the weights' store path.
--
-- nothing here is model-specific: adding an eighth model adds rows, never
-- columns.

CREATE TABLE IF NOT EXISTS images (
  id         INTEGER PRIMARY KEY,
  sha256     TEXT    NOT NULL UNIQUE,
  bytes      INTEGER NOT NULL,
  mime       TEXT,
  width      INTEGER,
  height     INTEGER,
  first_seen TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- where the bytes were found. many paths per image; a path is unique per
-- image but the same path may later hold different content (then it moves
-- to the new image's row set).
CREATE TABLE IF NOT EXISTS image_paths (
  image_id  INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
  path      TEXT    NOT NULL,
  last_seen TEXT    NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (image_id, path)
);
CREATE INDEX IF NOT EXISTS image_paths_path ON image_paths(path);

-- ---- provenance: the nix store path tree behind a value ------------------

-- store paths are interned: the fleet's closures are thousands of paths
-- each and overlap almost completely (one python, one cuda, one engine).
CREATE TABLE IF NOT EXISTS store_paths (
  id   INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE
);

-- a closure is identified by the sha256 of its sorted path list, so two
-- provenances built from the same tree share one closure row.
CREATE TABLE IF NOT EXISTS closures (
  id       INTEGER PRIMARY KEY,
  sha256   TEXT    NOT NULL UNIQUE,
  n_paths  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS closure_paths (
  closure_id    INTEGER NOT NULL REFERENCES closures(id) ON DELETE CASCADE,
  store_path_id INTEGER NOT NULL REFERENCES store_paths(id),
  PRIMARY KEY (closure_id, store_path_id)
);

-- model_path is the weights checkout, runner_path the launcher that drove
-- it, engine the vllm that executed it -- a store path in the bundled
-- flavour, a mutable ./build venv in the unbundled one (recorded verbatim,
-- so an impure run is visible as such rather than silently indistinguishable).
-- params is json: prompt, image budget, whatever shaped the request.
CREATE TABLE IF NOT EXISTS provenances (
  id          INTEGER PRIMARY KEY,
  model       TEXT NOT NULL,
  mode        TEXT,
  model_path  TEXT NOT NULL DEFAULT '',
  runner_path TEXT NOT NULL DEFAULT '',
  engine      TEXT NOT NULL DEFAULT '',
  closure_id  INTEGER REFERENCES closures(id),
  params      TEXT NOT NULL DEFAULT '{}',
  created     TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (model, mode, model_path, runner_path, engine, closure_id, params)
);
CREATE INDEX IF NOT EXISTS provenances_model ON provenances(model);

-- ---- the annotations ----------------------------------------------------

-- field is the kind of statement: tag, description, text, rating, subject,
-- emotion, dimensionality, source. key names the sub-entity a value is
-- about ('' for the image as a whole, a subject's name for its emotion) --
-- not null, because sqlite treats nulls as distinct in a UNIQUE and a
-- re-import would duplicate every row. confidence is 0..1 where the model
-- offers one and null where it does not; detail is json (ocr boxes, the
-- full per-class score vector, ...).
CREATE TABLE IF NOT EXISTS annotations (
  id            INTEGER PRIMARY KEY,
  image_id      INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
  provenance_id INTEGER NOT NULL REFERENCES provenances(id),
  field         TEXT    NOT NULL,
  key           TEXT    NOT NULL DEFAULT '',
  value         TEXT    NOT NULL,
  confidence    REAL,
  detail        TEXT,
  UNIQUE (image_id, provenance_id, field, key, value)
);
CREATE INDEX IF NOT EXISTS annotations_image ON annotations(image_id);
CREATE INDEX IF NOT EXISTS annotations_field ON annotations(field, value);
CREATE INDEX IF NOT EXISTS annotations_prov  ON annotations(provenance_id);

-- a model that ran and produced nothing usable is itself a fact worth
-- keeping: the browser shows it, and it keeps "no tags" distinguishable
-- from "never asked".
CREATE TABLE IF NOT EXISTS failures (
  image_id      INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
  provenance_id INTEGER NOT NULL REFERENCES provenances(id),
  error         TEXT,
  at            TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (image_id, provenance_id)
);

-- full text search over every value lives in schema_fts.sql: it needs an
-- sqlite built with FTS5, and the browser falls back to LIKE without it.

-- ---- convenience --------------------------------------------------------

-- flat join for ad-hoc sqlite3 sessions:
--   select model, value, confidence from annotation_rows
--   where field='subject' and sha256 like 'a1b2%' order by confidence desc;
CREATE VIEW IF NOT EXISTS annotation_rows AS
SELECT a.id, a.image_id, i.sha256, p.model, p.mode, a.field, a.key,
       a.value, a.confidence, a.detail, p.id AS provenance_id
FROM annotations a
JOIN images      i ON i.id = a.image_id
JOIN provenances p ON p.id = a.provenance_id;

-- how much each model had to say, and how sure it was:
--   select * from model_field_stats;
CREATE VIEW IF NOT EXISTS model_field_stats AS
SELECT p.model, a.field, COUNT(*) AS n,
       COUNT(a.confidence) AS n_with_confidence,
       ROUND(AVG(a.confidence), 3) AS mean_confidence
FROM annotations a JOIN provenances p ON p.id = a.provenance_id
GROUP BY p.model, a.field;
