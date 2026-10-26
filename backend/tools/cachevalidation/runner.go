package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	feed "github.com/ZoeySigel/owlet-video/backend/internal/application/feed"
	feedrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/feed"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type scheduledOp struct {
	Sequence int    `json:"sequence"`
	OffsetNS int64  `json:"offset_ns"`
	Kind     string `json:"kind"`
	Page     int    `json:"page"`
	Viewer   int64  `json:"viewer"`
}
type operationResponse struct {
	Result             *feed.FeedResult `json:"result,omitempty"`
	Counters           Counters         `json:"counters"`
	Error              string           `json:"error,omitempty"`
	Stale              bool             `json:"stale"`
	StaleAgeNS         int64            `json:"stale_age_ns"`
	FreshnessViolation bool             `json:"freshness_violation"`
}
type operationRecord struct {
	Operation          scheduledOp `json:"operation"`
	Dropped            bool        `json:"dropped"`
	LatencyNS          int64       `json:"latency_ns"`
	ScheduleDelayNS    int64       `json:"schedule_delay_ns"`
	CompletedInWindow  bool        `json:"completed_in_window"`
	Counters           Counters    `json:"counters"`
	Error              string      `json:"error,omitempty"`
	Stale              bool        `json:"stale"`
	StaleAgeNS         int64       `json:"stale_age_ns"`
	FreshnessViolation bool        `json:"freshness_violation"`
}
type latencyStats struct {
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
}
type runResult struct {
	ID                  string           `json:"id"`
	Mode                string           `json:"mode"`
	Scenario            string           `json:"scenario"`
	Rate                int              `json:"rate"`
	Round               int              `json:"round"`
	Overlap             int              `json:"overlap"`
	TraceFile           string           `json:"trace_file"`
	DurationSeconds     float64          `json:"duration_seconds"`
	Planned             int              `json:"planned"`
	Sent                int              `json:"sent"`
	Dropped             int              `json:"dropped"`
	Errors              int              `json:"errors"`
	SuccessfulInWindow  int              `json:"successful_in_window"`
	ReadCount           int              `json:"read_count"`
	WriteCount          int              `json:"write_count"`
	ReadSuccess         int              `json:"read_success"`
	CompleteCacheHits   int              `json:"public_complete_cache_hits"`
	NoPublicSQLReads    int              `json:"no_public_sql_reads"`
	ReadLatency         latencyStats     `json:"read_latency"`
	ReadArrivalLatency  latencyStats     `json:"read_arrival_latency"`
	ScheduleDelay       latencyStats     `json:"schedule_delay"`
	WriteLatency        latencyStats     `json:"write_latency"`
	ReadCounters        Counters         `json:"read_counters"`
	WriteCounters       Counters         `json:"write_counters"`
	ColdCounters        Counters         `json:"cold_counters"`
	ColdLatencyNS       int64            `json:"cold_latency_ns"`
	StaleReads          int              `json:"stale_reads"`
	MaxStaleAgeNS       int64            `json:"max_stale_age_ns"`
	FreshnessViolations int              `json:"freshness_violations"`
	Valid               bool             `json:"valid"`
	MemoryBefore        memorySnapshot   `json:"memory_before"`
	MemoryAfter         memorySnapshot   `json:"memory_after"`
	Resources           []resourceSample `json:"resource_samples"`
}

func (r runResult) SQLPerRead() float64 {
	if r.ReadCount == 0 {
		return 0
	}
	return float64(r.ReadCounters.SQLQueries) / float64(r.ReadCount)
}

type resourceSample struct {
	OffsetSeconds float64        `json:"offset_seconds"`
	Memory        memorySnapshot `json:"memory"`
}

func makeTrace(seed int64, scenario string, rate int, duration time.Duration, pages, users int) []scheduledOp {
	n := int(int64(rate) * int64(duration) / int64(time.Second))
	rng := rand.New(rand.NewSource(seed))
	out := make([]scheduledOp, n)
	mutations := 0
	if scenario == "count" {
		mutations = n / 20
	}
	if scenario == "card" {
		mutations = n / 100
	}
	mutationSlots := map[int]bool{}
	for _, i := range rng.Perm(n)[:mutations] {
		mutationSlots[i] = true
	}
	hotPages := max(1, pages/5)
	for i := range out {
		kind := "read"
		if mutationSlots[i] {
			kind = scenario
		}
		page := rng.Intn(pages)
		if rng.Intn(100) < 80 {
			page = rng.Intn(hotPages)
		}
		out[i] = scheduledOp{Sequence: i, OffsetNS: int64(i) * int64(time.Second) / int64(rate), Kind: kind, Page: page, Viewer: 1 + int64(rng.Intn(users))}
	}
	return out
}
func writeTrace(path string, trace []scheduledOp) error {
	f, e := os.Create(path)
	if e != nil {
		return e
	}
	defer f.Close()
	b := bufio.NewWriter(f)
	enc := json.NewEncoder(b)
	for _, op := range trace {
		if e = enc.Encode(op); e != nil {
			return e
		}
	}
	return b.Flush()
}

type writeHandler struct {
	mu                        sync.Mutex
	f                         *fixture
	r                         Reader
	truth                     *oracle
	countVersion, cardVersion int64
}

func (w *writeHandler) apply(ctx context.Context, kind string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	version := w.countVersion + 1
	if kind == "card" {
		version = w.cardVersion + 1
	}
	// Hold the oracle lock across commit so a reader cannot observe a committed
	// version before its in-memory truth record is published.
	w.truth.mu.Lock()
	err := w.f.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var result *gorm.DB
		if kind == "count" {
			result = tx.Exec("UPDATE video_stat SET like_count=? WHERE video_id=?", 3+version, w.truth.target)
		} else {
			result = tx.Exec("UPDATE video SET title=? WHERE id=?", videoTitle(w.truth.target, version), w.truth.target)
		}
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("mutation affected %d rows", result.RowsAffected)
		}
		return nil
	})
	if err != nil {
		w.truth.mu.Unlock()
		return err
	}
	if kind == "count" {
		w.countVersion = version
	} else {
		w.cardVersion = version
	}
	change := changeRecord{Version: version, At: time.Now()}
	if kind == "count" {
		w.truth.count = append(w.truth.count, change)
	} else {
		w.truth.card = append(w.truth.card, change)
	}
	w.truth.mu.Unlock()
	_, err = w.r.Invalidate(ctx, w.truth.target, kind)
	return err
}

func runLoad(ctx context.Context, f *fixture, c catalog, trace []scheduledOp, mode, scenario string, rate, round int) (runResult, error) {
	r := runResult{ID: fmt.Sprintf("%s-%s-%d-r%d", mode, scenario, rate, round), Mode: mode, Scenario: scenario, Rate: rate, Round: round, Overlap: c.Overlap, DurationSeconds: f.cfg.Duration.Seconds(), Planned: len(trace)}
	if err := f.reset(ctx); err != nil {
		return r, err
	}
	reader, err := newReader(mode, feedrepo.New(f.db), f.rdb)
	if err != nil {
		return r, err
	}
	truth := &oracle{target: c.Target}
	writer := &writeHandler{f: f, r: reader, truth: truth}
	start := time.Now()
	_, r.ColdCounters, err = readMeasured(ctx, reader, requestFor(c.Pages[0], 20, 1))
	r.ColdLatencyNS = time.Since(start).Nanoseconds()
	if err != nil {
		return r, err
	}
	for _, p := range c.Pages {
		if _, err = reader.Read(ctx, requestFor(p, 20, 1)); err != nil {
			return r, err
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var task scheduledOp
		if req.Method != "POST" || json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&task) != nil || task.Page < 0 || task.Page >= len(c.Pages) || (task.Kind != "read" && task.Kind != "count" && task.Kind != "card") {
			http.Error(w, "invalid validation task", 400)
			return
		}
		oc, op := withOperation(req.Context())
		response := operationResponse{}
		began := time.Now()
		var callErr error
		if task.Kind == "read" {
			response.Result, callErr = reader.Read(oc, requestFor(c.Pages[task.Page], 20, task.Viewer))
			if callErr == nil {
				callErr = verifyResult(response.Result, c.Pages[task.Page], task.Viewer, f.cfg.Users, c.Target, nil, nil)
			}
			if callErr == nil {
				response.Stale, response.StaleAgeNS, response.FreshnessViolation = inspectNS(truth, response.Result, began, time.Now())
			}
		} else {
			callErr = writer.apply(oc, task.Kind)
		}
		response.Counters = op.Snapshot()
		if callErr != nil {
			response.Error = callErr.Error()
			w.WriteHeader(500)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	transport := &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 256}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: f.cfg.RequestTimeout}
	// Prewarm executes only reads: mutation versions and the measured trace start at zero.
	until := time.Now().Add(f.cfg.Warmup)
	i := 0
	for time.Now().Before(until) {
		if _, err = reader.Read(ctx, requestFor(c.Pages[i%len(c.Pages)], 20, 1)); err != nil {
			return r, err
		}
		i++
		remaining := time.Until(until)
		if remaining > 0 {
			time.Sleep(min(time.Second/time.Duration(rate), remaining))
		}
	}
	r.MemoryBefore = readMemory(ctx, f.rdb)
	raw, err := os.Create(filepath.Join(f.cfg.Output, "runs", r.ID+".jsonl"))
	if err != nil {
		return r, err
	}
	defer raw.Close()
	buffer := bufio.NewWriter(raw)
	encoder := json.NewEncoder(buffer)
	records := make(chan operationRecord, f.cfg.MaxInflight)
	slots := make(chan struct{}, f.cfg.MaxInflight)
	var requests sync.WaitGroup
	var collection sync.WaitGroup
	var reads, writes, arrivalReads, scheduleDelays []int64
	var encodeErr error
	lateDropped := 0
	collection.Add(1)
	go func() {
		defer collection.Done()
		for record := range records {
			if e := encoder.Encode(record); e != nil && encodeErr == nil {
				encodeErr = e
			}
			if record.Dropped {
				lateDropped++
				continue
			}
			if record.Operation.Kind == "read" {
				r.ReadCount++
				r.ReadCounters = r.ReadCounters.Add(record.Counters)
				reads = append(reads, record.LatencyNS)
				arrivalReads = append(arrivalReads, record.LatencyNS+record.ScheduleDelayNS)
				if record.Error == "" {
					r.ReadSuccess++
					if record.Counters.SQLQueries == 2 {
						r.NoPublicSQLReads++
						misses := int64(0)
						for _, n := range record.Counters.Misses {
							misses += n
						}
						if misses == 0 {
							r.CompleteCacheHits++
						}
					}
				}
			} else {
				r.WriteCount++
				r.WriteCounters = r.WriteCounters.Add(record.Counters)
				writes = append(writes, record.LatencyNS)
			}
			scheduleDelays = append(scheduleDelays, record.ScheduleDelayNS)
			if record.Error != "" {
				r.Errors++
			} else if record.CompletedInWindow {
				r.SuccessfulInWindow++
			}
			if record.Stale {
				r.StaleReads++
			}
			if record.StaleAgeNS > r.MaxStaleAgeNS {
				r.MaxStaleAgeNS = record.StaleAgeNS
			}
			if record.FreshnessViolation {
				r.FreshnessViolations++
			}
		}
	}()
	begin := time.Now()
	deadline := begin.Add(f.cfg.Duration)
	resourceCtx, cancelResources := context.WithCancel(ctx)
	resourceDone := make(chan []resourceSample, 1)
	go func() {
		var samples []resourceSample
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-resourceCtx.Done():
				resourceDone <- samples
				return
			case <-ticker.C:
				samples = append(samples, resourceSample{OffsetSeconds: time.Since(begin).Seconds(), Memory: readMemory(ctx, f.rdb)})
			}
		}
	}()
	sent, dropped := 0, 0
	for _, task := range trace {
		scheduled := begin.Add(time.Duration(task.OffsetNS))
		if wait := time.Until(scheduled); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				dropped++
				continue
			case <-timer.C:
			}
		}
		if time.Since(scheduled) > 100*time.Millisecond {
			dropped++
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			dropped++
			continue
		}
		sent++
		requests.Add(1)
		go func(task scheduledOp, scheduled time.Time) {
			defer requests.Done()
			defer func() { <-slots }()
			actual := time.Now()
			rec := operationRecord{Operation: task, ScheduleDelayNS: actual.Sub(scheduled).Nanoseconds()}
			// A goroutine may itself start late after the dispatch check. Do not
			// issue that request as a hidden catch-up burst.
			if actual.Sub(scheduled) > 100*time.Millisecond {
				rec.Dropped, rec.Error = true, "actual dispatch missed arrival tolerance"
				records <- rec
				return
			}
			payload, _ := json.Marshal(task)
			request, e := http.NewRequestWithContext(ctx, "POST", server.URL, bytes.NewReader(payload))
			if e != nil {
				rec.Error = e.Error()
			} else {
				response, e := client.Do(request)
				if e != nil {
					rec.Error = e.Error()
				} else {
					var body operationResponse
					e = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&body)
					response.Body.Close()
					rec.Counters = body.Counters
					rec.Stale = body.Stale
					rec.StaleAgeNS = body.StaleAgeNS
					rec.FreshnessViolation = body.FreshnessViolation
					if e != nil {
						rec.Error = e.Error()
					} else if response.StatusCode != 200 || body.Error != "" {
						rec.Error = fmt.Sprintf("HTTP %d: %s", response.StatusCode, body.Error)
					}
				}
			}
			ended := time.Now()
			rec.LatencyNS = ended.Sub(actual).Nanoseconds()
			rec.CompletedInWindow = !ended.After(deadline)
			records <- rec
		}(task, scheduled)
	}
	if wait := time.Until(deadline); wait > 0 {
		time.Sleep(wait)
	}
	requests.Wait()
	close(records)
	collection.Wait()
	cancelResources()
	r.Resources = <-resourceDone
	r.Sent = sent - lateDropped
	r.Dropped = dropped + lateDropped
	r.ReadLatency = percentiles(reads)
	r.ReadArrivalLatency = percentiles(arrivalReads)
	r.ScheduleDelay = percentiles(scheduleDelays)
	r.WriteLatency = percentiles(writes)
	r.MemoryAfter = readMemory(ctx, f.rdb)
	if r.MemoryBefore.UsedMemory < 0 || r.MemoryAfter.UsedMemory < 0 {
		return r, fmt.Errorf("Redis resource sampling failed")
	}
	if err = buffer.Flush(); err != nil {
		return r, err
	}
	if encodeErr != nil {
		return r, encodeErr
	}
	r.Valid = r.FreshnessViolations == 0 && r.Errors == 0 && r.Dropped == 0 && r.ReadCounters.RedisErrors+r.WriteCounters.RedisErrors == 0 && r.ReadCounters.SQLErrors+r.WriteCounters.SQLErrors == 0
	if r.MemoryAfter.EvictedKeys != f.startEvicted {
		r.Valid = false
	}
	if r.Planned != r.Sent+r.Dropped {
		return r, fmt.Errorf("scheduler accounting failed")
	}
	if r.FreshnessViolations > 0 {
		return r, fmt.Errorf("freshness contract failed in %s", r.ID)
	}
	return r, nil
}

func inspectNS(o *oracle, r *feed.FeedResult, start, end time.Time) (bool, int64, bool) {
	if r == nil {
		return false, 0, true
	}
	stale, age, violation := o.inspect(r, start, end)
	return stale, age.Nanoseconds(), violation
}
func percentiles(samples []int64) latencyStats {
	if len(samples) == 0 {
		return latencyStats{}
	}
	v := append([]int64(nil), samples...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	at := func(q float64) float64 {
		return float64(v[max(0, int(math.Ceil(q*float64(len(v))))-1)]) / float64(time.Millisecond)
	}
	return latencyStats{at(.50), at(.95), at(.99)}
}

func readMemory(ctx context.Context, rdb *redis.Client) memorySnapshot {
	info, err := rdb.Info(ctx, "memory", "stats").Result()
	if err != nil {
		return memorySnapshot{UsedMemory: -1, DatasetBytes: -1, EvictedKeys: -1, Keys: -1}
	}
	fields := map[string]int64{}
	for _, line := range strings.Split(info, "\n") {
		p := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(p) == 2 {
			fields[p[0]], _ = strconv.ParseInt(p[1], 10, 64)
		}
	}
	keys, e := rdb.DBSize(ctx).Result()
	if e != nil {
		keys = -1
	}
	return memorySnapshot{fields["used_memory"], fields["used_memory_dataset"], fields["evicted_keys"], keys}
}
