package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const retentionDays = 90

var displayZone = time.FixedZone("UTC+8", 8*3600)
var journalName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.jsonl$`)

type dayStats struct {
	Date     string `json:"date"`
	Status   string `json:"status"`
	Checks   int    `json:"checks"`
	OK       int    `json:"ok"`
	Slow     int    `json:"slow"`
	Degraded int    `json:"degraded"`
	Failed   int    `json:"failed"`
	Unknown  int    `json:"unknown"`
	Expected int    `json:"expected"`
}
type incident struct {
	ID            string `json:"id"`
	ComponentID   string `json:"componentId"`
	ComponentName string `json:"componentName"`
	Status        string `json:"status"`
	Severity      string `json:"severity"`
	StartedAt     int64  `json:"startedAt"`
	EndedAt       int64  `json:"endedAt,omitempty"`
	Detail        string `json:"detail"`
}
type componentView struct {
	componentDef
	Status    string     `json:"status"`
	LatencyMS int64      `json:"latencyMs"`
	Detail    string     `json:"detail"`
	Uptime    *float64   `json:"uptime"`
	Coverage  *float64   `json:"coverage"`
	History   []dayStats `json:"history"`
}
type statusView struct {
	GeneratedAt     int64           `json:"generatedAt"`
	CheckedAt       int64           `json:"checkedAt"`
	NextCheckAt     int64           `json:"nextCheckAt"`
	MonitoringSince int64           `json:"monitoringSince"`
	IntervalSeconds int             `json:"intervalSeconds"`
	RetentionDays   int             `json:"retentionDays"`
	Location        string          `json:"location"`
	Overall         string          `json:"overall"`
	Stale           bool            `json:"stale"`
	StorageError    bool            `json:"storageError"`
	HistoryWarning  bool            `json:"historyWarning"`
	Components      []componentView `json:"components"`
	Incidents       []incident      `json:"incidents"`
}
type monitorStore struct {
	mu             sync.RWMutex
	dir            string
	interval       time.Duration
	location       string
	latest         record
	first          int64
	days           map[string]map[string]*dayStats
	incidents      []incident
	storageError   bool
	historyWarning bool
	lastPrune      string
}

func newMonitorStore(dir string, interval time.Duration, location string, now time.Time) (*monitorStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	store := &monitorStore{dir: dir, interval: interval, location: location, days: map[string]map[string]*dayStats{}, incidents: []incident{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	cutoff := now.In(displayZone).AddDate(0, 0, -(retentionDays - 1)).Format("2006-01-02")
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !journalName.MatchString(entry.Name()) || entry.Name()[:10] < cutoff {
			continue
		}
		file, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), 65536)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			var run record
			if json.Unmarshal(line, &run) != nil || !validRecord(run) || run.At > now.Add(time.Minute).UnixMilli() {
				store.historyWarning = true
				continue
			}
			run = normalizeLegacyPerformance(run)
			if run.At > store.latest.At {
				store.apply(run)
			}
		}
		scanErr := scanner.Err()
		file.Close()
		if scanErr != nil {
			return nil, scanErr
		}
	}
	store.prune(now)
	return store, nil
}
func validRecord(run record) bool {
	if run.At <= 0 || run.StartedAt <= 0 || run.StartedAt > run.At || len(run.Components) != len(componentDefs) {
		return false
	}
	for i, result := range run.Components {
		if result.ID != componentDefs[i].ID || result.LatencyMS < 0 || len(result.Detail) > 2000 {
			return false
		}
		switch result.Status {
		case operational, slow, degraded, outage, unknown:
		default:
			return false
		}
	}
	return true
}

var legacyImageSuccess = regexp.MustCompile(`^(\d+)/(\d+) 张图片通过；3 张近期样本(?:；\d+ 张响应超过 3 秒)?$`)
var legacyAPISuccess = regexp.MustCompile(`^CDN 配置：(验证通过|响应缓慢（超过 3 秒）)；帖子列表：(验证通过|响应缓慢（超过 3 秒）)$`)

// Reclassify only legacy records that explicitly prove successful validation.
// Raw journals remain unchanged; failures and incomplete coverage stay untouched.
func normalizeLegacyPerformance(run record) record {
	if run.Version >= 2 {
		return run
	}
	for i := range run.Components {
		result := &run.Components[i]
		if result.Status != degraded {
			continue
		}
		if result.ID == "originals" || result.ID == "thumbnails" {
			match := legacyImageSuccess.FindStringSubmatch(result.Detail)
			if len(match) != 3 || match[1] != match[2] || match[1] == "0" {
				continue
			}
			threshold := slowThreshold
			if result.ID == "originals" {
				threshold = originalSlowThreshold
			}
			result.Status = classifyProbe(nil, time.Duration(result.LatencyMS)*time.Millisecond, threshold).status
			result.Detail = fmt.Sprintf("%s/%s 张图片通过；3 张近期样本；最长下载 %.2f 秒（历史性能分类已修正）", match[1], match[2], float64(result.LatencyMS)/1000)
		} else if legacyAPISuccess.MatchString(result.Detail) || result.ID == "notice" && result.Detail == "响应缓慢（超过 3 秒）" {
			result.Status = slow
			result.Detail = strings.ReplaceAll(result.Detail, "响应缓慢（超过 3 秒）", "验证通过，响应较慢（超过 3 秒）")
		}
	}
	return run
}

// Only the scheduler calls append. HTTP readers never initiate probes or writes.
func (s *monitorStore) append(run record) error {
	if !validRecord(run) {
		return errors.New("invalid probe record")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.At <= s.latest.At {
		return errors.New("probe timestamp did not advance")
	}
	encoded, err := json.Marshal(run)
	if err == nil {
		day := time.UnixMilli(run.At).In(displayZone).Format("2006-01-02")
		var file *os.File
		file, err = os.OpenFile(filepath.Join(s.dir, day+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err == nil {
			// A leading newline isolates a possible incomplete final record after a crash.
			_, err = file.Write(append(append([]byte{'\n'}, encoded...), '\n'))
			if err == nil {
				err = file.Sync()
			}
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		s.storageError = true
		return err
	}
	s.storageError = false
	s.apply(run)
	s.prune(time.UnixMilli(run.At))
	return nil
}
func (s *monitorStore) apply(run record) {
	if s.first == 0 {
		s.first = run.StartedAt
	}
	// A gap is not a recovery. Preserve the prior incident as interrupted evidence.
	if s.latest.At != 0 && run.StartedAt-s.latest.At > int64(2*s.interval/time.Millisecond) {
		for i := range s.incidents {
			if s.incidents[i].Status == "active" {
				s.incidents[i].Status = "interrupted"
				s.incidents[i].EndedAt = s.latest.At
			}
		}
	}
	date := time.UnixMilli(run.At).In(displayZone).Format("2006-01-02")
	if s.days[date] == nil {
		s.days[date] = map[string]*dayStats{}
	}
	for i, result := range run.Components {
		day := s.days[date][result.ID]
		if day == nil {
			day = &dayStats{Date: date}
			s.days[date][result.ID] = day
		}
		day.Checks++
		switch result.Status {
		case operational:
			day.OK++
		case slow:
			day.Slow++
		case degraded:
			day.Degraded++
		case outage:
			day.Failed++
		default:
			day.Unknown++
		}
		active := -1
		for index := len(s.incidents) - 1; index >= 0; index-- {
			if s.incidents[index].ComponentID == result.ID && s.incidents[index].Status == "active" {
				active = index
				break
			}
		}
		if result.Status == outage || result.Status == degraded {
			if active < 0 {
				s.incidents = append(s.incidents, incident{ID: fmt.Sprintf("%d-%s", run.At, result.ID), ComponentID: result.ID, ComponentName: componentDefs[i].Name, Status: "active", Severity: result.Status, StartedAt: run.At, Detail: result.Detail})
			} else {
				s.incidents[active].Detail = result.Detail
				if result.Status == outage {
					s.incidents[active].Severity = outage
				}
			}
		} else if (result.Status == operational || result.Status == slow) && active >= 0 {
			s.incidents[active].Status = "resolved"
			s.incidents[active].EndedAt = run.At
		}
	}
	s.latest = run
	// Detailed raw records remain on disk; keep the latest 500 incidents in memory.
	if len(s.incidents) > 500 {
		keep := []incident{}
		for i, event := range s.incidents {
			if i >= len(s.incidents)-500 || event.Status == "active" {
				keep = append(keep, event)
			}
		}
		s.incidents = keep
	}
}
func (s *monitorStore) prune(now time.Time) {
	today := now.In(displayZone).Format("2006-01-02")
	if s.lastPrune == today {
		return
	}
	s.lastPrune = today
	cutoff := now.In(displayZone).AddDate(0, 0, -(retentionDays - 1)).Format("2006-01-02")
	for day := range s.days {
		if day < cutoff {
			delete(s.days, day)
		}
	}
	keep := s.incidents[:0]
	for _, event := range s.incidents {
		if event.Status == "active" || time.UnixMilli(event.EndedAt).In(displayZone).Format("2006-01-02") >= cutoff {
			keep = append(keep, event)
		}
	}
	s.incidents = keep
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		log.Printf("history retention: %v", err)
		return
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && journalName.MatchString(entry.Name()) && entry.Name()[:10] < cutoff {
			if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil {
				log.Printf("history retention: %v", err)
			}
		}
	}
}
func (s *monitorStore) view(now time.Time) statusView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stale := s.latest.At == 0 || now.Sub(time.UnixMilli(s.latest.At)) > 2*s.interval
	view := statusView{GeneratedAt: now.UnixMilli(), CheckedAt: s.latest.At, MonitoringSince: s.first, IntervalSeconds: int(s.interval / time.Second), RetentionDays: retentionDays, Location: s.location, Stale: stale, StorageError: s.storageError, HistoryWarning: s.historyWarning, Components: []componentView{}, Incidents: append([]incident{}, s.incidents...)}
	if s.latest.At > 0 {
		view.NextCheckAt = s.latest.StartedAt + s.interval.Milliseconds()
	}
	states := []string{}
	for index, def := range componentDefs {
		row := componentView{componentDef: def, Status: unknown, Detail: "等待首次定期检测", History: []dayStats{}}
		if len(s.latest.Components) > index {
			result := s.latest.Components[index]
			row.Status, row.LatencyMS, row.Detail = result.Status, result.LatencyMS, result.Detail
		}
		if stale || s.storageError {
			row.Status = unknown
			if s.latest.At > 0 {
				row.Detail = "最新记录已过期，等待探针更新"
			}
			if s.storageError {
				row.Detail = "记录保存失败，最新状态暂不可确认"
			}
		}
		known, available, expected, observed := 0, 0, 0, 0
		for offset := retentionDays - 1; offset >= 0; offset-- {
			date := now.In(displayZone).AddDate(0, 0, -offset).Format("2006-01-02")
			day := dayStats{Date: date, Status: unknown}
			if saved := s.days[date][def.ID]; saved != nil {
				day = *saved
			}
			start, _ := time.ParseInLocation("2006-01-02", date, displayZone)
			end := start.AddDate(0, 0, 1)
			if end.After(now) {
				end = now
			}
			if s.first > 0 && time.UnixMilli(s.first).After(start) {
				start = time.UnixMilli(s.first)
			}
			if end.After(start) {
				day.Expected = max(day.Checks, int((end.Sub(start)+s.interval-1)/s.interval))
			}
			switch {
			case day.Checks == 0:
				day.Status = unknown
			case day.Failed == day.Checks:
				day.Status = outage
			case day.Failed > 0 || day.Degraded > 0:
				day.Status = degraded
			case day.Unknown > 0:
				day.Status = unknown
			case day.Slow > 0:
				day.Status = slow
			default:
				day.Status = operational
			}
			known += day.OK + day.Slow + day.Degraded + day.Failed
			available += day.OK + day.Slow
			expected += day.Expected
			observed += day.Checks
			row.History = append(row.History, day)
		}
		if known > 0 {
			value := float64(available) * 100 / float64(known)
			row.Uptime = &value
		}
		if expected > 0 {
			value := min(100, float64(observed)*100/float64(expected))
			row.Coverage = &value
		}
		states = append(states, row.Status)
		view.Components = append(view.Components, row)
	}
	view.Overall = aggregate(states)
	sort.SliceStable(view.Incidents, func(i, j int) bool { return view.Incidents[i].StartedAt > view.Incidents[j].StartedAt })
	return view
}
func (s *monitorStore) run(ctx context.Context, probe func(context.Context, time.Time) record) {
	for {
		s.mu.RLock()
		last := s.latest.StartedAt
		s.mu.RUnlock()
		delay := time.Duration(0)
		if last > 0 {
			delay = time.Until(time.UnixMilli(last).Add(s.interval))
			if delay < 0 {
				delay = 0
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		start := time.Now()
		cycle, cancel := context.WithTimeout(ctx, 90*time.Second)
		result := probe(cycle, start)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err := s.append(result); err != nil {
			log.Printf("save probe: %v", err)
			// Avoid a tight retry loop if the disk is unavailable.
			timer := time.NewTimer(s.interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}
