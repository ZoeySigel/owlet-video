package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"time"
)

func profilerMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/debug/pprof/", pprof.Index)
	m.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	m.HandleFunc("/debug/pprof/profile", pprof.Profile)
	m.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	m.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return m
}
func startProfiler(ctx context.Context, mode string) error {
	if os.Getenv("PPROF_ENABLED") != "true" {
		return nil
	}
	addr := "127.0.0.1:6060"
	if mode == "worker" {
		addr = "127.0.0.1:6061"
	}
	// Deliberately never mount profiling on the public Gin router.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: profilerMux(), ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = server.Close() }()
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("pprof: %v", err)
		}
	}()
	log.Printf("%s pprof listening on %s", mode, addr)
	return nil
}
