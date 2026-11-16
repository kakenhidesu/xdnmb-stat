package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// All health measurements run in the tester's browser. This service only publishes image addresses.
//
//go:embed index.html status-core.js status-page.js local-probe.user.js
var assets embed.FS

const (
	cacheTTL         = 5 * time.Minute
	maxAge           = 15 * time.Minute
	retryAfter       = 30 * time.Second
	refreshTimeout   = 20 * time.Second
	maxResponseBytes = 2 << 20
	recentCount      = 3
	maxCDNs          = 3
)

var apiOrigins = []string{"https://www.nmbxd.com", "https://www.nmbxd1.com", "https://api.nmb.best"}
var imagePath = regexp.MustCompile(`(?i)^\d{4}-\d{2}-\d{2}/[a-zA-Z0-9_-]+\.(jpe?g|png|gif|webp|avif)$`)

type catalog struct {
	GeneratedAt int64    `json:"generatedAt"`
	CDNs        []string `json:"cdns"`
	Paths       []string `json:"paths"`
}

type catalogStore struct {
	mu          sync.Mutex
	cached      *catalog
	pending     chan struct{}
	lastAttempt time.Time
	now         func() time.Time
	load        func(context.Context) (*catalog, error)
}

func newCatalogStore(load func(context.Context) (*catalog, error)) *catalogStore {
	return &catalogStore{now: time.Now, load: load}
}

func (s *catalogStore) get(ctx context.Context) *catalog {
	s.mu.Lock()
	now := s.now()
	if s.cached != nil && now.Sub(time.UnixMilli(s.cached.GeneratedAt)) < cacheTTL {
		result := s.cached
		s.mu.Unlock()
		return result
	}
	if s.pending == nil && (s.lastAttempt.IsZero() || now.Sub(s.lastAttempt) >= retryAfter) {
		s.lastAttempt = now
		s.pending = make(chan struct{})
		go s.refresh()
	}
	// An existing, unexpired catalogue can be served immediately during refresh.
	if s.cached != nil && now.Sub(time.UnixMilli(s.cached.GeneratedAt)) <= maxAge {
		result := s.cached
		s.mu.Unlock()
		return result
	}
	pending := s.pending
	s.mu.Unlock()
	if pending == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	case <-pending:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && s.now().Sub(time.UnixMilli(s.cached.GeneratedAt)) <= maxAge {
		return s.cached
	}
	return nil
}

func (s *catalogStore) refresh() {
	// A visitor disconnecting must not cancel the shared refresh for other visitors.
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	result, err := s.load(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && result != nil {
		s.cached = result
	}
	close(s.pending)
	s.pending = nil
}

type cdnEntry struct {
	URL string `json:"url"`
}
type imagePost struct {
	Img     string      `json:"img"`
	Ext     string      `json:"ext"`
	Replies []imagePost `json:"Replies"`
}
type sourceImages struct{ cdns, paths []string }

func normalizeCDN(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("invalid CDN address")
	}
	return strings.TrimRight(u.String(), "/") + "/", nil
}

func recentPaths(posts []imagePost, now time.Time) []string {
	seen := make(map[string]bool)
	add := func(p imagePost) {
		name := p.Img + p.Ext
		if !imagePath.MatchString(name) {
			return
		}
		day, err := time.Parse("2006-01-02", name[:10])
		if err != nil || day.After(now.Add(24*time.Hour)) || now.Sub(day) > 7*24*time.Hour {
			return
		}
		if name == "2022-12-04/638c87770b738.jpg" || name == "2026-02-12/698deed346c6c.png" {
			return
		}
		seen[name] = true
	}
	for _, p := range posts {
		add(p)
		for _, reply := range p.Replies {
			add(reply)
		}
	}
	paths := make([]string, 0, len(seen))
	for name := range seen {
		paths = append(paths, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	if len(paths) > recentCount {
		paths = paths[:recentCount]
	}
	return paths
}

func fetchJSON(ctx context.Context, client *http.Client, endpoint string, dest any) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	query := u.Query()
	query.Set("_", strconv.FormatInt(time.Now().UnixMilli(), 10))
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "xdnmb-stat-sample-catalog/1.0")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !(mediaType == "application/json" || strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json")) {
		return errors.New("expected JSON response")
	}
	reader := &io.LimitedReader{R: resp.Body, N: maxResponseBytes + 1}
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(dest); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("unexpected data after JSON")
	}
	if reader.N == 0 {
		return errors.New("response exceeds size limit")
	}
	return nil
}

func collectCatalog(ctx context.Context, client *http.Client, origins []string, now time.Time) (*catalog, error) {
	results := make(chan sourceImages, len(origins))
	for _, origin := range origins {
		go func(origin string) {
			var result sourceImages
			var configs []cdnEntry
			if fetchJSON(ctx, client, origin+"/Api/getCdnPath", &configs) == nil && len(configs) > 0 {
				for _, entry := range configs {
					base, err := normalizeCDN(entry.URL)
					if err != nil {
						result.cdns = nil
						break
					}
					result.cdns = append(result.cdns, base)
				}
			}
			var posts []imagePost
			if fetchJSON(ctx, client, origin+"/Api/showf?id=4&page=1", &posts) == nil {
				result.paths = recentPaths(posts, now)
			}
			results <- result
		}(origin)
	}
	cdns, paths := make(map[string]bool), make(map[string]bool)
	for range origins {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-results:
			for _, base := range result.cdns {
				cdns[base] = true
			}
			for _, name := range result.paths {
				paths[name] = true
			}
		}
	}
	if len(cdns) == 0 || len(cdns) > maxCDNs || len(paths) < recentCount {
		return nil, errors.New("incomplete recent image catalogue")
	}
	result := &catalog{GeneratedAt: now.UnixMilli(), CDNs: make([]string, 0, len(cdns)), Paths: make([]string, 0, len(paths))}
	for base := range cdns {
		result.CDNs = append(result.CDNs, base)
	}
	for name := range paths {
		result.Paths = append(result.Paths, name)
	}
	sort.Strings(result.CDNs)
	sort.Sort(sort.Reverse(sort.StringSlice(result.Paths)))
	result.Paths = result.Paths[:recentCount]
	return result, nil
}

func newHandler(store *catalogStore) http.Handler {
	types := map[string]string{
		"index.html":          "text/html; charset=utf-8",
		"status-core.js":      "application/javascript; charset=utf-8",
		"status-page.js":      "application/javascript; charset=utf-8",
		"local-probe.user.js": "application/javascript; charset=utf-8",
	}
	active := make(chan struct{}, 64)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/image-samples.json" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
		default:
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Busy, retry shortly", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/image-samples.json" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			data := store.get(r.Context())
			if data == nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				if r.Method != http.MethodHead {
					io.WriteString(w, `{"error":"近期图片目录暂不可用"}`)
				}
			} else if r.Method != http.MethodHead {
				json.NewEncoder(w).Encode(data)
			}
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		contentType, ok := types[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		file, err := assets.Open(name)
		if err != nil {
			http.Error(w, "Unable to serve this request", http.StatusInternalServerError)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, name, time.Time{}, file.(io.ReadSeeker))
	})
}

func main() {
	// A GC soft target, not an RSS cap. Operators can override it with GOMEMLIMIT.
	if _, configured := os.LookupEnv("GOMEMLIMIT"); !configured {
		debug.SetMemoryLimit(32 << 20)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 6
	transport.MaxIdleConnsPerHost = 1
	transport.MaxConnsPerHost = 2
	transport.IdleConnTimeout = time.Minute
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect not allowed") },
	}
	store := newCatalogStore(func(ctx context.Context) (*catalog, error) {
		return collectCatalog(ctx, client, apiOrigins, time.Now())
	})
	host, port := os.Getenv("XDAO_HOST"), os.Getenv("XDAO_PORT")
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "18787"
	}
	server := &http.Server{Addr: net.JoinHostPort(host, port), Handler: newHandler(store),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()
	log.Printf("Xdao status: http://%s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
