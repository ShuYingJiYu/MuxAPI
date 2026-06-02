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
	Addr           string        // 监听地址
	DBPath         string        // SQLite 路径
	AdminToken     string        // 接入层鉴权 token（sub2api upstream key）
	ProbeInterval  time.Duration // 健康探测间隔
	ProbeModel     string        // 探测用的最小模型请求
	ProbePath      string        // 探测端点(OpenAI:/v1/chat/completions, Claude:/v1/messages)
	FailThreshold  int           // 连续失败多少次熔断
	Cooldown       time.Duration // 熔断冷却时长
	MaxRetries     int           // 同上游重试次数
	MonitorInterval time.Duration // 监控项探测间隔
}

func Load() *Config {
	loadDotEnv(".env")
	return &Config{
		Addr:           env("MUXAPI_ADDR", ":8080"),
		DBPath:         env("MUXAPI_DB", "muxapi.db"),
		AdminToken:     env("MUXAPI_TOKEN", ""),
		ProbeInterval:  envDur("MUXAPI_PROBE_INTERVAL", 15*time.Second),
		ProbeModel:     env("MUXAPI_PROBE_MODEL", "gpt-5.5"),
		ProbePath:      env("MUXAPI_PROBE_PATH", "/v1/chat/completions"),
		FailThreshold:  envInt("MUXAPI_FAIL_THRESHOLD", 3),
		Cooldown:       envDur("MUXAPI_COOLDOWN", 30*time.Second),
		MaxRetries:     envInt("MUXAPI_MAX_RETRIES", 3),
		MonitorInterval: envDur("MUXAPI_MONITOR_INTERVAL", 5*time.Minute),
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

func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}
