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
	RedisTimeout, DBTimeout                                                            time.Duration
	RedisDB                                                                            int
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
		Addr:          env("ADDR", "127.0.0.1:8080"),
		MySQLDSN:      os.Getenv("MYSQL_DSN"),
		RedisAddr:     env("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
		RedisDB:       intEnv("REDIS_DB", 0),
		RabbitURL:     os.Getenv("RABBITMQ_URL"),
		JWTSecret:     os.Getenv("JWT_SECRET"),
		PublicURL:     strings.TrimRight(env("PUBLIC_URL", "http://localhost:3000"), "/"),
		DataDir:       env("DATA_DIR", "./data-core"),
		SecureCookies: env("SECURE_COOKIES", "false") == "true",
		MaxMediaBytes: max,
		RedisTimeout:  durationEnv("REDIS_TIMEOUT", 200*time.Millisecond),
		DBTimeout:     durationEnv("DB_TIMEOUT", 3*time.Second),
	}
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
	if err != nil || v < 0 || v > 15 {
		return fallback
	}
	return v
}
func (c Config) defaults() Config {
	if c.RedisTimeout <= 0 {
		c.RedisTimeout = 200 * time.Millisecond
	}
	if c.DBTimeout <= 0 {
		c.DBTimeout = 3 * time.Second
	}
	return c
}

const accessTTL = 15 * time.Minute
const refreshTTL = 7 * 24 * time.Hour
