// Package images identifies pictures by content: sha256 of the bytes, and
// the metadata the store records (size, mime, dimensions).
package images

import (
	"crypto/sha256"
	"encoding/hex"
	"image"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"slopbooru/internal/store"
)

// mime by decoder name, as Pillow's Image.MIME names them
var mimes = map[string]string{"jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif",
	"webp": "image/webp", "bmp": "image/bmp", "tiff": "image/tiff"}

// by extension, when no decoder recognises the bytes (store.py's fallback)
var extMimes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".gif": "image/gif", ".bmp": "image/bmp"}

// Describe hashes a file and reads its metadata.
func Describe(path string) (store.Image, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return store.Image{}, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	f, err := os.Open(abs)
	if err != nil {
		return store.Image{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return store.Image{}, err
	}
	im := store.Image{SHA256: hex.EncodeToString(h.Sum(nil)), Path: abs, Bytes: n}
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		if cfg, format, err := image.DecodeConfig(f); err == nil {
			im.Width, im.Height, im.Mime = cfg.Width, cfg.Height, mimes[format]
		}
	}
	if im.Mime == "" {
		im.Mime = extMimes[strings.ToLower(filepath.Ext(abs))]
	}
	return im, nil
}

// Index hashes every regular file directly in dir (not recursive, like
// classify.py's survey): sha256 -> image, and name -> image.
func Index(dir string) (map[string]store.Image, map[string]store.Image, error) {
	bySha, byName := map[string]store.Image{}, map[string]store.Image{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.Type()&fs.ModeType != 0 && e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		im, err := Describe(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if _, dup := bySha[im.SHA256]; !dup {
			bySha[im.SHA256] = im
		}
		byName[e.Name()] = im
	}
	return bySha, byName, nil
}

// DescribeKnown is Describe for a file whose sha256 is already known: the
// metadata only, without reading the whole file again.
func DescribeKnown(path, sha string) (store.Image, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return store.Image{}, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return store.Image{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return store.Image{}, err
	}
	im := store.Image{SHA256: sha, Path: abs, Bytes: st.Size()}
	if cfg, format, err := image.DecodeConfig(f); err == nil {
		im.Width, im.Height, im.Mime = cfg.Width, cfg.Height, mimes[format]
	}
	if im.Mime == "" {
		im.Mime = extMimes[strings.ToLower(filepath.Ext(abs))]
	}
	return im, nil
}
