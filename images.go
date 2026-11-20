package main

import (
	"encoding/binary"
	"io"
	"os"
	"path"
	"strings"
)

// Which files are images, decided like Mihon's ImageUtil.isImage: a file
// with a known image extension is one; any other file is one if its first
// bytes are those of an image (Mihon asks its image decoder for the
// format). So pages and covers named without an extension, or with an odd
// one like .jfif, are found too.
//
// Only the types in imageTypes count, the ones the Komga extension and its
// clients display; Mihon also takes JPEG 2000, which they don't list.

// sniffLen is how much of a file sniffImage needs.
const sniffLen = 64

// notImageExts are extensions never worth sniffing. Skipping them gives the
// same answer as sniffing (their files aren't images) without reading them,
// which matters in solid RARs: reading one file there means decoding the
// archive up to it.
var notImageExts = map[string]bool{
	".xml": true, ".txt": true, ".nfo": true, ".md": true, ".log": true,
	".htm": true, ".html": true, ".xhtml": true, ".css": true, ".js": true, ".json": true,
	".opf": true, ".ncx": true, ".db": true, ".ini": true, ".url": true,
	".sfv": true, ".md5": true, ".sha1": true, ".pdf": true,
	".zip": true, ".cbz": true, ".rar": true, ".cbr": true, ".7z": true, ".cb7": true,
	".tar": true, ".cbt": true, ".epub": true,
}

// imageExts gives a file extension for each media type in imageTypes, for
// naming copies of images found by content.
var imageExts = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"image/jxl": ".jxl", "image/avif": ".avif", "image/heif": ".heif",
}

// imageMediaType returns the media type of a file named name if it is an
// image, else "". head returns the file's first sniffLen bytes (fewer for a
// short file, nil if unreadable); it is only called when the name doesn't
// tell.
func imageMediaType(name string, head func() []byte) string {
	ext := strings.ToLower(path.Ext(name))
	if mt := imageTypes[ext]; mt != "" {
		return mt
	}
	if notImageExts[ext] {
		return ""
	}
	return sniffImage(head())
}

// fileHead returns the first sniffLen bytes of a file, nil if unreadable.
func fileHead(p string) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	return readHead(f)
}

// readHead returns the first sniffLen bytes of r, nil if unreadable.
func readHead(r io.Reader) []byte {
	buf := make([]byte, sniffLen)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil
	}
	return buf[:n]
}

// sniffImage returns the media type of an image starting with head, or ""
// if it isn't one of the types in imageTypes.
func sniffImage(head []byte) string {
	h := string(head)
	switch {
	case strings.HasPrefix(h, "\xff\xd8\xff"):
		return "image/jpeg"
	case strings.HasPrefix(h, "\x89PNG\r\n\x1a\n"):
		return "image/png"
	case strings.HasPrefix(h, "GIF87a"), strings.HasPrefix(h, "GIF89a"):
		return "image/gif"
	case len(h) >= 12 && h[:4] == "RIFF" && h[8:12] == "WEBP":
		return "image/webp"
	case strings.HasPrefix(h, "\xff\x0a"), // JPEG XL codestream
		strings.HasPrefix(h, "\x00\x00\x00\x0cJXL \r\n\x87\n"): // JPEG XL container
		return "image/jxl"
	}
	return sniffISOBMFF(head)
}

// sniffISOBMFF recognizes AVIF and HEIF by the brands of their ftyp box.
// AVIF files may also carry HEIF's general brands (mif1), so an AVIF brand
// anywhere wins.
func sniffISOBMFF(head []byte) string {
	if len(head) < 16 || string(head[4:8]) != "ftyp" {
		return ""
	}
	size := int(binary.BigEndian.Uint32(head[:4]))
	if size < 16 || size > len(head) {
		size = len(head) // brands past what was read are not seen
	}
	heif := false
	// Major brand at 8, minor version at 12, compatible brands from 16.
	for i := 8; i+4 <= size; i += 4 {
		if i == 12 {
			continue
		}
		switch string(head[i : i+4]) {
		case "avif", "avis":
			return "image/avif"
		case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1":
			heif = true
		}
	}
	if heif {
		return "image/heif"
	}
	return ""
}

// imageFileExt is the extension for a copy of an image file named name: its
// own if it is an image extension, else one for its media type.
func imageFileExt(name, mediaType string) string {
	if ext := strings.ToLower(path.Ext(name)); imageTypes[ext] != "" {
		return ext
	}
	return imageExts[mediaType]
}

// coverMediaType returns the media type of the file at p, named name, if it
// is a cover: "cover" with any or no extension, in any case, and an image,
// as Mihon's LocalCoverManager takes it. Else "".
func coverMediaType(p, name string) string {
	if !strings.EqualFold(strings.TrimSuffix(name, path.Ext(name)), "cover") {
		return ""
	}
	return imageMediaType(name, func() []byte { return fileHead(p) })
}
