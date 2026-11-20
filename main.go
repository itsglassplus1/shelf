// Command shelf is a minimal, read-only comic/manga server that speaks the
// subset of Komga's REST API used by the Mihon/Tachiyomi Komga extension, so
// Suwayomi can read a local collection through that extension.
// See README.md for setup and behavior.
//
// Configuration (environment variables):
//
//	SHELF_CATEGORIES    category folders relative to SHELF_ROOT,  (required)
//	                    separated by ":", e.g. "comics/marvel:manga"
//	SHELF_ROOT          library root, never written to            (default /library)
//	SHELF_DATA_DIR      writable folder outside the library       (default /data)
//	                      credentials             login (see auth.go)
//	                      cache/                  RAR chapters unpacked on first read
//	                      series/<series name>/   cover and ComicInfo.xml copies
//	SHELF_CACHE_MAX_MB  soft size cap of cache/, in MB            (default 1024)
//	SHELF_LISTEN        listen address                            (default :25600)
//	SHELF_AUTH          "off" to serve without a login            (default on)
//
// By default every request needs the username and password from
// <data>/credentials, created with a random password on first start; edit
// the file to change them. /healthz stays open. With SHELF_AUTH=off anyone
// who can reach the port can read the library: only for setups where
// nothing else can reach it, such as Suwayomi in the same pod.
//
// Every directory directly inside a category is a series; series folder names
// must be unique across categories (IDs derive from them). Inside a series,
// every .cbz/.zip/.cbr/.rar/.cbt/.tar/.epub file and every sub-directory of
// images is a chapter (EPUBs: see epub.go). Series and pages use natural
// order ("Chapter 2" < "Chapter 10"); chapter numbers are recognized like
// Mihon's (see chapternum.go). Metadata comes from ComicInfo.xml files (see
// comicinfo.go).
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type config struct {
	root        string
	dataDir     string
	categories  []string
	listen      string
	cacheDir    string // <data>/cache
	seriesDir   string // <data>/series
	credentials string // <data>/credentials; "" = no login (SHELF_AUTH=off)
	cacheMax    int64  // bytes
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	data := envOr("SHELF_DATA_DIR", "/data")
	cfg := config{
		root:        envOr("SHELF_ROOT", "/library"),
		dataDir:     data,
		listen:      envOr("SHELF_LISTEN", ":25600"),
		cacheDir:    filepath.Join(data, "cache"),
		seriesDir:   filepath.Join(data, "series"),
		credentials: filepath.Join(data, credentialsName),
	}
	mb, err := strconv.ParseInt(envOr("SHELF_CACHE_MAX_MB", "1024"), 10, 64)
	if err != nil || mb <= 0 {
		log.Fatalf("ERROR: SHELF_CACHE_MAX_MB must be a positive number of MB, got %q", os.Getenv("SHELF_CACHE_MAX_MB"))
	}
	cfg.cacheMax = mb << 20
	for _, c := range strings.Split(os.Getenv("SHELF_CATEGORIES"), ":") {
		if c = strings.Trim(strings.TrimSpace(c), "/"); c != "" {
			cfg.categories = append(cfg.categories, c)
		}
	}
	if len(cfg.categories) == 0 {
		log.Fatal("ERROR: SHELF_CATEGORIES is empty; set it to e.g. \"comics/marvel:manga\"")
	}
	switch v := strings.ToLower(envOr("SHELF_AUTH", "on")); v {
	case "on":
	case "off":
		cfg.credentials = ""
	default:
		log.Fatalf("ERROR: SHELF_AUTH must be \"on\" or \"off\", got %q", v)
	}
	return cfg
}

func main() {
	log.SetFlags(0) // journald adds timestamps
	cfg := loadConfig()
	checkDataDir(cfg.dataDir)

	var auth *authenticator
	if cfg.credentials != "" {
		var err error
		if auth, err = newAuthenticator(cfg.credentials); err != nil {
			log.Fatalf("ERROR: login not usable, not starting: %v", err)
		}
	}

	cat := newCatalog(cfg)
	if err := cat.refresh(); err != nil {
		log.Fatalf("ERROR: initial scan failed, not starting:\n%v", err)
	}
	idx := cat.current()
	books := 0
	for _, s := range idx.series {
		books += len(s.Books)
	}
	log.Printf("shelf: %d categories, %d series, %d chapters; listening on %s",
		len(idx.libs), len(idx.series), books, cfg.listen)
	if auth != nil {
		log.Printf("shelf: login required (user %q, from %s)", auth.user, cfg.credentials)
	} else {
		log.Printf("WARN: shelf: SHELF_AUTH=off: anyone who can reach %s can read the library", cfg.listen)
	}
	cat.logUnusedSeriesFolders()

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           newServer(cat, auth),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// checkDataDir logs how to fix a data folder shelf can't write to. The
// container image runs shelf as uid 1000, so a volume made for an older
// image (which ran as root) needs its owner changed once. What then fails
// (the login, RAR chapters, covers) is reported where it fails.
func checkDataDir(dir string) {
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		var f *os.File
		if f, err = os.CreateTemp(dir, ".write-test-"); err == nil {
			f.Close()
			os.Remove(f.Name())
			return
		}
	}
	log.Printf("ERROR: data folder %s is not writable by uid %d: %v\n"+
		"    give it to that user, e.g. for a volume:\n"+
		"    podman run --rm -v <volume>:/data docker.io/library/alpine chown -R %d:%d /data",
		dir, os.Getuid(), err, os.Getuid(), os.Getgid())
}
