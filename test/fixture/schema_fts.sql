-- full text index over every annotation value, whatever field it came from:
-- one search box that hits descriptions, ocr'd text, tags and character
-- names alike. applied on top of schema.sql only when sqlite has FTS5 --
-- without it the browser falls back to LIKE scans, which is fine at this
-- scale and merely slower.
--
-- external content table: the rows live in annotations and the index is
-- kept in sync by the triggers below, so there is exactly one copy of the
-- text. tokenchars keeps booru-style tags ('cardcaptor_sakura',
-- 'kero_(cardcaptor_sakura)') searchable as one word and as their parts.

CREATE VIRTUAL TABLE IF NOT EXISTS annotations_fts USING fts5(
  value,
  field UNINDEXED,
  content='annotations',
  content_rowid='id',
  tokenize="unicode61 remove_diacritics 2 tokenchars '_-'"
);

CREATE TRIGGER IF NOT EXISTS annotations_ai AFTER INSERT ON annotations BEGIN
  INSERT INTO annotations_fts(rowid, value, field)
    VALUES (new.id, new.value, new.field);
END;
CREATE TRIGGER IF NOT EXISTS annotations_ad AFTER DELETE ON annotations BEGIN
  INSERT INTO annotations_fts(annotations_fts, rowid, value, field)
    VALUES ('delete', old.id, old.value, old.field);
END;
CREATE TRIGGER IF NOT EXISTS annotations_au AFTER UPDATE ON annotations BEGIN
  INSERT INTO annotations_fts(annotations_fts, rowid, value, field)
    VALUES ('delete', old.id, old.value, old.field);
  INSERT INTO annotations_fts(rowid, value, field)
    VALUES (new.id, new.value, new.field);
END;
