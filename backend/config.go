package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr, MySQLDSN, RedisAddr, RedisPassword, RabbitURL, JWTSecret, PublicURL, DataDir string
	SecureCookies                                                                      bool
	MaxMediaBytes                                                                      int64
	HotWindow, RankRefresh, SnapshotTTL, RedisTimeout, DBTimeout                       time.Duration
	RankLimit                                                                          int
	WriteRPS, WriteBurst, WriteMaxPending                                              int
	WriteMaxAge                                                                        time.Duration
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func configFromEnv() Config {
	max, _ := strconv.ParseInt(env("MAX_MEDIA_BYTES", "8589934592"), 10, 64)
	return Config{
		Addr:            env("ADDR", "127.0.0.1:8080"),
		MySQLDSN:        os.Getenv("MYSQL_DSN"),
		RedisAddr:       env("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:   os.Getenv("REDIS_PASSWORD"),
		RabbitURL:       os.Getenv("RABBITMQ_URL"),
		JWTSecret:       os.Getenv("JWT_SECRET"),
		PublicURL:       strings.TrimRight(env("PUBLIC_URL", "http://localhost:3000"), "/"),
		DataDir:         env("DATA_DIR", "./data"),
		SecureCookies:   env("SECURE_COOKIES", "false") == "true",
		MaxMediaBytes:   max,
		HotWindow:       durationEnv("HOT_WINDOW", 24*time.Hour),
		RankRefresh:     durationEnv("RANK_REFRESH", time.Minute),
		SnapshotTTL:     durationEnv("FEED_SNAPSHOT_TTL", 15*time.Minute),
		RedisTimeout:    durationEnv("REDIS_TIMEOUT", 200*time.Millisecond),
		DBTimeout:       durationEnv("DB_TIMEOUT", 3*time.Second),
		RankLimit:       intEnv("FEED_RANK_LIMIT", 2000),
		WriteRPS:        positiveEnv("WRITE_RPS", 20),
		WriteBurst:      positiveEnv("WRITE_BURST", 20),
		WriteMaxPending: positiveEnv("WRITE_MAX_PENDING", 200),
		WriteMaxAge:     durationEnv("WRITE_MAX_AGE", 10*time.Second),
	}
}

func positiveEnv(key string, fallback int) int {
	v, err := strconv.Atoi(env(key, strconv.Itoa(fallback)))
	if err != nil || v < 1 || v > 10000 {
		return fallback
	}
	return v
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(env(key, fallback.String()))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}
func intEnv(key string, fallback int) int {
	v, err := strconv.Atoi(env(key, strconv.Itoa(fallback)))
	if err != nil || v < 20 || v > 10000 {
		return fallback
	}
	return v
}
func (c Config) defaults() Config {
	if c.WriteRPS <= 0 {
		c.WriteRPS = 20
	}
	if c.WriteBurst <= 0 {
		c.WriteBurst = 20
	}
	if c.WriteMaxPending <= 0 {
		c.WriteMaxPending = 200
	}
	if c.WriteMaxAge <= 0 {
		c.WriteMaxAge = 10 * time.Second
	}
	if c.HotWindow <= 0 {
		c.HotWindow = 24 * time.Hour
	}
	if c.RankRefresh <= 0 {
		c.RankRefresh = time.Minute
	}
	if c.SnapshotTTL <= c.RankRefresh {
		c.SnapshotTTL = 15 * time.Minute
		if c.SnapshotTTL <= c.RankRefresh {
			c.SnapshotTTL = 2 * c.RankRefresh
		}
	}
	if c.RedisTimeout <= 0 {
		c.RedisTimeout = 200 * time.Millisecond
	}
	if c.DBTimeout <= 0 {
		c.DBTimeout = 3 * time.Second
	}
	if c.RankLimit < 20 || c.RankLimit > 10000 {
		c.RankLimit = 2000
	}
	return c
}

const accessTTL = 15 * time.Minute
const refreshTTL = 7 * 24 * time.Hour
