package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type manifest struct {
	Purpose           string            `json:"purpose"`
	Config            config            `json:"config"`
	StartedAt         time.Time         `json:"started_at"`
	FinishedAt        time.Time         `json:"finished_at"`
	Failure           string            `json:"failure,omitempty"`
	GoVersion         string            `json:"go_version"`
	GOOS              string            `json:"goos"`
	GOARCH            string            `json:"goarch"`
	LogicalCPUs       int               `json:"logical_cpus"`
	ExecutableSHA256  string            `json:"executable_sha256"`
	GitCommit         string            `json:"git_commit"`
	GitStatus         string            `json:"git_status"`
	TrackedDiffSHA256 string            `json:"tracked_diff_sha256"`
	UntrackedFiles    map[string]string `json:"untracked_file_sha256"`
	MySQLVersion      string            `json:"mysql_version"`
	RedisServer       string            `json:"redis_server"`
	Notes             []string          `json:"notes"`
}

func collectManifest(c config) manifest {
	git := func(args ...string) []byte { out, _ := exec.Command("git", args...).Output(); return out }
	diff := sha256.Sum256(git("diff", "--binary", "HEAD"))
	m := manifest{Purpose: purpose, Config: c, StartedAt: time.Now().UTC(), GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(), GitCommit: strings.TrimSpace(string(git("rev-parse", "HEAD"))), GitStatus: string(git("status", "--short")), TrackedDiffSHA256: hex.EncodeToString(diff[:]), UntrackedFiles: map[string]string{}, Notes: []string{
		"A is a test-only full-public-page cache, not a historical version.",
		"B reuses production timeline/cache code; both variants share live viewer-state and publication checks.",
		"Redis byte counters are application payload/key/member bytes, not wire traffic; instrumentation overhead is included equally.",
		"Database transaction updates and cache invalidations are test events, not the production Redis/MQ like pipeline.",
		"HTTP is local and test-only; no login hashing, reverse proxy, TLS, video I/O, public network, or production capacity claim.",
		"Three paired full-profile repetitions report medians/ranges; smoke is qualification only.",
	}}
	root := strings.TrimSpace(string(git("rev-parse", "--show-toplevel")))
	if executable, e := os.Executable(); e == nil {
		if data, e := os.ReadFile(executable); e == nil {
			sum := sha256.Sum256(data)
			m.ExecutableSHA256 = hex.EncodeToString(sum[:])
		}
	}
	for _, name := range strings.Split(string(git("ls-files", "--others", "--exclude-standard", "-z")), "\x00") {
		if name == "" {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(name))
		rel, err := filepath.Rel(c.Output, full)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if b, err := os.ReadFile(full); err == nil {
			sum := sha256.Sum256(b)
			m.UntrackedFiles[name] = hex.EncodeToString(sum[:])
		}
	}
	return m
}

// Preserve the actual tracked working-tree changes and the experiment sources,
// so a hash in the manifest is backed by reviewable, reproducible content.
func saveSourceSnapshot(output string) error {
	rootBytes, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("locate source repository: %w", err)
	}
	root := strings.TrimSpace(string(rootBytes))
	destination := filepath.Join(output, "source")
	if err := os.MkdirAll(destination, 0755); err != nil {
		return err
	}
	diff, err := exec.Command("git", "-C", root, "diff", "--binary", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("capture working-tree changes: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "working-tree.patch"), diff, 0644); err != nil {
		return err
	}
	paths := []string{"backend/tools/cachevalidation", "compose.cache-validation.yml", "scripts/validate-cache.ps1"}
	for _, relative := range paths {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(relative)), func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			target := filepath.Join(destination, rel)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0644)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type rangeSummary struct{ Median, Min, Max float64 }

func summarize(values []float64) rangeSummary {
	if len(values) == 0 {
		return rangeSummary{}
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	mid := v[len(v)/2]
	if len(v)%2 == 0 {
		mid = (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	return rangeSummary{mid, v[0], v[len(v)-1]}
}
func formatRange(v []float64) string {
	s := summarize(v)
	return fmt.Sprintf("%.2f [%.2f, %.2f]", s.Median, s.Min, s.Max)
}

func writeSummary(c config, mechanisms []mechanismResult, runs []runResult, gates []gateResult) error {
	var b strings.Builder
	fmt.Fprint(&b, "# 缓存拆分对照结果\n\n")
	fmt.Fprintf(&b, "配置：`%s`；用户 %d，视频 %d，页大小 %d，种子 %d。每组采样 %s，预热 %s，重复 %d 轮。\n\n", c.Profile, c.Users, c.Videos, c.PageSize, c.Seed, c.Duration, c.Warmup, c.Rounds)
	if c.Profile == "smoke" {
		fmt.Fprint(&b, "**这是短资格验证，不能作为稳定吞吐或生产容量证据。**\n\n")
	}
	fmt.Fprint(&b, "A 为测试专用整页缓存；B 复用当前拆分缓存。业务写入使用同一数据库事务写入器，不经过 MQ。两组都保留用户状态与视频可见性查询。\n\n")
	expectedRuns, expectedMechanisms := c.Rounds*3*len(c.Rates)*2, c.Rounds*3*2*2
	fmt.Fprintf(&b, "完成负载组 %d / %d；完成机制组 %d / %d。\n\n", len(runs), expectedRuns, len(mechanisms), expectedMechanisms)
	fmt.Fprintln(&b, "## 正确性门槛\n\n| 检查 | 结果 | 说明 |\n|---|---|---|")
	allPassed := len(gates) > 0
	for _, g := range gates {
		state := "PASS"
		if !g.Passed {
			state = "FAIL"
			allPassed = false
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", g.Name, state, strings.ReplaceAll(g.Detail, "|", "/"))
	}
	if !allPassed {
		fmt.Fprintln(&b, "\n**正确性未通过，停止性能收益判定。**")
	}
	fmt.Fprint(&b, "\n## 单次更新与逐页重建\n\n下表为重复轮次的中位数及范围。维护阶段含相同的数据库更新；删除与反向索引写入分别统计。\n\n")
	fmt.Fprintln(&b, "| 更新 | K | 模式 | 删除键数 | 索引写操作 | 维护写入 B | 重建 SQL | 重建写入 B |\n|---|---:|---|---:|---:|---:|---:|---:|")
	for _, kind := range []string{"count", "card"} {
		for _, k := range []int{1, 5, 20} {
			for _, mode := range []string{"whole", "split"} {
				var del, index, write, sql, rebuild []float64
				for _, m := range mechanisms {
					if m.Kind == kind && m.Overlap == k && m.Mode == mode {
						del = append(del, float64(m.Maintenance.KeysDeleted))
						index = append(index, float64(m.Maintenance.IndexWrites))
						write = append(write, float64(m.Maintenance.RedisWriteBytes))
						sql = append(sql, float64(m.Rebuild.SQLQueries))
						rebuild = append(rebuild, float64(m.Rebuild.RedisWriteBytes))
					}
				}
				if len(del) > 0 {
					fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s | %s |\n", kind, k, mode, formatRange(del), formatRange(index), formatRange(write), formatRange(sql), formatRange(rebuild))
				}
			}
		}
	}
	fmt.Fprint(&b, "\n## 固定到达速率负载\n\nP95 是各轮读请求 P95 的中位数及范围，不是合并分位数；包括已发起的成功和失败请求。到达延迟从计划发起时间计起。SQL 包含缓存未命中与每次请求必须执行的用户态/可见性查询。Redis 总写 B/读包含读取回填与更新维护成本。\n\n")
	fmt.Fprintln(&b, "| 场景 | 计划操作/s | 模式 | 读 P95 ms | 到达→完成 P95 ms | SQL/读 | Redis 总写 B/读 | 成功操作/s | 完整公共缓存命中率 |\n|---|---:|---|---:|---:|---:|---:|---:|---:|")
	for _, scenario := range []string{"read", "count", "card"} {
		for _, rate := range c.Rates {
			for _, mode := range []string{"whole", "split"} {
				var lat, arrival, sql, write, rps, hit []float64
				for _, r := range runs {
					if r.Scenario == scenario && r.Rate == rate && r.Mode == mode {
						lat = append(lat, r.ReadLatency.P95MS)
						arrival = append(arrival, r.ReadArrivalLatency.P95MS)
						sql = append(sql, r.SQLPerRead())
						write = append(write, float64(r.ReadCounters.RedisWriteBytes+r.WriteCounters.RedisWriteBytes)/float64(max(1, r.ReadCount)))
						rps = append(rps, float64(r.SuccessfulInWindow)/r.DurationSeconds)
						hit = append(hit, 100*float64(r.CompleteCacheHits)/float64(max(1, r.ReadSuccess)))
					}
				}
				if len(lat) > 0 {
					fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s | %s | %s |\n", scenario, rate, mode, formatRange(lat), formatRange(arrival), formatRange(sql), formatRange(write), formatRange(rps), formatRange(hit))
				}
			}
		}
	}
	fmt.Fprintln(&b, "\n### 每轮有效性与资源\n\n| 运行 | 有效 | 错误 | 丢弃 | 陈旧读 | 最长观察陈旧 ms | 新鲜度违规 | 内存前/后 B | 淘汰增量 |\n|---|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, r := range runs {
		fmt.Fprintf(&b, "| %s | %t | %d | %d | %d | %.2f | %d | %d / %d | %d |\n", r.ID, r.Valid, r.Errors, r.Dropped, r.StaleReads, float64(r.MaxStaleAgeNS)/1e6, r.FreshnessViolations, r.MemoryBefore.UsedMemory, r.MemoryAfter.UsedMemory, r.MemoryAfter.EvictedKeys-r.MemoryBefore.EvictedKeys)
	}
	fmt.Fprint(&b, "\n## 判读与边界\n\n")
	valid := allPassed && len(runs) == expectedRuns && len(mechanisms) == expectedMechanisms
	for _, m := range mechanisms {
		valid = valid && m.Correct
	}
	for _, r := range runs {
		valid = valid && r.Valid
	}
	if !valid {
		fmt.Fprint(&b, "存在未通过或未完成的验证，不能据此声称稳定性能收益。\n\n")
	} else {
		fmt.Fprint(&b, "已完成的轮次通过所列正确性/调度门槛。请同时比较维护、重建、读取延迟和内存；不能用单个命中率或最好的一轮下结论。\n\n")
	}
	fmt.Fprintln(&b, "- 单次维护的删除量不等于总成本：A 的反向索引维护和之后页面重建均已计入。")
	fmt.Fprintln(&b, "- lookup 命中率见各轮 JSON；singleflight 内二次检查也计 lookup，不能把不同层的命中率平均后横比。")
	fmt.Fprintln(&b, "- 完整公共命中要求各层没有lookup miss且没有额外公共SQL；singleflight共享回源单列为无公共SQL读取，不冒充缓存命中。")
	fmt.Fprintln(&b, "- Redis 字节是成功命令的键/值/成员及读取结果的应用负载，不含 RESP、TCP/TLS，也不是 HTTP 响应字节。")
	fmt.Fprintln(&b, "- 并发陈旧度按请求开始前已提交的变更判断，拒绝超过校验时已提交上界的版本；只记录实际观察到的陈旧，不能证明未采样时段绝无陈旧。")
	fmt.Fprintln(&b, "- 资源采样只覆盖Redis内存/淘汰，不是完整CPU或MySQL资源分析；调度延迟P50/P95/P99另见各轮JSON。")
	fmt.Fprintln(&b, "- 反向索引需要与活页共存；主对照在预热或测量发生淘汰时标为无效，不把索引淘汰遗漏当成缓存收益。")
	fmt.Fprintln(&b, "- 冷读取、每次维护、读写延迟 P50/P95/P99、每秒资源样本及完整请求轨迹在 JSON/JSONL 中。")
	fmt.Fprintln(&b, "- 只覆盖本机时间线元数据、指定缓存预算和合成负载，不覆盖媒体传输、鉴权开销、MQ、推荐排序或生产最大容量。")
	if err := os.WriteFile(filepath.Join(c.Output, "summary.md"), []byte(b.String()), 0644); err != nil {
		return err
	}
	return writeCSV(filepath.Join(c.Output, "runs.csv"), runs)
}

func writeCSV(path string, runs []runResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"id", "mode", "scenario", "rate", "round", "valid", "read_p50_ms", "read_p95_ms", "read_p99_ms", "write_p95_ms", "sql_per_read", "redis_commands", "read_payload_bytes", "write_payload_bytes", "maintenance_deleted_keys", "index_writes", "errors", "dropped", "stale_reads", "memory_bytes"})
	for _, r := range runs {
		all := r.ReadCounters.Add(r.WriteCounters)
		row := []string{r.ID, r.Mode, r.Scenario, strconv.Itoa(r.Rate), strconv.Itoa(r.Round), strconv.FormatBool(r.Valid), fmt.Sprintf("%.4f", r.ReadLatency.P50MS), fmt.Sprintf("%.4f", r.ReadLatency.P95MS), fmt.Sprintf("%.4f", r.ReadLatency.P99MS), fmt.Sprintf("%.4f", r.WriteLatency.P95MS), fmt.Sprintf("%.4f", r.SQLPerRead()), fmt.Sprint(all.RedisCommands), fmt.Sprint(all.RedisReadBytes), fmt.Sprint(all.RedisWriteBytes), fmt.Sprint(r.WriteCounters.KeysDeleted), fmt.Sprint(all.IndexWrites), fmt.Sprint(r.Errors), fmt.Sprint(r.Dropped), fmt.Sprint(r.StaleReads), fmt.Sprint(r.MemoryAfter.UsedMemory)}
		if err = w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
