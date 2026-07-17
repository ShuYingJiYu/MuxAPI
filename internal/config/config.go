// Package config 从环境变量和可选的 .env 文件加载启动配置。
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// loadDotEnv 把同目录 .env 的 KEY=VALUE 注入环境（已存在的真实环境变量优先，不覆盖）。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			k = strings.TrimSpace(k)
			if os.Getenv(k) == "" {
				os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
			}
		}
	}
}

// Config 全局配置，从环境变量加载（可选 .env 文件提供默认值）
type Config struct {
	Addr          string        // 监听地址
	DatabaseURL   string        // PostgreSQL 连接串
	AdminToken    string        // 接入层鉴权 token（sub2api upstream key）
	FailThreshold int           // 连续失败多少次熔断
	Cooldown      time.Duration // 熔断冷却时长
	MaxRetries    int           // 单次下游请求最多尝试的上游数
	MaxBody       int64         // 请求体最大字节数（防 DoS），默认 32MB
}

// Load 返回带默认值的启动配置；同名系统环境变量优先于 .env。
func Load() *Config {
	loadDotEnv(".env")
	return &Config{
		Addr:          env("MUXAPI_ADDR", ":8080"),
		DatabaseURL:   env("MUXAPI_DATABASE_URL", ""),
		AdminToken:    env("MUXAPI_TOKEN", ""),
		FailThreshold: envInt("MUXAPI_FAIL_THRESHOLD", 3),
		Cooldown:      envDur("MUXAPI_COOLDOWN", 30*time.Second),
		MaxRetries:    envInt("MUXAPI_MAX_RETRIES", 3),
		// 请求体上限：防 io.ReadAll 无限读导致 OOM/DoS。默认 32MB，单位字节。
		MaxBody: envInt64("MUXAPI_MAX_BODY", 32<<20),
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envInt64(k string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(k), 10, 64); err == nil {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}
