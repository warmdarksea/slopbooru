// Package imagestore keeps pictures on disk by content:
//
//	ROOT/ab/abcdef....<ext>              the picture, as uploaded
//	ROOT/standins/abcdef...-<max>.png    a copy downscaled to <max> pixels
//
// The same bytes uploaded twice are one file. The jobs table refers to
// pictures by sha256 only.
package imagestore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"

	_ "image/gif"
	_ "image/jpeg"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"imgrec/internal/images"
	"imgrec/internal/store"
)

type Store struct{ Root string }

// Resize names the downscaling this package does. Specs declare it in
// their input (it is part of every cache key), and Input refuses any
// other name: a spec can't claim a method the code doesn't use.
const Resize = "catmull-rom/png v1"

var exts = map[string]string{"jpeg": ".jpg", "png": ".png", "gif": ".gif", "webp": ".webp",
	"bmp": ".bmp", "tiff": ".tiff"}

func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Store{abs}, os.MkdirAll(filepath.Join(abs, "standins"), 0o755)
}

// Put stores a picture and describes it; created is false when the same
// bytes were already here. Rejects what no decoder reads.
func (s *Store) Put(r io.Reader) (im store.Image, created bool, err error) {
	tmp, err := os.CreateTemp(s.Root, ".in-*")
	if err != nil {
		return store.Image{}, false, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return store.Image{}, false, err
	}
	tmp.Seek(0, io.SeekStart)
	_, format, err := image.DecodeConfig(tmp)
	tmp.Close()
	if err != nil {
		return store.Image{}, false, fmt.Errorf("not a picture: %w", err)
	}
	sha := hex.EncodeToString(h.Sum(nil))
	dest := filepath.Join(s.Root, sha[:2], sha+exts[format])
	if _, err := os.Stat(dest); err != nil {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return store.Image{}, false, err
		}
		if err := os.Rename(tmp.Name(), dest); err != nil {
			return store.Image{}, false, err
		}
		created = true
	}
	im, err = images.Describe(dest)
	return im, created, err
}

// PutFile stores a picture from a path.
func (s *Store) PutFile(path string) (store.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return store.Image{}, err
	}
	defer f.Close()
	im, _, err := s.Put(f)
	return im, err
}

// Remove deletes a picture and its stand-ins.
func (s *Store) Remove(sha string) error {
	if p := s.Path(sha); p != "" {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	standins, _ := filepath.Glob(filepath.Join(s.Root, "standins", sha+"-*"))
	for _, p := range standins {
		os.Remove(p)
	}
	return nil
}

// Path is where a picture is, or "".
func (s *Store) Path(sha string) string {
	if len(sha) < 2 {
		return ""
	}
	m, _ := filepath.Glob(filepath.Join(s.Root, sha[:2], sha+".*"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// Input is what a model is shown for a picture: the picture itself, or,
// over maxPixels, a downscaled stand-in (a 13 MP photo is ~16k image
// tokens against an 8k context). Returns its path and sha256.
func (s *Store) Input(sha string, maxPixels int, method string) (string, string, error) {
	if maxPixels > 0 && method != Resize {
		return "", "", fmt.Errorf("resize %q: this build does %q", method, Resize)
	}
	path := s.Path(sha)
	if path == "" {
		return "", "", fmt.Errorf("no picture %s", sha)
	}
	if maxPixels <= 0 {
		return path, sha, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	cfg, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil || cfg.Width*cfg.Height <= maxPixels {
		return path, sha, nil
	}
	out := filepath.Join(s.Root, "standins", fmt.Sprintf("%s-%d.png", sha, maxPixels))
	if _, err := os.Stat(out); err != nil {
		if err := downscale(path, out, cfg.Width, cfg.Height, maxPixels); err != nil {
			return "", "", err
		}
	}
	d, err := images.Describe(out)
	if err != nil {
		return "", "", err
	}
	return out, d.SHA256, nil
}

func downscale(src, out string, w, h, maxPixels int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	im, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return err
	}
	scale := math.Sqrt(float64(maxPixels) / float64(w*h))
	nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), im, im.Bounds(), draw.Src, nil)
	tmp, err := os.CreateTemp(filepath.Dir(out), ".standin-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	// fast compression: still lossless and deterministic (the bytes are part
	// of the cache key); saves ~10% of a stand-in (the resampling dominates)
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(tmp, dst); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), out)
}
