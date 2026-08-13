// Package main is a simple HTTP-over-TCP echo backend used for local testing.
// Start it with: go run ./cmd/backend :9000
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: backend <:port>")
		os.Exit(1)
	}
	port := os.Args[1]

	srv := &http.Server{Addr: port, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	http.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintf(w, "Hello from %s", port) })
	http.HandleFunc("/home", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "This is the home page") })
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("[%s] %s %s\n", port, r.Method, r.URL.Path)
		_, _ = fmt.Fprintf(w, "Hello from %s", port)
	})
	fmt.Printf("backend listening on %s\n", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	}
}
