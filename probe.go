package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	operational      = "operational"
	degraded         = "degraded"
	outage           = "outage"
	unknown          = "unknown"
	recentCount      = 3
	maxCDNs          = 3
	maxResponseBytes = 2 << 20
	maxImageBytes    = 10 << 20
	slowThreshold    = 3 * time.Second
)

type componentDef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Group       string `json:"group"`
	Host        string `json:"host"`
	Description string `json:"description"`
}

var componentDefs = []componentDef{
	{"main", "主站 API", "API 服务", "www.nmbxd.com", "CDN 配置与帖子列表"},
	{"backup", "备用域名 API", "API 服务", "www.nmbxd1.com", "CDN 配置与帖子列表"},
	{"api", "备用 API", "API 服务", "api.nmb.best", "CDN 配置与帖子列表"},
	{"thumbnails", "图片缩略图", "图片服务", "动态 CDN 地址", "2 张固定图片 + 3 张近期图片"},
	{"originals", "图片原图", "图片服务", "动态 CDN 地址", "2 张固定图片 + 3 张近期图片"},
	{"notice", "公告接口", "其他服务", "nmb.ovear.info", "HTTP 状态与公告内容格式"},
}
var apiOrigins = []string{"https://www.nmbxd.com", "https://www.nmbxd1.com", "https://api.nmb.best"}
var baselines = []string{"2022-12-04/638c87770b738.jpg", "2026-02-12/698deed346c6c.png"}
var imagePath = regexp.MustCompile(`(?i)^\d{4}-\d{2}-\d{2}/[a-zA-Z0-9_-]+\.(jpe?g|png|gif)$`)

type observation struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	LatencyMS int64  `json:"latencyMs"`
	Detail    string `json:"detail"`
}
type record struct {
	At         int64         `json:"at"`
	StartedAt  int64         `json:"startedAt"`
	Components []observation `json:"components"`
}
type probeCheck struct {
	status, detail string
	latency        int64
}
type cdnEntry struct {
	URL string `json:"url"`
}
type imagePost struct {
	ID      json.RawMessage `json:"id"`
	Content *string         `json:"content"`
	Img     *string         `json:"img"`
	Ext     *string         `json:"ext"`
	Replies []imagePost     `json:"Replies"`
}
type apiProbe struct {
	result observation
	cdns   []string
	paths  []string
}

func aggregate(states []string) string {
	if len(states) == 0 {
		return unknown
	}
	bad, pending, slow := 0, false, false
	for _, state := range states {
		switch state {
		case outage:
			bad++
		case unknown:
			pending = true
		case degraded:
			slow = true
		}
	}
	if bad == len(states) {
		return outage
	}
	if bad > 0 || slow {
		return degraded
	}
	if pending {
		return unknown
	}
	return operational
}
func normalizeCDN(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("CDN 地址格式无效")
	}
	return strings.TrimRight(u.String(), "/") + "/", nil
}
func recentPaths(posts []imagePost, now time.Time) []string {
	seen := map[string]bool{}
	add := func(p imagePost) {
		if p.Img == nil || p.Ext == nil {
			return
		}
		name := *p.Img + *p.Ext
		if !imagePath.MatchString(name) {
			return
		}
		day, err := time.Parse("2006-01-02", name[:10])
		if err != nil || day.After(now.Add(24*time.Hour)) || now.Sub(day) > 7*24*time.Hour {
			return
		}
		for _, baseline := range baselines {
			if name == baseline {
				return
			}
		}
		seen[name] = true
	}
	for _, p := range posts {
		add(p)
		for _, reply := range p.Replies {
			add(reply)
		}
	}
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(result)))
	if len(result) > recentCount {
		result = result[:recentCount]
	}
	return result
}
func validPosts(posts []imagePost) bool {
	if len(posts) == 0 {
		return false
	}
	valid := func(p imagePost) bool {
		var id json.Number
		if json.Unmarshal(p.ID, &id) != nil {
			return false
		}
		n, err := strconv.ParseInt(string(id), 10, 64)
		return err == nil && n > 0 && p.Content != nil && p.Img != nil && p.Ext != nil
	}
	for _, p := range posts {
		if !valid(p) {
			return false
		}
		for _, r := range p.Replies {
			if !valid(r) {
				return false
			}
		}
	}
	return true
}
func request(ctx context.Context, client *http.Client, endpoint string) (*http.Response, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	query := u.Query()
	query.Set("_", strconv.FormatInt(time.Now().UnixMilli(), 10))
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "xdnmb-stat-monitor/2.0")
	req.Header.Set("Cache-Control", "no-cache")
	return client.Do(req)
}
func fetchJSON(ctx context.Context, client *http.Client, endpoint string, dest any) error {
	resp, err := request(ctx, client, endpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	kind, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !(kind == "application/json" || strings.HasPrefix(kind, "application/") && strings.HasSuffix(kind, "+json")) {
		return errors.New("返回内容不是 JSON")
	}
	reader := &io.LimitedReader{R: resp.Body, N: maxResponseBytes + 1}
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(dest); err != nil {
		return errors.New("JSON 无法解析或结构不符")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("JSON 后存在多余内容")
	}
	if reader.N == 0 {
		return errors.New("响应超过 2 MiB 检测上限")
	}
	return nil
}
func measure(fn func() error) probeCheck {
	start := time.Now()
	err := fn()
	elapsed := time.Since(start)
	if err != nil {
		detail := err.Error()
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			detail = "连接失败或请求超时"
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			detail = "探针超时"
		}
		return probeCheck{outage, detail, elapsed.Milliseconds()}
	}
	status := operational
	detail := "验证通过"
	if elapsed > slowThreshold {
		status = degraded
		detail = "响应缓慢（超过 3 秒）"
	}
	return probeCheck{status, detail, elapsed.Milliseconds()}
}
func checkAPI(ctx context.Context, client *http.Client, origin, id string, now time.Time) apiProbe {
	result := apiProbe{}
	var configs []cdnEntry
	config := measure(func() error {
		if err := fetchJSON(ctx, client, origin+"/Api/getCdnPath", &configs); err != nil {
			return err
		}
		if len(configs) == 0 {
			return errors.New("CDN 配置为空")
		}
		bases := []string{}
		for _, entry := range configs {
			base, err := normalizeCDN(entry.URL)
			if err != nil {
				return err
			}
			bases = append(bases, base)
		}
		result.cdns = bases
		return nil
	})
	var posts []imagePost
	content := measure(func() error {
		if err := fetchJSON(ctx, client, origin+"/Api/showf?id=4&page=1", &posts); err != nil {
			return err
		}
		if !validPosts(posts) {
			return errors.New("帖子列表内容格式不符")
		}
		result.paths = recentPaths(posts, now)
		return nil
	})
	result.result = observation{ID: id, Status: aggregate([]string{config.status, content.status}), LatencyMS: max(config.latency, content.latency), Detail: "CDN 配置：" + config.detail + "；帖子列表：" + content.detail}
	return result
}
func checkImage(ctx context.Context, client *http.Client, endpoint string) probeCheck {
	limited := false
	result := measure(func() error {
		resp, err := request(ctx, client, endpoint)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if resp.ContentLength > maxImageBytes {
			limited = true
			return errors.New("图片超过 10 MiB 检测上限")
		}
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "image/") {
			return errors.New("响应不是图片")
		}
		limiter := &io.LimitedReader{R: resp.Body, N: maxImageBytes + 1}
		reader := bufio.NewReader(limiter)
		config, _, err := image.DecodeConfig(reader)
		if err != nil {
			return errors.New("图片格式或头部无效")
		}
		if config.Width <= 0 || config.Height <= 0 {
			return errors.New("图片尺寸无效")
		}
		if _, err = io.Copy(io.Discard, reader); err != nil {
			return errors.New("图片下载不完整")
		}
		if limiter.N == 0 {
			limited = true
			return errors.New("图片超过 10 MiB 检测上限")
		}
		return nil
	})
	if limited {
		result.status = unknown
	}
	return result
}
func probeOnce(ctx context.Context, client *http.Client, now time.Time) record {
	results := make([]apiProbe, len(apiOrigins))
	var wg sync.WaitGroup
	notice := observation{ID: "notice"}
	for i, origin := range apiOrigins {
		wg.Add(1)
		go func(i int, origin string) {
			defer wg.Done()
			results[i] = checkAPI(ctx, client, origin, componentDefs[i].ID, now)
		}(i, origin)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		var body struct {
			Content *string     `json:"content"`
			Enable  *bool       `json:"enable"`
			Date    json.Number `json:"date"`
		}
		check := measure(func() error {
			if err := fetchJSON(ctx, client, "https://nmb.ovear.info/nmb-notice.json", &body); err != nil {
				return err
			}
			if body.Content == nil || body.Enable == nil || body.Date == "" {
				return errors.New("公告内容格式不符")
			}
			return nil
		})
		notice = observation{"notice", check.status, check.latency, check.detail}
	}()
	wg.Wait()
	bases, paths := map[string]bool{}, map[string]bool{}
	run := record{StartedAt: now.UnixMilli(), Components: []observation{}}
	for _, api := range results {
		run.Components = append(run.Components, api.result)
		for _, base := range api.cdns {
			bases[base] = true
		}
		for _, path := range api.paths {
			paths[path] = true
		}
	}
	coverage := ""
	if len(bases) == 0 {
		coverage = "未取得当前 CDN 配置"
		bases["https://image.nmb.best/"] = true
	}
	if len(bases) > maxCDNs {
		coverage = "CDN 地址超出本轮覆盖范围"
	}
	selectedBases := []string{}
	for base := range bases {
		selectedBases = append(selectedBases, base)
	}
	sort.Strings(selectedBases)
	if len(selectedBases) > maxCDNs {
		selectedBases = selectedBases[:maxCDNs]
	}
	recent := []string{}
	for path := range paths {
		recent = append(recent, path)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(recent)))
	if len(recent) > recentCount {
		recent = recent[:recentCount]
	}
	if len(recent) < recentCount {
		coverage = fmt.Sprintf("近期样本不足（%d/%d）", len(recent), recentCount)
	}
	type job struct {
		kind int
		url  string
	}
	jobs := []job{}
	for _, base := range selectedBases {
		for _, path := range append(append([]string{}, baselines...), recent...) {
			for k, folder := range []string{"thumb/", "image/"} {
				jobs = append(jobs, job{k, base + folder + path})
			}
		}
	}
	checks := make([]probeCheck, len(jobs))
	queue := make(chan int)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range queue {
				if ctx.Err() != nil {
					checks[index] = probeCheck{unknown, "本轮探针超时", 0}
				} else {
					checks[index] = checkImage(ctx, client, jobs[index].url)
				}
			}
		}()
	}
	for i := range jobs {
		queue <- i
	}
	close(queue)
	wg.Wait()
	for k, id := range []string{"thumbnails", "originals"} {
		states := []string{}
		passed, slow, total := 0, 0, 0
		latency := int64(0)
		failure := ""
		for i, job := range jobs {
			if job.kind != k {
				continue
			}
			check := checks[i]
			states = append(states, check.status)
			total++
			latency = max(latency, check.latency)
			if check.status == operational || check.status == degraded {
				passed++
				if check.status == degraded {
					slow++
				}
			} else if failure == "" {
				failure = check.detail
			}
		}
		detail := fmt.Sprintf("%d/%d 张图片通过；%d 张近期样本", passed, total, len(recent))
		if slow > 0 {
			detail += fmt.Sprintf("；%d 张响应超过 3 秒", slow)
		}
		if failure != "" {
			detail += "；" + failure
		}
		if coverage != "" {
			states = append(states, unknown)
			detail += "；" + coverage
		}
		run.Components = append(run.Components, observation{id, aggregate(states), latency, detail})
	}
	run.Components = append(run.Components, notice)
	run.At = time.Now().UnixMilli()
	return run
}
