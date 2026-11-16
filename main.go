package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed index.html status-page.js status.css docs/assets/fonts/*
var assets embed.FS

func newHandler(store *monitorStore) http.Handler {
	types := map[string]string{"index.html": "text/html; charset=utf-8", "status-page.js": "application/javascript; charset=utf-8", "status.css": "text/css; charset=utf-8"}
	fonts, _ := assets.ReadDir("docs/assets/fonts")
	fontTypes := map[string]string{".woff2": "font/woff2", ".css": "text/css; charset=utf-8", ".txt": "text/plain; charset=utf-8"}
	for _, font := range fonts {
		if kind, ok := fontTypes[path.Ext(font.Name())]; ok && !font.IsDir() {
			types["assets/fonts/"+font.Name()] = kind
		}
	}
	active := make(chan struct{}, 32)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(405)
			return
		}
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
		default:
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Busy, retry shortly", 503)
			return
		}
		if r.URL.Path == "/api/status" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != http.MethodHead {
				json.NewEncoder(w).Encode(store.view(time.Now()))
			}
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		kind, ok := types[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		assetName := name
		if strings.HasPrefix(name, "assets/fonts/") {
			assetName = "docs/" + name
		}
		file, err := assets.Open(assetName)
		if err != nil {
			http.Error(w, "Unable to serve this request", 500)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", kind)
		w.Header().Set("Cache-Control", "no-cache")
		if strings.HasSuffix(name, ".woff2") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeContent(w, r, name, time.Time{}, file.(io.ReadSeeker))
	})
}
func main() {
	if _, configured := os.LookupEnv("GOMEMLIMIT"); !configured {
		debug.SetMemoryLimit(32 << 20)
	}
	interval := 5 * time.Minute
	if text := os.Getenv("XDAO_INTERVAL_SECONDS"); text != "" {
		seconds, err := strconv.Atoi(text)
		if err != nil || seconds < 60 || seconds > 3600 {
			log.Fatal("XDAO_INTERVAL_SECONDS must be 60..3600")
		}
		interval = time.Duration(seconds) * time.Second
	}
	dataDir := os.Getenv("XDAO_DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	location := os.Getenv("XDAO_PROBE_LOCATION")
	if location == "" {
		location = "台湾 · Google Cloud"
	}
	store, err := newMonitorStore(dataDir, interval, location, time.Now())
	if err != nil {
		log.Fatalf("load history: %v", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 8
	transport.MaxIdleConnsPerHost = 2
	transport.MaxConnsPerHost = 3
	transport.IdleConnTimeout = time.Minute
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: checkRedirect}
	host, port := os.Getenv("XDAO_HOST"), os.Getenv("XDAO_PORT")
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "18787"
	}
	server := &http.Server{Addr: net.JoinHostPort(host, port), Handler: newHandler(store), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Fatal(err)
	}
	go store.run(ctx, func(c context.Context, now time.Time) record { return probeOnce(c, client, now) })
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("Xdao monitor: %s; interval=%s; data=%s", server.Addr, interval, dataDir)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
