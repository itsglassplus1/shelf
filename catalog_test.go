package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A rescan that fails keeps the previous library, logs once, retries on
// every check, and recovers when the problem is fixed.
func TestRescanFailureKeepsLibrary(t *testing.T) {
	cat, _ := testLibrary(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	before := cat.current()

	// A series name collision: "Other" (in manga) now also in comics.
	dup := filepath.Join(cat.cfg.root, "comics", "Other")
	os.MkdirAll(dup, 0o755)
	touchLater(t, filepath.Join(cat.cfg.root, "comics"))
	cat.checkForChanges()
	cat.checkForChanges() // retried, not logged again
	if cat.current() != before {
		t.Error("failed rescan replaced the library")
	}
	if n := strings.Count(logs.String(), "rescan failed"); n != 1 || !strings.Contains(logs.String(), `collision for "Other"`) {
		t.Errorf("log: %q; want one rescan failure naming the collision", logs.String())
	}

	// Fixed, and a new series added meanwhile: picked up, recovery logged.
	os.Remove(dup)
	writeZip(t, filepath.Join(cat.cfg.root, "comics", "New", "c1.cbz"), "p.png", "x")
	touchLater(t, filepath.Join(cat.cfg.root, "comics"))
	cat.checkForChanges()
	if cat.current() == before || cat.lastError != "" || !strings.Contains(logs.String(), "problem resolved") {
		t.Errorf("not recovered: lastError %q, log %q", cat.lastError, logs.String())
	}
	found := false
	for _, s := range cat.current().series {
		found = found || s.Name == "New"
	}
	if !found {
		t.Error("series added while the rescan was failing not picked up")
	}
}

// touchLater moves a folder's mtime forward, so a change is seen even on
// file systems with coarse timestamps.
func touchLater(t *testing.T, p string) {
	t.Helper()
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
}
