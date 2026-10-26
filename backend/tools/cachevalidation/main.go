// cachevalidation compares cache organizations, not production capacity.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

const purpose = "owlet-cache-split-validation-v1"
const markerKey = "cachevalidation:marker"

type config struct {
	Profile        string        `json:"profile"`
	Duration       time.Duration `json:"duration_ns"`
	Warmup         time.Duration `json:"warmup_ns"`
	Rounds         int           `json:"rounds"`
	Rates          []int         `json:"rates"`
	Users          int           `json:"users"`
	Videos         int           `json:"videos"`
	Seed           int64         `json:"seed"`
	PageSize       int           `json:"page_size"`
	MaxInflight    int           `json:"max_inflight"`
	RequestTimeout time.Duration `json:"request_timeout_ns"`
	Output         string        `json:"-"`
	MySQLDSN       string        `json:"-"`
	RedisAddr      string        `json:"redis_addr"`
}

func parseConfig() (config, error) {
	c := config{PageSize: 20, MaxInflight: 256, RequestTimeout: 3 * time.Second}
	var rates string
	flag.StringVar(&c.Profile, "profile", "full", "full or smoke (short qualification, not capacity evidence)")
	flag.StringVar(&c.MySQLDSN, "mysql-dsn", os.Getenv("CACHE_VALIDATION_MYSQL_DSN"), "dedicated localhost cache_validation database DSN")
	flag.StringVar(&c.RedisAddr, "redis-addr", os.Getenv("CACHE_VALIDATION_REDIS_ADDR"), "dedicated empty localhost Redis")
	flag.StringVar(&c.Output, "output", "", "new results directory; existing results are never overwritten")
	flag.DurationVar(&c.Duration, "duration", 60*time.Second, "measurement duration per variant")
	flag.DurationVar(&c.Warmup, "warmup", 30*time.Second, "unmeasured warmup duration")
	flag.IntVar(&c.Rounds, "rounds", 3, "paired repetitions")
	flag.StringVar(&rates, "rates", "20,100", "open-loop total operations per second")
	flag.IntVar(&c.Users, "users", 1000, "fixture users")
	flag.IntVar(&c.Videos, "videos", 10000, "fixture videos (metadata only)")
	flag.Int64Var(&c.Seed, "seed", 20261024, "deterministic fixture/trace seed")
	flag.Parse()
	seen := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if c.Profile == "smoke" {
		if !seen["duration"] {
			c.Duration = 5 * time.Second
		}
		if !seen["warmup"] {
			c.Warmup = time.Second
		}
		if !seen["rounds"] {
			c.Rounds = 1
		}
		if !seen["rates"] {
			rates = "20"
		}
		if !seen["users"] {
			c.Users = 100
		}
		if !seen["videos"] {
			c.Videos = 1000
		}
	} else if c.Profile != "full" {
		return c, errors.New("profile must be full or smoke")
	}
	for _, s := range strings.Split(rates, ",") {
		n, e := strconv.Atoi(strings.TrimSpace(s))
		if e != nil || n < 1 || n > 10000 {
			return c, fmt.Errorf("invalid rate %q", s)
		}
		c.Rates = append(c.Rates, n)
	}
	if c.Duration < time.Second || c.Warmup < 0 || c.Rounds < 1 || c.Users < 4 || c.Videos < 100 || len(c.Rates) == 0 {
		return c, errors.New("require duration >=1s, warmup >=0, rounds >=1, users >=4, videos >=100")
	}
	if c.MySQLDSN == "" || c.RedisAddr == "" {
		return c, errors.New("mysql-dsn and redis-addr are required; use scripts/validate-cache.ps1")
	}
	if c.Output == "" {
		c.Output = filepath.Join("..", "docs", "cache-validation", "results", time.Now().UTC().Format("20060102T150405Z"))
	}
	var err error
	c.Output, err = filepath.Abs(c.Output)
	return c, err
}

func main() {
	c, err := parseConfig()
	if err == nil {
		err = execute(c)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cache validation:", err)
		os.Exit(1)
	}
}

func execute(c config) (retErr error) {
	if entries, err := os.ReadDir(c.Output); err == nil && len(entries) > 0 {
		return fmt.Errorf("refusing non-empty output directory %s", c.Output)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Join(c.Output, "runs"), 0755); err != nil {
		return err
	}
	m := collectManifest(c)
	defer func() {
		m.FinishedAt = time.Now().UTC()
		if retErr != nil {
			m.Failure = retErr.Error()
		}
		if err := writeJSON(filepath.Join(c.Output, "manifest.json"), m); err != nil && retErr == nil {
			retErr = err
		}
	}()
	if err := saveSourceSnapshot(c.Output); err != nil {
		return err
	}
	catalog := makeCatalog(c.Videos, c.PageSize, 20)
	if err := writeJSON(filepath.Join(c.Output, "catalog.json"), catalog); err != nil {
		return err
	}
	parsed, err := mysqldriver.ParseDSN(c.MySQLDSN)
	if err != nil {
		return err
	}
	if parsed.DBName != "cache_validation" || parsed.Net != "tcp" || !isLoopbackAddress(parsed.Addr) || !isLoopbackAddress(c.RedisAddr) {
		return errors.New("only localhost TCP and database cache_validation are accepted")
	}
	db, err := gorm.Open(mysql.Open(c.MySQLDSN), &gorm.Config{Logger: newSQLMeter(), SkipDefaultTransaction: true})
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(32)
	sqlDB.SetMaxIdleConns(16)
	rdb := redis.NewClient(&redis.Options{Addr: c.RedisAddr, PoolSize: 64, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, ClientName: "owlet-cache-validation"})
	defer rdb.Close()
	installRedisMeter(rdb)
	ctx := context.Background()
	lockConn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer lockConn.Close()
	var acquired int
	if err = lockConn.QueryRowContext(ctx, "SELECT GET_LOCK(?,0)", purpose).Scan(&acquired); err != nil {
		return err
	}
	if acquired != 1 {
		return fmt.Errorf("another validation process owns this fixture")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = lockConn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", purpose)
	}()
	if err = authorizeAndPrepare(ctx, db, rdb); err != nil {
		return err
	}
	_ = db.Raw("SELECT VERSION()").Scan(&m.MySQLVersion).Error
	m.RedisServer, _ = rdb.Info(ctx, "server").Result()
	if err = writeJSON(filepath.Join(c.Output, "manifest.json"), m); err != nil {
		return err
	}
	f := &fixture{cfg: c, db: db, rdb: rdb}
	fmt.Println("Running correctness gates with real MySQL and Redis...")
	gates, err := runCorrectness(ctx, f)
	if werr := writeJSON(filepath.Join(c.Output, "correctness.json"), gates); werr != nil {
		return werr
	}
	if err != nil {
		_ = writeSummary(c, nil, nil, gates)
		return err
	}
	fmt.Println("Measuring invalidation/rebuild fanout (K=1,5,20)...")
	mechanisms, err := runMechanisms(ctx, f)
	if werr := writeJSON(filepath.Join(c.Output, "mechanisms.json"), mechanisms); werr != nil {
		return werr
	}
	if err != nil {
		_ = writeSummary(c, mechanisms, nil, gates)
		return err
	}
	var runs []runResult
	defer func() {
		if e := writeSummary(c, mechanisms, runs, gates); e != nil && retErr == nil {
			retErr = e
		}
	}()
	for round := 0; round < c.Rounds; round++ {
		for _, scenario := range []string{"read", "count", "card"} {
			for _, rate := range c.Rates {
				trace := makeTrace(c.Seed+int64(round), scenario, rate, c.Duration, len(catalog.Pages), c.Users)
				traceName := fmt.Sprintf("trace-%s-%d-r%d.jsonl", scenario, rate, round+1)
				if err = writeTrace(filepath.Join(c.Output, "runs", traceName), trace); err != nil {
					return err
				}
				modes := []string{"whole", "split"}
				if round%2 == 1 {
					modes = []string{"split", "whole"}
				}
				for _, mode := range modes {
					fmt.Printf("round %d/%d scenario=%s rate=%d mode=%s\n", round+1, c.Rounds, scenario, rate, mode)
					r, runErr := runLoad(ctx, f, catalog, trace, mode, scenario, rate, round+1)
					r.TraceFile = traceName
					runs = append(runs, r)
					if werr := writeJSON(filepath.Join(c.Output, "runs", r.ID+".json"), r); werr != nil {
						return werr
					}
					fmt.Printf("  read p95=%.2fms SQL/read=%.2f stale=%d errors=%d dropped=%d\n", r.ReadLatency.P95MS, r.SQLPerRead(), r.StaleReads, r.Errors, r.Dropped)
					if runErr != nil {
						return runErr
					}
				}
			}
		}
	}
	fmt.Println("Results:", filepath.Join(c.Output, "summary.md"))
	for _, r := range runs {
		if !r.Valid {
			return fmt.Errorf("one or more load runs invalid; inspect summary.md and raw results")
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(path, append(b, '\n'), 0644)
}
