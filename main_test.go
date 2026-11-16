package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)

func fixtureCatalog() *catalog {
	return &catalog{GeneratedAt: testNow.UnixMilli(), CDNs: []string{"https://image.nmb.best/"},
		Paths: []string{"2026-11-16/a.jpg", "2026-11-15/b.png", "2026-11-14/c.gif"}}
}

func TestRecentPaths(t *testing.T) {
	posts := []imagePost{{Img: "2022-12-04/638c87770b738", Ext: ".jpg", Replies: []imagePost{
		{Img: "2026-11-16/a", Ext: ".jpg"}, {Img: "2026-11-15/b", Ext: ".png"},
		{Img: "2026-11-14/c", Ext: ".gif"}, {Img: "2026-11-16/a", Ext: ".jpg"},
		{Img: "2026-11-13/d", Ext: ".jpg"}, {Img: "2026-02-31/x", Ext: ".jpg"},
		{Img: "2027-01-01/x", Ext: ".jpg"}, {Img: "../../secret", Ext: ".jpg"},
	}}}
	if got := recentPaths(posts, testNow); !reflect.DeepEqual(got, fixtureCatalog().Paths) {
		t.Fatalf("paths: %v", got)
	}
}

func TestNormalizeCDN(t *testing.T) {
	got, err := normalizeCDN("https://image.nmb.best")
	if err != nil || got != "https://image.nmb.best/" {
		t.Fatalf("%q %v", got, err)
	}
	for _, input := range []string{"http://host/", "https://", "https://user:password@host/", "https://host/?x=1", "https://host/#x"} {
		if _, err := normalizeCDN(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestFetchJSON(t *testing.T) {
	cases := []struct {
		name, contentType, body string
		status                  int
		pass                    bool
	}{
		{"valid", "application/json; charset=utf-8", `[{"url":"https://image.nmb.best/"}]`, 200, true},
		{"http error", "application/json", `[]`, 503, false},
		{"html challenge", "text/html", `<h1>challenge</h1>`, 200, false},
		{"broken json", "application/json", `[`, 200, false},
		{"trailing json", "application/json", `[] {}`, 200, false},
		{"business error", "application/json", `{"error":"no"}`, 200, false},
		{"oversized", "application/json", `[]` + strings.Repeat(" ", maxResponseBytes), 200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("id") != "4" || r.URL.Query().Get("_") == "" {
					t.Error("query lost")
				}
				if r.Header.Get("Cookie") != "" {
					t.Error("unexpected cookie")
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var data []cdnEntry
			err := fetchJSON(context.Background(), server.Client(), server.URL+"?id=4", &data)
			if (err == nil) != tc.pass {
				t.Fatalf("error = %v, expected pass %v", err, tc.pass)
			}
		})
	}
}

func TestFetchTimeoutIncludesBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var data []cdnEntry
	if err := fetchJSON(ctx, server.Client(), server.URL, &data); err == nil {
		t.Fatal("body timeout not enforced")
	}
}

func TestCollectCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/bad/") {
			w.WriteHeader(503)
			return
		}
		if strings.HasSuffix(r.URL.Path, "getCdnPath") {
			io.WriteString(w, `[{"url":"https://image.nmb.best/"},{"url":"https://image.nmb.best/"}]`)
		} else {
			io.WriteString(w, `[{"img":"2022-12-04/old","ext":".jpg","Replies":[{"img":"2026-11-16/a","ext":".jpg"},{"img":"2026-11-15/b","ext":".png"},{"img":"2026-11-14/c","ext":".gif"}]}]`)
		}
	}))
	defer server.Close()
	data, err := collectCatalog(context.Background(), server.Client(), []string{server.URL, server.URL + "/bad"}, testNow)
	if err != nil || !reflect.DeepEqual(data, fixtureCatalog()) {
		t.Fatalf("%+v %v", data, err)
	}
	if _, err := collectCatalog(context.Background(), server.Client(), []string{server.URL + "/bad"}, testNow); err == nil {
		t.Fatal("unavailable data treated as complete")
	}
}

func TestSharedRefreshAndFreshCache(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	store := newCatalogStore(func(context.Context) (*catalog, error) { calls.Add(1); <-release; return fixtureCatalog(), nil })
	store.now = func() time.Time { return testNow }
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := store.get(context.Background()); !reflect.DeepEqual(got, fixtureCatalog()) {
				t.Error("incorrect shared result")
			}
		}()
	}
	close(release)
	wg.Wait()
	store.get(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("refreshes = %d", calls.Load())
	}
}

func TestStaleCacheAndExpiry(t *testing.T) {
	var calls atomic.Int32
	store := newCatalogStore(func(context.Context) (*catalog, error) { calls.Add(1); return nil, errors.New("offline") })
	store.cached = fixtureCatalog()
	store.now = func() time.Time { return testNow.Add(6 * time.Minute) }
	if got := store.get(context.Background()); got == nil || got.GeneratedAt != testNow.UnixMilli() {
		t.Fatal("stale timestamp changed")
	}
	store.mu.Lock()
	pending := store.pending
	store.mu.Unlock()
	if pending != nil {
		<-pending
	}
	store.get(context.Background())
	if calls.Load() != 1 {
		t.Fatal("failure retry was not throttled")
	}
	store.now = func() time.Time { return testNow.Add(16 * time.Minute) }
	if got := store.get(context.Background()); got != nil {
		t.Fatal("expired catalogue served")
	}
}

func TestCanceledVisitorDoesNotCancelSharedRefresh(t *testing.T) {
	release := make(chan struct{})
	store := newCatalogStore(func(ctx context.Context) (*catalog, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return fixtureCatalog(), nil
		}
	})
	store.now = func() time.Time { return testNow }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if store.get(ctx) != nil {
		t.Fatal("canceled visitor got result")
	}
	close(release)
	if store.get(context.Background()) == nil {
		t.Fatal("other visitor lost shared result")
	}
}

func TestHTTPRoutesAndEmbeddedAssets(t *testing.T) {
	store := newCatalogStore(func(context.Context) (*catalog, error) { return fixtureCatalog(), nil })
	store.now = func() time.Time { return testNow }
	handler := newHandler(store)
	for route, name := range map[string]string{"/": "index.html", "/index.html": "index.html", "/status-core.js": "status-core.js", "/status-page.js": "status-page.js", "/local-probe.user.js": "local-probe.user.js"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", route, nil))
		body, err := os.ReadFile(name)
		if err != nil || w.Code != 200 || string(body) != w.Body.String() {
			t.Fatalf("embedded asset mismatch for %s", route)
		}
	}
	for _, route := range []string{"/server.cjs", "/main.go", "/.git/config", "/proxy?url=https://example.com", "/../main.go"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", route, nil))
		if w.Code != 404 {
			t.Errorf("%s returned %d", route, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/image-samples.json", nil))
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
		t.Fatal("invalid catalogue response")
	}
	if len(body) != 3 || body["generatedAt"] == nil || body["paths"] == nil || body["cdns"] == nil {
		t.Fatal("unexpected server fields")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("missing catalogue CORS header")
	}
	for _, route := range []string{"/", "/image-samples.json"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("HEAD", route, nil))
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatal("invalid HEAD response")
		}
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", route, nil))
		if w.Code != 405 {
			t.Fatal("POST accepted")
		}
	}
}

func TestUnavailableCatalogIsNotHealthData(t *testing.T) {
	store := newCatalogStore(func(context.Context) (*catalog, error) { return nil, errors.New("offline") })
	w := httptest.NewRecorder()
	newHandler(store).ServeHTTP(w, httptest.NewRequest("GET", "/image-samples.json", nil))
	if w.Code != 503 || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("invalid unavailable response")
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body) != 1 || body["error"] == nil {
		t.Fatal("server leaked health results")
	}
}
