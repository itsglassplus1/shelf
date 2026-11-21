package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"
)

// servePprof serves Go's profiling endpoints on their own listener, with no
// login, for measuring shelf while it is in use (SHELF_PPROF, off by
// default). Kept off the API's port so it can't be exposed with it by
// accident. For example, with SHELF_PPROF=127.0.0.1:6060:
//
//	go tool pprof -http=: http://127.0.0.1:6060/debug/pprof/heap
//	go tool pprof -http=: 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
//	curl http://127.0.0.1:6060/debug/pprof/goroutine?debug=1
//
// Profiles show file paths from the library and the data folder.
func servePprof(addr string) {
	m := http.NewServeMux()
	m.HandleFunc("/debug/pprof/", pprof.Index) // also serves heap, allocs, goroutine, ...
	m.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	m.HandleFunc("/debug/pprof/profile", pprof.Profile)
	m.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	m.HandleFunc("/debug/pprof/trace", pprof.Trace)
	m.HandleFunc("/debug/pprof/heapsummary", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, heapSummary())
	})
	log.Printf("WARN: shelf: profiling on http://%s/debug/pprof/ (SHELF_PPROF), without a login", addr)
	srv := &http.Server{Addr: addr, Handler: m, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("ERROR: profiling listener on %s: %v", addr, err)
	}
}

// heapSummary describes Go's memory use: the live heap, what the runtime has
// taken from the OS in all, and how much of that it has given back.
func heapSummary() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mb := func(n uint64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }
	return fmt.Sprintf("heap %s in use, %s from the OS (%s returned), %d GCs, %d goroutines",
		mb(m.HeapInuse), mb(m.Sys), mb(m.HeapReleased), m.NumGC, runtime.NumGoroutine())
}
