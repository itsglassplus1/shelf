package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
)

// Tar chapters (.cbt/.tar) are read in place. An uncompressed tar is a
// sequence of headers, each followed by its file's data, so one pass over
// the headers (seeking past the data, not reading it) records where every
// file is. Pages are then served as slices of the archive, like plain files.
//
// Compressed tars (.tar.gz and the like renamed to .cbt) can't be read from
// the middle and aren't supported; archiveFormat reports them by name.

// tarEntry is a regular file in a tar archive.
type tarEntry struct {
	name         string // path in the archive, without a leading "./"
	offset, size int64  // where its data is in the archive file
}

// isTarHeader reports whether block (the first 512 bytes of a file) is a tar
// header: POSIX "ustar\x00" or GNU "ustar " magic at offset 257.
func isTarHeader(block []byte) bool {
	return len(block) >= 262 && string(block[257:262]) == "ustar"
}

// compressedFormat names the compression of a file starting with head, or "".
func compressedFormat(head []byte) string {
	switch h := string(head); {
	case strings.HasPrefix(h, "\x1f\x8b"):
		return "gzip"
	case strings.HasPrefix(h, "BZh"):
		return "bzip2"
	case strings.HasPrefix(h, "\xfd7zXZ\x00"):
		return "xz"
	case strings.HasPrefix(h, "\x28\xb5\x2f\xfd"):
		return "zstd"
	case strings.HasPrefix(h, "7z\xbc\xaf\x27\x1c"):
		return "7z"
	}
	return ""
}

// tarEntries lists the regular files of a tar archive with their positions.
func tarEntries(file string) ([]tarEntry, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// tar.Reader reads headers block by block and seeks past file data when
	// it can, so this reads only the headers, and f's position after Next is
	// the start of that file's data.
	tr := tar.NewReader(f)
	var out []tarEntry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if !h.FileInfo().Mode().IsRegular() || isSparse(h) {
			continue // directories, links; sparse files aren't stored contiguously
		}
		off, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		out = append(out, tarEntry{name: strings.TrimPrefix(h.Name, "./"), offset: off, size: h.Size})
	}
}

func isSparse(h *tar.Header) bool {
	if h.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// listTarPages returns the pages of a tar chapter in natural order.
func listTarPages(file string) ([]pageEntry, error) {
	entries, err := tarEntries(file)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pages []pageEntry
	for _, e := range entries {
		base := path.Base(e.name)
		if strings.HasPrefix(e.name, "__MACOSX/") || strings.HasPrefix(base, ".") {
			continue
		}
		head := func() []byte { return readHead(io.NewSectionReader(f, e.offset, e.size)) }
		if mt := imageMediaType(base, head); mt != "" {
			pages = append(pages, pageEntry{name: e.name, mediaType: mt, inTar: true, tarOffset: e.offset, tarSize: e.size})
		}
	}
	slices.SortFunc(pages, func(a, b pageEntry) int { return naturalCompare(a.name, b.name) })
	return pages, nil
}

// readComicInfoTar returns the raw ComicInfo.xml of a tar chapter, preferring
// one at the root, like readComicInfoZip. Returns nil, nil if it has none.
func readComicInfoTar(file string) ([]byte, error) {
	entries, err := tarEntries(file)
	if err != nil {
		return nil, err
	}
	var found *tarEntry
	for i, e := range entries {
		if strings.EqualFold(path.Base(e.name), comicInfoName) && !strings.HasPrefix(e.name, "__MACOSX/") {
			if found == nil || !strings.Contains(e.name, "/") {
				found = &entries[i]
			}
		}
	}
	if found == nil {
		return nil, nil
	}
	if found.size > 1<<20 {
		return nil, errors.New("ComicInfo.xml larger than 1 MB")
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, found.size)
	if _, err := f.ReadAt(data, found.offset); err != nil {
		return nil, fmt.Errorf("%s: %s: %w", file, found.name, err)
	}
	return data, nil
}
