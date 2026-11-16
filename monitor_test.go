package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRedirectsValidateFinalResponse(t *testing.T) {
	for _, tc := range []struct {
		name, target, body, want string
		code, calls              int
	}{
		{"main moves to known domain", "https://www.nmbxd1.com/Api/getCdnPath", `[{"url":"https://image.nmb.best/"}]`, operational, 200, 2},
		{"redirect cannot hide error", "https://www.nmbxd1.com/Api/getCdnPath", `{}`, outage, 503, 2},
		{"redirect cannot hide invalid JSON", "https://www.nmbxd1.com/Api/getCdnPath", `bad`, outage, 200, 2},
		{"unrelated host is unverified", "https://unrelated.example/", `[]`, unknown, 200, 1},
		{"HTTP downgrade is unverified", "http://www.nmbxd1.com/Api/getCdnPath", `[]`, unknown, 200, 1},
		{"embedded credentials are rejected", "https://user:pass@www.nmbxd1.com/", `[]`, unknown, 200, 1},
		{"nonstandard port is rejected", "https://www.nmbxd1.com:1234/", `[]`, unknown, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{CheckRedirect: checkRedirect, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 301, Header: http.Header{"Location": {tc.target}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return &http.Response{StatusCode: tc.code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}
			got := measure(func() error {
				var result []cdnEntry
				return fetchJSON(context.Background(), client, apiOrigins[0]+"/Api/getCdnPath", &result)
			})
			if got.status != tc.want || calls != tc.calls {
				t.Fatalf("status=%s detail=%s calls=%d", got.status, got.detail, calls)
			}
			if tc.want == unknown && strings.Contains(got.detail, "连接失败") {
				t.Fatal("policy error mislabeled as network failure")
			}
		})
	}
}

func TestRedirectLoopStopsAndSameOriginCanRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{CheckRedirect: checkRedirect, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://nmb.ovear.info/next"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}
	got := measure(func() error {
		var body any
		return fetchJSON(context.Background(), client, "https://nmb.ovear.info/start", &body)
	})
	if got.status != unknown || calls != 6 || !strings.Contains(got.detail, "跳转次数") {
		t.Fatalf("result=%+v calls=%d", got, calls)
	}
}

func sample(at time.Time, status string) record {
	result := record{At: at.UnixMilli(), StartedAt: at.Add(-time.Second).UnixMilli(), Components: []observation{}}
	for _, def := range componentDefs {
		result.Components = append(result.Components, observation{def.ID, status, 100, "fixture"})
	}
	return result
}
func testStore(t *testing.T) *monitorStore {
	t.Helper()
	store, err := newMonitorStore(t.TempDir(), 5*time.Minute, "测试节点", testNow)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
func TestJournalSurvivesRestartAndBuildsIncidents(t *testing.T) {
	store := testStore(t)
	for i, status := range []string{operational, outage, outage, operational} {
		if err := store.append(sample(testNow.Add(time.Duration(i)*5*time.Minute), status)); err != nil {
			t.Fatal(err)
		}
	}
	now := testNow.Add(16 * time.Minute)
	before := store.view(now)
	restored, err := newMonitorStore(store.dir, store.interval, store.location, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, restored.view(now)) {
		t.Fatal("restart lost history")
	}
	if len(before.Incidents) != 6 || before.Incidents[0].Status != "resolved" {
		t.Fatalf("incidents: %+v", before.Incidents)
	}
	if *before.Components[0].Uptime != 50 {
		t.Fatalf("uptime: %v", *before.Components[0].Uptime)
	}
	if before.Components[0].History[89].Checks != 4 {
		t.Fatal("daily counters lost")
	}
	raw, _ := json.Marshal(before)
	var view map[string]any
	json.Unmarshal(raw, &view)
	if view["components"].([]any)[0].(map[string]any)["id"] != "main" {
		t.Fatal("component id missing from JSON")
	}
}
func TestUnknownAndUnobservedHistoryAreNotGreen(t *testing.T) {
	store := testStore(t)
	empty := store.view(testNow)
	if empty.Overall != unknown || !empty.Stale || empty.Components[0].Uptime != nil {
		t.Fatal("empty history looks healthy")
	}
	store.append(sample(testNow, operational))
	run := sample(testNow.Add(5*time.Minute), unknown)
	store.append(run)
	view := store.view(testNow.Add(6 * time.Minute))
	if view.Overall != unknown || view.Components[0].History[0].Status != unknown {
		t.Fatal("unknown history looks healthy")
	}
	if *view.Components[0].Uptime != 100 {
		t.Fatal("unknown should be excluded, with coverage shown separately")
	}
	if got := store.view(testNow.Add(20 * time.Minute)); !got.Stale || got.Components[0].Status != unknown {
		t.Fatal("stale data looks healthy")
	}
}
func TestGapDoesNotInventRecovery(t *testing.T) {
	store := testStore(t)
	store.append(sample(testNow, outage))
	store.append(sample(testNow.Add(time.Hour), operational))
	view := store.view(testNow.Add(time.Hour))
	if view.Incidents[0].Status != "interrupted" || view.Incidents[0].EndedAt != testNow.UnixMilli() {
		t.Fatal("gap reported as recovery")
	}
	if *view.Components[0].Coverage >= 50 {
		t.Fatal("monitoring gap missing from coverage")
	}
}
func TestTruncatedJournalDoesNotSwallowNextRecord(t *testing.T) {
	store := testStore(t)
	store.append(sample(testNow, operational))
	file := filepath.Join(store.dir, "2026-11-16.jsonl")
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"at":`)
	f.Close()
	restored, err := newMonitorStore(store.dir, store.interval, "test", testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !restored.historyWarning {
		t.Fatal("corrupt record was not reported")
	}
	restored.append(sample(testNow.Add(5*time.Minute), outage))
	next, err := newMonitorStore(store.dir, store.interval, "test", testNow.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if next.view(testNow.Add(6 * time.Minute)).Components[0].History[89].Checks != 2 {
		t.Fatal("record after corrupt tail lost")
	}
}
func TestWriteFailureCannotPublishHealthyResult(t *testing.T) {
	store := testStore(t)
	store.dir = filepath.Join(store.dir, "missing")
	if err := store.append(sample(testNow, operational)); err == nil {
		t.Fatal("expected write failure")
	}
	view := store.view(testNow)
	if !view.StorageError || view.Overall != unknown || view.CheckedAt != 0 {
		t.Fatal("unsaved success published")
	}
}
func TestRetentionOnlyPrunesOwnedExpiredJournals(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "2020-01-01.jsonl"), []byte("old"), 0o600)
	os.WriteFile(filepath.Join(dir, "notes.json"), []byte("keep"), 0o600)
	store, err := newMonitorStore(dir, 5*time.Minute, "test", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2020-01-01.jsonl")); !os.IsNotExist(err) {
		t.Fatal("expired journal retained")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.json")); err != nil {
		t.Fatal("unrelated file deleted")
	}
	if len(store.view(testNow).Components[0].History) != 90 {
		t.Fatal("wrong history window")
	}
}
func TestSchedulerRunsWithoutVisitorsAndDoesNotOverlap(t *testing.T) {
	store := testStore(t)
	store.interval = 25 * time.Millisecond
	var calls, active, peak atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		store.run(ctx, func(ctx context.Context, now time.Time) record {
			n := active.Add(1)
			if n > peak.Load() {
				peak.Store(n)
			}
			calls.Add(1)
			select {
			case <-ctx.Done():
			case <-time.After(40 * time.Millisecond):
			}
			active.Add(-1)
			result := sample(time.Now(), operational)
			result.StartedAt = now.UnixMilli()
			return result
		})
		close(done)
	}()
	deadline := time.After(time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("scheduler did not run")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if peak.Load() != 1 {
		t.Fatal("overlapping probes")
	}
	if store.latest.At == 0 {
		t.Fatal("scheduled result not saved")
	}
}
func TestHTTPReadsDoNotProbeOrWrite(t *testing.T) {
	store := testStore(t)
	handler := newHandler(store)
	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/status", nil))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	files, _ := os.ReadDir(store.dir)
	if len(files) != 0 || store.latest.At != 0 {
		t.Fatal("visitor triggered a probe")
	}
	for _, route := range []string{"/api/probe", "/image-samples.json", "/local-probe.user.js", "/main.go", "/.git/config"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", route, nil))
		if w.Code != 404 {
			t.Errorf("route %s exposed", route)
		}
	}
	for _, route := range []string{"/", "/api/status", "/status.css", "/status-page.js", "/assets/fonts/fonts.css", "/assets/fonts/nmb-gothic-ui-v1.woff2"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("HEAD", route, nil))
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatal("invalid HEAD")
		}
	}
	font := httptest.NewRecorder()
	handler.ServeHTTP(font, httptest.NewRequest("GET", "/assets/fonts/nmb-gothic-ui-v1.woff2", nil))
	if font.Code != 200 || font.Header().Get("Content-Type") != "font/woff2" || !strings.HasPrefix(font.Body.String(), "wOF2") {
		t.Fatal("web font was not served as WOFF2")
	}
	if font.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatal("versioned web font is not cached")
	}
	for _, route := range []string{"/assets/fonts/missing.woff2", "/assets/fonts/../../main.go", "/docs/index.html"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", route, nil))
		if w.Code != 404 {
			t.Errorf("unlisted asset %s exposed", route)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/status", nil))
	if w.Code != 405 {
		t.Fatal("mutation accepted")
	}
}
func TestJSONValidationAndBodyDeadline(t *testing.T) {
	for _, tc := range []struct {
		body, contentType string
		status            int
		valid             bool
	}{{`[]`, "application/json", 200, true}, {`[]`, "application/json", 503, false}, {`<html>challenge</html>`, "text/html", 200, false}, {`[`, "application/json", 200, false}, {`[] {}`, "application/json", 200, false}, {`{"error":"fail"}`, "application/json", 200, false}, {`[]` + strings.Repeat(" ", maxResponseBytes), "application/json", 200, false}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("id") != "4" || r.URL.Query().Get("_") == "" {
				t.Error("query changed")
			}
			w.Header().Set("Content-Type", tc.contentType)
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		var dest []cdnEntry
		err := fetchJSON(context.Background(), server.Client(), server.URL+"?id=4", &dest)
		server.Close()
		if (err == nil) != tc.valid {
			t.Fatalf("body %q: %v", tc.body[:min(30, len(tc.body))], err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "[")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var dest []cdnEntry
	if fetchJSON(ctx, server.Client(), server.URL, &dest) == nil {
		t.Fatal("body deadline missing")
	}
}
func TestPostsAndRecentSelection(t *testing.T) {
	raw := `[{"id":"1","content":"body","img":"2022-12-04/old","ext":".jpg","Replies":[{"id":2,"content":"reply","img":"2026-11-16/new","ext":".png"}]}]`
	var posts []imagePost
	json.Unmarshal([]byte(raw), &posts)
	if !validPosts(posts) {
		t.Fatal("valid numeric string IDs rejected")
	}
	if got := recentPaths(posts, testNow); !reflect.DeepEqual(got, []string{"2026-11-16/new.png"}) {
		t.Fatal(got)
	}
	posts[0].Content = nil
	if validPosts(posts) {
		t.Fatal("invalid schema accepted")
	}
	if _, err := normalizeCDN("https://user:pass@host/"); err == nil {
		t.Fatal("credential URL accepted")
	}
}
func TestAPIConfigSuccessCannotHideFailedPosts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "getCdnPath") {
			io.WriteString(w, `[{"url":"https://image.nmb.best/"}]`)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	result := checkAPI(context.Background(), server.Client(), server.URL, "main", testNow)
	if result.result.Status != degraded || !strings.Contains(result.result.Detail, "503") {
		t.Fatal(result)
	}
}
func TestImageChecksValidateBytesAndLimits(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.White)
	var pngData bytes.Buffer
	png.Encode(&pngData, img)
	for _, tc := range []struct {
		kind, body string
		code       int
		large      bool
		want       string
	}{{"image/png", pngData.String(), 200, false, operational}, {"image/png", "invalid", 200, false, outage}, {"text/html", "error", 200, false, outage}, {"image/png", pngData.String(), 503, false, outage}, {"image/png", "", 200, true, unknown}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", tc.kind)
			if tc.large {
				w.Header().Set("Content-Length", "20000000")
			}
			w.WriteHeader(tc.code)
			io.WriteString(w, tc.body)
		}))
		result := checkImage(context.Background(), server.Client(), server.URL, true)
		server.Close()
		if result.status != tc.want {
			t.Fatalf("%+v", result)
		}
	}
}
func TestStateAggregationNeverHidesMissingCoverage(t *testing.T) {
	if aggregate([]string{operational, unknown}) != unknown || aggregate([]string{operational, outage}) != degraded || aggregate([]string{outage, outage}) != outage {
		t.Fatal("invalid aggregation")
	}
}

func TestFailedChecksReduceAvailability(t *testing.T) {
	store := testStore(t)
	store.append(sample(testNow, operational))
	store.append(sample(testNow.Add(5*time.Minute), degraded))
	view := store.view(testNow.Add(6 * time.Minute))
	if *view.Components[0].Uptime != 50 {
		t.Fatal("degraded service displayed as 100% normal")
	}
}

func TestSlowSuccessIsAvailableWithoutFaultIncidents(t *testing.T) {
	store := testStore(t)
	store.append(sample(testNow, operational))
	store.append(sample(testNow.Add(5*time.Minute), slow))
	view := store.view(testNow.Add(6 * time.Minute))
	if view.Overall != slow || len(view.Incidents) != 0 || *view.Components[0].Uptime != 100 || view.Components[0].History[89].Slow != 1 {
		t.Fatalf("slow success treated as failure: %+v", view)
	}
	store.append(sample(testNow.Add(10*time.Minute), outage))
	store.append(sample(testNow.Add(15*time.Minute), slow))
	view = store.view(testNow.Add(16 * time.Minute))
	if *view.Components[0].Uptime != 75 || view.Incidents[0].Status != "resolved" {
		t.Fatal("slow success did not restore availability")
	}
	if aggregate([]string{operational, slow}) != slow || aggregate([]string{slow, unknown}) != unknown || aggregate([]string{slow, outage}) != degraded {
		t.Fatal("slow state hides failure or missing coverage")
	}
}

func TestOriginalDownloadTimingIsNotAnOutage(t *testing.T) {
	for _, tc := range []struct {
		duration time.Duration
		want     string
	}{{3 * time.Second, operational}, {6 * time.Second, operational}, {10 * time.Second, operational}, {11 * time.Second, slow}, {29 * time.Second, slow}} {
		if got := classifyProbe(nil, tc.duration, originalSlowThreshold); got.status != tc.want {
			t.Fatalf("duration=%s result=%+v", tc.duration, got)
		}
	}
	if got := classifyProbe(errors.New("HTTP 503"), time.Second, originalSlowThreshold); got.status != outage {
		t.Fatal("fast error must still fail")
	}
	if got := classifyProbe(nil, 4*time.Second, slowThreshold); got.status != slow {
		t.Fatal("API latency must not become a fault")
	}
}

func TestImageTimeoutIsUnknownButBrokenTransferIsFailure(t *testing.T) {
	var pngData bytes.Buffer
	png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "20000")
		w.Write(pngData.Bytes())
		w.(http.Flusher).Flush()
		if r.URL.Path == "/stall" {
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	got := checkImage(ctx, server.Client(), server.URL+"/stall", true)
	if got.status != unknown || !strings.Contains(got.detail, "超时") {
		t.Fatalf("timeout result=%+v", got)
	}
	got = checkImage(context.Background(), server.Client(), server.URL+"/broken", true)
	if got.status != outage || !strings.Contains(got.detail, "不完整") {
		t.Fatalf("broken transfer result=%+v", got)
	}
}

func TestLegacyPerformanceReloadPreservesRealFailuresAndRawLogs(t *testing.T) {
	for _, tc := range []struct {
		detail  string
		latency int64
		version int
		want    string
	}{
		{"5/5 张图片通过；3 张近期样本；3 张响应超过 3 秒", 6000, 0, operational},
		{"5/5 张图片通过；3 张近期样本", 6000, 0, operational},
		{"5/5 张图片通过；3 张近期样本；3 张响应超过 3 秒", 12000, 0, slow},
		{"4/5 张图片通过；3 张近期样本；HTTP 503", 6000, 0, degraded},
		{"5/5 张图片通过；3 张近期样本；CDN 地址超出本轮覆盖范围", 6000, 0, degraded},
		{"5/5 张图片通过；3 张近期样本", 6000, 2, degraded},
	} {
		run := sample(testNow, degraded)
		run.Version = tc.version
		for i := range run.Components {
			run.Components[i].Status = operational
		}
		run.Components[4] = observation{"originals", degraded, tc.latency, tc.detail}
		raw, _ := json.Marshal(run)
		dir := t.TempDir()
		path := filepath.Join(dir, "2026-11-16.jsonl")
		os.WriteFile(path, append(raw, '\n'), 0600)
		store, err := newMonitorStore(dir, 5*time.Minute, "test", testNow)
		if err != nil {
			t.Fatal(err)
		}
		view := store.view(testNow.Add(time.Minute))
		if view.Components[4].Status != tc.want {
			t.Fatalf("detail=%s got=%s", tc.detail, view.Components[4].Status)
		}
		if (len(view.Incidents) > 0) != (tc.want == degraded) {
			t.Fatal("history generated incorrect incident")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, append(raw, '\n')) {
			t.Fatal("original journal was rewritten")
		}
	}
}

type fixtureTransport struct {
	failedOriginal bool
	oldOnly        bool
	png            []byte
}

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, kind, code := "", "application/json", 200
	switch {
	case strings.Contains(r.URL.Path, "getCdnPath"):
		body = `[{"url":"https://image.nmb.best/"}]`
	case strings.Contains(r.URL.Path, "showf"):
		day := time.Now().UTC().Format("2006-01-02")
		if f.oldOnly {
			day = "2022-12-04"
		}
		posts := []map[string]any{}
		for _, name := range []string{"sample1", "sample2", "sample3"} {
			posts = append(posts, map[string]any{"id": "123", "content": "body", "img": day + "/" + name, "ext": ".png"})
		}
		encoded, _ := json.Marshal(posts)
		body = string(encoded)
	case strings.Contains(r.URL.Path, "notice"):
		body = `{"content":"notice","enable":true,"date":20261116}`
	default:
		kind = "image/png"
		body = string(f.png)
		if f.failedOriginal && strings.Contains(r.URL.Path, "/image/") && strings.Contains(r.URL.Path, "sample1") {
			code = 503
		}
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{kind}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
}
func TestFullProbeSuccessAndRecentOriginalFailure(t *testing.T) {
	var imageBody bytes.Buffer
	png.Encode(&imageBody, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	for _, tc := range []struct {
		fail, old       bool
		thumb, original string
	}{{false, false, operational, operational}, {true, false, operational, degraded}, {false, true, unknown, unknown}} {
		client := &http.Client{Transport: fixtureTransport{tc.fail, tc.old, imageBody.Bytes()}}
		run := probeOnce(context.Background(), client, time.Now())
		if !validRecord(run) || run.Components[3].Status != tc.thumb || run.Components[4].Status != tc.original {
			t.Fatalf("unexpected result: %+v", run)
		}
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("offline")
}
func TestFullProbeRecordsAllComponentsWhenNetworkFails(t *testing.T) {
	client := &http.Client{Transport: failingTransport{}}
	run := probeOnce(context.Background(), client, time.Now())
	if !validRecord(run) {
		t.Fatalf("invalid run: %+v", run)
	}
	for _, c := range run.Components {
		if c.Status == operational {
			t.Fatal("offline probe appears healthy")
		}
	}
}
