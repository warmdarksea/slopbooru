package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "image/gif"
	_ "image/png"

	"golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// decoding a 13 MP photo is ~50 MB of pixels; a page of 48 thumbnails
// arriving at once must not decode 48 of them in parallel.
var thumbSlots = make(chan struct{}, runtime.NumCPU())

// thumbnail returns the path of a cached square thumbnail, making it on the
// first request; "" means serve the original (the format is one Go cannot
// decode, the file is broken, or there is no cache dir).
func thumbnail(path, sha string, size int, cacheDir string) string {
	if cacheDir == "" || len(sha) < 2 {
		return ""
	}
	out := filepath.Join(cacheDir, sha[:2], fmt.Sprintf("%s_%d.jpg", sha, size))
	if _, err := os.Stat(out); err == nil {
		return out
	}
	thumbSlots <- struct{}{}
	defer func() { <-thumbSlots }()
	if err := makeThumb(path, out, size); err != nil {
		logf("thumbnail failed for %s: %v", path, err)
		return ""
	}
	return out
}

func makeThumb(src, out string, size int) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	im, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	// ImageOps.fit: the centred square, scaled. The centre square is the
	// same square under any rotation or flip, so the EXIF orientation can
	// be applied after scaling, to 256x256 pixels instead of 13 MP.
	b := im.Bounds()
	side := min(b.Dx(), b.Dy())
	crop := image.Rect(0, 0, side, side).Add(b.Min).
		Add(image.Pt((b.Dx()-side)/2, (b.Dy()-side)/2))
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	// transparent pixels onto white, rather than whatever colour they hold
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), im, crop, draw.Over, nil)
	final := orient(dst, exifOrientation(data))

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := jpeg.Encode(tmp, final, &jpeg.Options{Quality: 82}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), out)
}

// orient applies an EXIF orientation (1..8) to a square image.
func orient(src *image.RGBA, o int) image.Image {
	if o < 2 || o > 8 {
		return src
	}
	n := src.Bounds().Dx()
	dst := image.NewRGBA(src.Bounds())
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			var sx, sy int
			switch o {
			case 2: // mirrored
				sx, sy = n-1-x, y
			case 3: // rotated 180
				sx, sy = n-1-x, n-1-y
			case 4: // flipped
				sx, sy = x, n-1-y
			case 5: // transposed
				sx, sy = y, x
			case 6: // shot rotated 90 clockwise: display it turned back
				sx, sy = y, n-1-x
			case 7: // transversed
				sx, sy = n-1-y, n-1-x
			case 8:
				sx, sy = n-1-y, x
			}
			dst.SetRGBA(x, y, src.RGBAAt(sx, sy))
		}
	}
	return dst
}

// exifOrientation finds tag 0x0112 in a JPEG's APP1 segment; 1 when absent.
// (Other formats can carry EXIF too, but a phone photo is a JPEG.)
func exifOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	r := bytes.NewReader(data[2:])
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil || hdr[0] != 0xFF {
			return 1
		}
		marker, n := hdr[1], int(binary.BigEndian.Uint16(hdr[2:]))
		if marker == 0xDA || marker == 0xD9 || n < 2 { // start of scan: no more headers
			return 1
		}
		seg := make([]byte, n-2)
		if _, err := io.ReadFull(r, seg); err != nil {
			return 1
		}
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
	}
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off < 0 || off+2 > len(t) {
		return 1
	}
	count := int(bo.Uint16(t[off:]))
	for i := 0; i < count; i++ {
		ent := off + 2 + 12*i
		if ent+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[ent:]) == 0x0112 {
			return int(bo.Uint16(t[ent+8:]))
		}
	}
	return 1
}

// mimeTypes is a fixed table on purpose: Go's mime package reads
// /etc/mime.types on linux and the registry on windows, and what this
// serves must not depend on the host.
var mimeTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".jpe": "image/jpeg",
	".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
	".bmp": "image/bmp", ".tif": "image/tiff", ".tiff": "image/tiff",
	".avif": "image/avif", ".heic": "image/heic", ".heif": "image/heif",
	".jxl": "image/jxl", ".svg": "image/svg+xml", ".ico": "image/vnd.microsoft.icon",
	".mp4": "video/mp4", ".webm": "video/webm",
}

func guessMime(path string) string {
	if m, ok := mimeTypes[strings.ToLower(filepath.Ext(path))]; ok {
		return m
	}
	return "application/octet-stream"
}
