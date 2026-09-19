package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr, MySQLDSN, RedisAddr, RedisPassword, RabbitURL, JWTSecret, PublicURL, DataDir string
	SecureCookies bool
	MaxMediaBytes int64
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" { return v }
	return fallback
}

func configFromEnv() Config {
	max, _ := strconv.ParseInt(env("MAX_MEDIA_BYTES", "8589934592"), 10, 64)
	return Config{
		Addr: env("ADDR", "127.0.0.1:8080"),
		MySQLDSN: os.Getenv("MYSQL_DSN"),
		RedisAddr: env("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
		RabbitURL: os.Getenv("RABBITMQ_URL"),
		JWTSecret: os.Getenv("JWT_SECRET"),
		PublicURL: strings.TrimRight(env("PUBLIC_URL", "http://localhost:3000"), "/"),
		DataDir: env("DATA_DIR", "./data"),
		SecureCookies: env("SECURE_COOKIES", "false") == "true",
		MaxMediaBytes: max,
	}
}

const accessTTL = 15 * time.Minute
const refreshTTL = 30 * 24 * time.Hour
