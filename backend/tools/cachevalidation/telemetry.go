package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm/logger"
)

// Redis byte counters measure application payload, not RESP framing, TCP, TLS,
// client retries, or wire traffic. Writes include written keys and values/member
// arguments; reads include returned scalar payloads. A pipeline counts each
// command, including MULTI/EXEC when go-redis exposes them to its hook.
type Counters struct {
	SQLQueries      int64            `json:"sql_queries"`
	SQLRows         int64            `json:"sql_rows"`
	SQLErrors       int64            `json:"sql_errors"`
	SQLDurationNS   int64            `json:"sql_duration_ns"`
	RedisCommands   int64            `json:"redis_commands"`
	RedisErrors     int64            `json:"redis_errors"`
	RedisReadBytes  int64            `json:"redis_read_bytes"`
	RedisWriteBytes int64            `json:"redis_write_bytes"`
	KeysDeleted     int64            `json:"keys_deleted"`
	KeysWritten     int64            `json:"keys_written"`
	IndexWrites     int64            `json:"index_writes"`
	Hits            map[string]int64 `json:"hits"`
	Misses          map[string]int64 `json:"misses"`
}

func (c Counters) Add(other Counters) Counters {
	result := Counters{
		SQLQueries: c.SQLQueries + other.SQLQueries, SQLRows: c.SQLRows + other.SQLRows,
		SQLErrors: c.SQLErrors + other.SQLErrors, SQLDurationNS: c.SQLDurationNS + other.SQLDurationNS,
		RedisCommands: c.RedisCommands + other.RedisCommands, RedisErrors: c.RedisErrors + other.RedisErrors,
		RedisReadBytes: c.RedisReadBytes + other.RedisReadBytes, RedisWriteBytes: c.RedisWriteBytes + other.RedisWriteBytes,
		KeysDeleted: c.KeysDeleted + other.KeysDeleted, KeysWritten: c.KeysWritten + other.KeysWritten,
		IndexWrites: c.IndexWrites + other.IndexWrites, Hits: copyCounts(c.Hits), Misses: copyCounts(c.Misses),
	}
	for key, n := range other.Hits {
		result.Hits[key] += n
	}
	for key, n := range other.Misses {
		result.Misses[key] += n
	}
	return result
}

func copyCounts(values map[string]int64) map[string]int64 {
	copy := make(map[string]int64, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

type operationKey struct{}
type operation struct {
	mu       sync.Mutex
	counters Counters
}

func withOperation(ctx context.Context) (context.Context, *operation) {
	op := &operation{counters: Counters{Hits: map[string]int64{}, Misses: map[string]int64{}}}
	return context.WithValue(ctx, operationKey{}, op), op
}

func operationFrom(ctx context.Context) *operation {
	op, _ := ctx.Value(operationKey{}).(*operation)
	return op
}

func (op *operation) add(delta Counters) {
	if op == nil {
		return
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	op.counters = op.counters.Add(delta)
}

func (op *operation) Snapshot() Counters {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.counters.Add(Counters{})
}

func observeCache(ctx context.Context, layer string, hits, misses int) {
	operationFrom(ctx).add(Counters{Hits: map[string]int64{layer: int64(hits)}, Misses: map[string]int64{layer: int64(misses)}})
}

type sqlMeter struct{}

func newSQLMeter() logger.Interface                            { return sqlMeter{} }
func (sqlMeter) LogMode(logger.LogLevel) logger.Interface      { return sqlMeter{} }
func (sqlMeter) Info(context.Context, string, ...interface{})  {}
func (sqlMeter) Warn(context.Context, string, ...interface{})  {}
func (sqlMeter) Error(context.Context, string, ...interface{}) {}
func (sqlMeter) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	op := operationFrom(ctx)
	if op == nil {
		return
	}
	_, rows := fc()
	if rows < 0 {
		rows = 0
	}
	delta := Counters{SQLQueries: 1, SQLRows: rows, SQLDurationNS: time.Since(begin).Nanoseconds()}
	if err != nil {
		delta.SQLErrors = 1
	}
	op.add(delta)
}

type redisMeter struct{}

func installRedisMeter(client *redis.Client) { client.AddHook(redisMeter{}) }
func (redisMeter) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (redisMeter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		meterRedisCommand(ctx, cmd, err)
		return err
	}
}
func (redisMeter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		err := next(ctx, commands)
		for _, cmd := range commands {
			meterRedisCommand(ctx, cmd, cmd.Err())
		}
		return err
	}
}

func meterRedisCommand(ctx context.Context, cmd redis.Cmder, err error) {
	op := operationFrom(ctx)
	if op == nil {
		return
	}
	delta := Counters{RedisCommands: 1}
	if err != nil && !errors.Is(err, redis.Nil) {
		delta.RedisErrors = 1
	}
	if err == nil {
		value := redisCommandValue(cmd)
		delta.RedisReadBytes = payloadBytes(reflect.ValueOf(value))
		args := cmd.Args()
		name := strings.ToLower(cmd.Name())
		if name == "del" || name == "unlink" {
			if n, ok := value.(int64); ok {
				delta.KeysDeleted = n
				if n > 0 {
					for _, key := range args[1:] {
						delta.RedisWriteBytes += argBytes(key)
					}
				}
			}
		}
		writeStart := 0
		switch name {
		case "set", "setex", "psetex":
			if len(args) >= 3 && value != nil {
				delta.KeysWritten = 1
				delta.RedisWriteBytes = argBytes(args[1])
				if name == "set" {
					delta.RedisWriteBytes += argBytes(args[2])
				} else if len(args) >= 4 {
					delta.RedisWriteBytes += argBytes(args[3])
				}
			}
		case "mset":
			writeStart = 1
			delta.KeysWritten = int64((len(args) - 1) / 2)
		case "hset", "hmset", "hincrby", "zadd", "sadd", "lpush", "rpush":
			writeStart = 1
			delta.KeysWritten = 1
		case "zrem", "srem", "zremrangebyscore":
			if n, ok := value.(int64); ok && n > 0 {
				writeStart = 1
				delta.KeysWritten = 1
			}
		case "expire", "pexpire", "expireat", "pexpireat":
			if yes, ok := value.(int64); ok && yes == 1 {
				writeStart = 1
				delta.KeysWritten = 1
			}
		}
		if writeStart > 0 {
			for _, arg := range args[writeStart:] {
				delta.RedisWriteBytes += argBytes(arg)
			}
		}
		if len(args) > 1 && strings.HasPrefix(fmt.Sprint(args[1]), wholeIndexPrefix) {
			switch name {
			case "zadd", "zrem", "zremrangebyscore", "expire", "pexpire", "del", "unlink":
				delta.IndexWrites = 1
			}
		}
	}
	op.add(delta)
}

func argBytes(arg interface{}) int64 {
	if value, ok := arg.([]byte); ok {
		return int64(len(value))
	}
	return int64(len(fmt.Sprint(arg)))
}

func redisCommandValue(cmd redis.Cmder) interface{} {
	switch value := cmd.(type) {
	case *redis.StringCmd:
		return value.Val()
	case *redis.SliceCmd:
		return value.Val()
	case *redis.StringSliceCmd:
		return value.Val()
	case *redis.IntCmd:
		return value.Val()
	case *redis.StatusCmd:
		return value.Val()
	case *redis.BoolCmd:
		if value.Val() {
			return int64(1)
		}
		return int64(0)
	case *redis.DurationCmd:
		if value.Val() < 0 {
			return int64(value.Val())
		}
		if cmd.Name() == "pttl" {
			return int64(value.Val() / time.Millisecond)
		}
		return int64(value.Val() / time.Second)
	case *redis.MapStringStringCmd:
		return value.Val()
	case *redis.ZSliceCmd:
		return value.Val()
	case *redis.Cmd:
		return value.Val()
	default:
		return nil
	}
}

func payloadBytes(value reflect.Value) int64 {
	if !value.IsValid() {
		return 0
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0
		}
		return payloadBytes(value.Elem())
	}
	switch value.Kind() {
	case reflect.String:
		return int64(value.Len())
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return int64(value.Len())
		}
		var total int64
		for i := 0; i < value.Len(); i++ {
			total += payloadBytes(value.Index(i))
		}
		return total
	case reflect.Map:
		var total int64
		iter := value.MapRange()
		for iter.Next() {
			total += payloadBytes(iter.Key()) + payloadBytes(iter.Value())
		}
		return total
	case reflect.Struct:
		var total int64
		for i := 0; i < value.NumField(); i++ {
			total += payloadBytes(value.Field(i))
		}
		return total
	default:
		if value.CanInterface() {
			return int64(len(fmt.Sprint(value.Interface())))
		}
		return 0
	}
}
