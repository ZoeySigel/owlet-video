// Command capacity is a local-only, open-loop HTTP load generator and fixture
// builder. It never accepts arbitrary production targets or credentials.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-jwt/jwt/v5"
)

const dsn = "capacity:capacity-local-only@tcp(127.0.0.1:23316)/owlet_capacity?parseTime=True&charset=utf8mb4&loc=UTC"
const secret = "capacity-test-secret-never-use-in-production-2026"
const baseURL = "http://127.0.0.1:28080"
const userCount = 10000
const videoCount = 100000

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
func sessionID(id int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", id) }
func database() *sql.DB {
	db, err := sql.Open("mysql", dsn)
	must(err)
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(3)
	must(db.Ping())
	return db
}
func main() {
	mode := flag.String("mode", "load", "seed, load, or snapshot")
	scenario := flag.String("scenario", "latest", "latest, detail-hot, detail-random, hot, likes, write, async, mixed")
	rate := flag.Int("rate", 20, "scheduled requests per second")
	duration := flag.Duration("duration", time.Minute, "arrival window")
	drain := flag.Duration("drain", 30*time.Second, "max asynchronous drain time")
	output := flag.String("out", "capacity-result.json", "JSON result path")
	flag.Parse()
	if *rate < 1 || *rate > 2000 || *duration <= 0 || *duration > time.Hour || *drain < 0 || *drain > 5*time.Minute {
		log.Fatal("invalid bounded test configuration")
	}
	db := database()
	defer db.Close()
	switch *mode {
	case "seed":
		seed(db)
	case "load":
		load(db, *scenario, *rate, *duration, *drain, *output)
	case "snapshot":
		writeJSON(*output, snapshot(db))
	default:
		log.Fatal("unknown mode")
	}
}
func writeJSON(path string, value any) {
	must(os.MkdirAll(filepath.Dir(path), 0755))
	b, err := json.MarshalIndent(value, "", "  ")
	must(err)
	must(os.WriteFile(path, append(b, '\n'), 0644))
}
func insert(db *sql.DB, table, columns string, n int, row func(int) []any) {
	for start := 0; start < n; start += 500 {
		end := min(start+500, n)
		args := []any{}
		marks := []string{}
		for i := start; i < end; i++ {
			r := row(i)
			args = append(args, r...)
			marks = append(marks, "("+strings.TrimSuffix(strings.Repeat("?,", len(r)), ",")+")")
		}
		_, err := db.Exec("INSERT INTO "+table+" ("+columns+") VALUES "+strings.Join(marks, ","), args...)
		must(err)
		if end%50000 == 0 || end == n {
			log.Printf("seed %s: %d/%d", table, end, n)
		}
	}
}
func seed(db *sql.DB) {
	var existing int
	must(db.QueryRow("SELECT COUNT(*) FROM users").Scan(&existing))
	if existing != 0 {
		log.Fatal("seed requires an EMPTY owlet_capacity database; refuses to overwrite")
	}
	now := time.Now().UTC().Truncate(time.Second)
	insert(db, "users", "id,username,password_hash,bio,avatar_url,created_at", userCount, func(i int) []any {
		return []any{i + 1, fmt.Sprintf("capacity_%05d", i+1), "capacity-fixture-no-password", "capacity fixture", "", now.Add(-7 * 24 * time.Hour)}
	})
	insert(db, "sessions", "id,user_id,refresh_hash,expires_at,created_at", userCount, func(i int) []any {
		return []any{sessionID(i + 1), i + 1, strings.Repeat("0", 64), now.Add(24 * time.Hour), now}
	})
	insert(db, "videos", "id,user_id,title,description,file_path,size,cover_path,likes_count,comments_count,popularity,published_at,created_at", videoCount, func(i int) []any {
		return []any{i + 1, i%userCount + 1, fmt.Sprintf("Capacity video %d", i+1), "Fixture metadata #capacity", "capacity-placeholder.mp4", 1024, "", 0, 0, 0, now.Add(-72 * time.Hour).Add(time.Duration(i) * time.Second), now}
	})
	// Each like pair is unique. Half of the timeline is outside the hot window.
	insert(db, "likes", "user_id,video_id,created_at", 600000, func(i int) []any {
		vid := i%videoCount + 1
		uid := (vid+i/videoCount*997)%userCount + 1
		return []any{uid, vid, now.Add(-time.Duration(i%172800) * time.Second)}
	})
	insert(db, "comments", "user_id,video_id,body,created_at", 300000, func(i int) []any {
		return []any{i%userCount + 1, (i*7919)%videoCount + 1, "Capacity historical comment", now.Add(-time.Duration(i%172800) * time.Second)}
	})
	insert(db, "follows", "follower_id,following_id,created_at", 100000, func(i int) []any {
		uid := i%userCount + 1
		target := (uid+i/userCount)%userCount + 1
		return []any{uid, target, now}
	})
	_, err := db.Exec(`UPDATE videos v LEFT JOIN (SELECT video_id,COUNT(*) AS n FROM likes GROUP BY video_id) l ON l.video_id=v.id LEFT JOIN (SELECT video_id,COUNT(*) AS n FROM comments GROUP BY video_id) c ON c.video_id=v.id SET v.likes_count=COALESCE(l.n,0),v.comments_count=COALESCE(c.n,0),v.popularity=COALESCE(l.n,0)*3+COALESCE(c.n,0)*5`)
	must(err)
	writeJSON(".tools/capacity/fixture.json", map[string]any{"seededAt": now, "users": userCount, "videos": videoCount, "likes": 600000, "comments": 300000, "follows": 100000, "media": "metadata only; media bandwidth not measured"})
}

type Snapshot struct {
	At                time.Time `json:"at"`
	PendingOutbox     int64     `json:"pendingOutbox"`
	PendingCommands   int64     `json:"pendingCommands"`
	CompletedCommands int64     `json:"completedCommands"`
	FailedCommands    int64     `json:"failedCommands"`
	Notifications     int64     `json:"notifications"`
	Error             string    `json:"error,omitempty"`
}

func snapshot(db *sql.DB) Snapshot {
	s := Snapshot{At: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM outboxes WHERE published_at IS NULL),(SELECT COUNT(*) FROM interaction_commands WHERE completed_at IS NULL),(SELECT COUNT(*) FROM interaction_commands WHERE completed_at IS NOT NULL),(SELECT COUNT(*) FROM interaction_commands WHERE status>=400),(SELECT COUNT(*) FROM notifications)`).Scan(&s.PendingOutbox, &s.PendingCommands, &s.CompletedCommands, &s.FailedCommands, &s.Notifications)
	if err != nil {
		s.Error = err.Error()
	}
	return s
}

type Observation struct {
	Endpoint                      string
	Status                        int
	MS, LagMS                     float64
	Execution, Error, OperationID string
	Finished                      time.Time
}
type Distribution struct {
	Count              int `json:"count"`
	P50, P95, P99, Max float64
}

func distribution(values []float64) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	at := func(p float64) float64 { return values[max(0, int(math.Ceil(float64(len(values))*p))-1)] }
	return Distribution{len(values), at(.5), at(.95), at(.99), values[len(values)-1]}
}

type Result struct {
	Scenario                                                                                  string `json:"scenario"`
	TargetRPS                                                                                 int    `json:"targetRps"`
	Started, Finished                                                                         time.Time
	WindowSeconds                                                                             float64 `json:"windowSeconds"`
	Planned, Sent, Dropped, Success, Errors, Limited                                          int
	SuccessRPS                                                                                float64        `json:"successRps"`
	Status                                                                                    map[int]int    `json:"status"`
	Execution                                                                                 map[string]int `json:"execution"`
	LatencyMS, SuccessLatencyMS, SchedulerLagMS                                               Distribution
	Endpoints                                                                                 map[string]Distribution `json:"endpoints"`
	Samples                                                                                   []string                `json:"errorSamples"`
	Backlog                                                                                   []Snapshot              `json:"backlog"`
	DrainSeconds                                                                              float64                 `json:"drainSeconds"`
	CommandsCreated, CommandsCompleted, CommandsFailed, AcceptedAsync, AcceptedAsyncCompleted int64
	CommandCompletionMS                                                                       Distribution
}

func tokens() []string {
	values := make([]string, userCount)
	now := time.Now()
	for i := range values {
		claims := jwt.MapClaims{"kind": "access", "sid": sessionID(i + 1), "sub": fmt.Sprint(i + 1), "iat": now.Unix(), "exp": now.Add(2 * time.Hour).Unix()}
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
		must(err)
		values[i] = raw
	}
	return values
}
func route(scenario string, i int) (method, path, endpoint string, auth, async bool) {
	method = "GET"
	endpoint = scenario
	if scenario == "mixed" {
		switch i % 10 {
		case 0, 1, 2:
			scenario = "detail-hot"
		case 3, 4:
			scenario = "detail-random"
		case 5, 6:
			scenario = "latest"
		case 7, 8:
			scenario = "likes"
		default:
			scenario = "write"
		}
		endpoint = scenario
	}
	switch scenario {
	case "latest":
		path = "/api/v1/videos?sort=latest"
	case "detail-hot":
		path = "/api/v1/videos/100000"
	case "detail-random":
		path = fmt.Sprintf("/api/v1/videos/%d", (i*7919)%videoCount+1)
	case "hot":
		path = "/api/v1/videos?sort=hot"
	case "likes":
		path = "/api/v1/videos?sort=likes"
	case "write", "async":
		method = "POST"
		auth = true
		async = scenario == "async"
		path = fmt.Sprintf("/api/v1/videos/%d/comments", (i*7919)%videoCount+1)
	default:
		log.Fatal("unsupported scenario")
	}
	return
}
func load(db *sql.DB, scenario string, rate int, duration, drain time.Duration, out string) {
	route(scenario, 0)
	var n int
	must(db.QueryRow("SELECT COUNT(*) FROM videos").Scan(&n))
	if n < videoCount {
		log.Fatal("capacity fixture not ready")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024, MaxConnsPerHost: 1024, IdleConnTimeout: 30 * time.Second}}
	defer client.CloseIdleConnections()
	auth := tokens()
	planned := int(duration.Seconds() * float64(rate))
	events := make(chan Observation, planned)
	slots := make(chan struct{}, 512)
	var wg sync.WaitGroup
	r := Result{Scenario: scenario, TargetRPS: rate, WindowSeconds: duration.Seconds(), Planned: planned, Status: map[int]int{}, Execution: map[string]int{}, Endpoints: map[string]Distribution{}}
	r.Backlog = append(r.Backlog, snapshot(db))
	r.Started = time.Now().UTC()
	deadline := r.Started.Add(duration)
	stopMonitor := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopMonitor:
				return
			case <-ticker.C:
				r.Backlog = append(r.Backlog, snapshot(db))
				if len(r.Backlog)%12 == 0 {
					last := r.Backlog[len(r.Backlog)-1]
					fmt.Printf("PROGRESS elapsed=%.0fs pendingOutbox=%d pendingCommands=%d completed=%d monitorError=%q\n", time.Since(r.Started).Seconds(), last.PendingOutbox, last.PendingCommands, last.CompletedCommands, last.Error)
				}
			}
		}
	}()
	for i := 0; i < planned; i++ {
		due := r.Started.Add(time.Duration(float64(i) / float64(rate) * float64(time.Second)))
		if wait := time.Until(due); wait > 0 {
			time.Sleep(wait)
		}
		lag := time.Since(due)
		if time.Now().After(deadline) || lag > 100*time.Millisecond {
			r.Dropped++
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			r.Dropped++
			continue
		}
		r.Sent++
		wg.Add(1)
		go func(i int, lag time.Duration) {
			defer wg.Done()
			defer func() { <-slots }()
			method, path, endpoint, needsAuth, isAsync := route(scenario, i)
			var body io.Reader
			if method == "POST" {
				body = strings.NewReader(fmt.Sprintf(`{"body":"capacity %s request %d"}`, r.Started.Format(time.RFC3339Nano), i))
			}
			req, err := http.NewRequest(method, baseURL+path, body)
			if err != nil {
				panic(err)
			}
			if needsAuth {
				req.Header.Set("Authorization", "Bearer "+auth[i%len(auth)])
				req.Header.Set("Content-Type", "application/json")
			}
			if isAsync {
				req.Header.Set("Prefer", "respond-async")
			}
			started := time.Now()
			o := Observation{Endpoint: endpoint, LagMS: float64(lag.Microseconds()) / 1000}
			resp, err := client.Do(req)
			if err != nil {
				o.Error = err.Error()
			} else {
				b, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
				resp.Body.Close()
				o.Status = resp.StatusCode
				o.Execution = resp.Header.Get("X-Interaction-Execution")
				if e != nil {
					o.Error = e.Error()
				} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					var parsed map[string]json.RawMessage
					if json.Unmarshal(b, &parsed) != nil {
						o.Error = "invalid JSON response"
					} else if resp.StatusCode == 202 {
						_ = json.Unmarshal(parsed["operationId"], &o.OperationID)
						if o.OperationID == "" {
							o.Error = "202 without operationId"
						}
					} else if method == "POST" {
						if len(parsed["id"]) == 0 {
							o.Error = "comment result missing id"
						}
					} else if strings.HasPrefix(endpoint, "detail") {
						if len(parsed["id"]) == 0 {
							o.Error = "detail missing id"
						}
					} else {
						if len(parsed["items"]) == 0 {
							o.Error = "feed missing items"
						}
					}
				} else {
					o.Error = string(b)
					if len(o.Error) > 240 {
						o.Error = o.Error[:240]
					}
				}
			}
			o.MS = float64(time.Since(started).Microseconds()) / 1000
			o.Finished = time.Now()
			events <- o
		}(i, lag)
	}
	if wait := time.Until(deadline); wait > 0 {
		time.Sleep(wait)
	}
	wg.Wait()
	close(events)
	close(stopMonitor)
	<-monitorDone
	r.Backlog = append(r.Backlog, snapshot(db))
	latencies := []float64{}
	successLatencies := []float64{}
	lags := []float64{}
	byEndpoint := map[string][]float64{}
	accepted := map[string]bool{}
	for o := range events {
		r.Status[o.Status]++
		latencies = append(latencies, o.MS)
		lags = append(lags, o.LagMS)
		byEndpoint[o.Endpoint] = append(byEndpoint[o.Endpoint], o.MS)
		if o.Error == "" {
			r.Success++
			successLatencies = append(successLatencies, o.MS)
			if !o.Finished.After(deadline) {
				r.SuccessRPS += 1 / duration.Seconds()
			}
		} else {
			r.Errors++
			if len(r.Samples) < 12 {
				r.Samples = append(r.Samples, fmt.Sprintf("%s status=%d %s", o.Endpoint, o.Status, o.Error))
			}
		}
		if o.Status == 429 {
			r.Limited++
		}
		if o.Execution != "" {
			r.Execution[o.Execution]++
		}
		if o.OperationID != "" {
			accepted[o.OperationID] = true
		}
	}
	r.LatencyMS = distribution(latencies)
	r.SuccessLatencyMS = distribution(successLatencies)
	r.SchedulerLagMS = distribution(lags)
	for endpoint, values := range byEndpoint {
		r.Endpoints[endpoint] = distribution(values)
	}
	drainStart := time.Now()
	drainEnd := drainStart.Add(drain)
	for time.Now().Before(drainEnd) {
		s := snapshot(db)
		r.Backlog = append(r.Backlog, s)
		if s.Error == "" && s.PendingOutbox == 0 && s.PendingCommands == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	r.DrainSeconds = time.Since(drainStart).Seconds()
	r.AcceptedAsync = int64(len(accepted))
	completion := []float64{}
	rows, err := db.Query("SELECT id,created_at,completed_at,status FROM interaction_commands WHERE created_at >= ? AND created_at <= ?", r.Started.Add(-time.Millisecond), deadline.Add(10*time.Second))
	must(err)
	for rows.Next() {
		var id string
		var created time.Time
		var completed sql.NullTime
		var status int
		must(rows.Scan(&id, &created, &completed, &status))
		r.CommandsCreated++
		if completed.Valid {
			r.CommandsCompleted++
			completion = append(completion, float64(completed.Time.Sub(created).Microseconds())/1000)
			if status >= 400 {
				r.CommandsFailed++
			}
			if accepted[id] && status < 400 {
				r.AcceptedAsyncCompleted++
			}
		}
	}
	must(rows.Err())
	rows.Close()
	r.CommandCompletionMS = distribution(completion)
	r.Finished = time.Now().UTC()
	writeJSON(out, r)
	fmt.Printf("%s target=%d sent=%d success=%d errors=%d dropped=%d p95=%.1fms p99=%.1fms drain=%.1fs\n", scenario, rate, r.Sent, r.Success, r.Errors, r.Dropped, r.LatencyMS.P95, r.LatencyMS.P99, r.DrainSeconds)
}
