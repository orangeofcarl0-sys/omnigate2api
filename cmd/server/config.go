package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"omnigate2api/internal/pool"
)

// Config 顶层配置。
type Config struct {
	Listen       string `json:"listen"`
	APIKey       string `json:"api_key"` // 只读 env OMNIGATE_API_KEY
	AuthDir      string `json:"auth_dir"`
	StateFile    string `json:"state_file"`
	DefaultModel string `json:"default_model"`
	// OAuthCallbackHost 可选：远程授权时把回调指向公网地址（如 https://oneapi.example.com/codearts），
	// 让浏览器回调经反向代理进入本服务，而不是 127.0.0.1。留空则用本机端口。
	OAuthCallbackHost string `json:"oauth_callback_host,omitempty"`

	Cooldown struct {
		SoftRate    string `json:"soft_rate"`
		ErrThresh   int    `json:"err_threshold"`
		ErrCooldown string `json:"err_cooldown"`
	} `json:"cooldown"`

	Watch struct {
		Enabled           bool `json:"enabled"`
		PollMinutes       int  `json:"poll_minutes"`
		RefreshSkewM      int  `json:"refresh_skew_minutes"`
		KeepaliveInterval int  `json:"keepalive_interval_minutes"` // 保活心跳间隔（新增）
	} `json:"watch"`

	MaxConcurrent   int    `json:"max_concurrent"`   // 单账号最大并发数（新增）
	KeepaliveWindow string `json:"keepalive_window"` // 保活窗口（新增）

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`

	SoftRateDur    time.Duration
	ErrCooldownDur time.Duration
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:       ":7866",
		AuthDir:      "./auths",
		StateFile:    "./data/state.json",
		DefaultModel: "glm-5.2",
	}
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Watch.Enabled = true
	c.Watch.PollMinutes = 30
	c.Watch.RefreshSkewM = 30
	c.Watch.KeepaliveInterval = 15 // 新增：保活间隔 15 分钟
	c.MaxConcurrent = 1            // 上游单账号最多 3 并发会话但释放慢，串行最稳
	c.KeepaliveWindow = "10m"      // 新增：保活窗口 10 分钟
	c.Upstream.TimeoutSeconds = 120
	return c
}

// Load 加载配置 + OMNIGATE_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if c.APIKey == "" {
		c.APIKey = "dummy-key-for-codearts"
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("OMNIGATE_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("OMNIGATE_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("OMNIGATE_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("OMNIGATE_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("OMNIGATE_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("OMNIGATE_OAUTH_CALLBACK_HOST"); v != "" {
		c.OAuthCallbackHost = v
	}
	if v := os.Getenv("OMNIGATE_WATCH_ENABLED"); v != "" {
		c.Watch.Enabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("OMNIGATE_WATCH_POLL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.PollMinutes = n
		}
	}
	if v := os.Getenv("OMNIGATE_WATCH_REFRESH_SKEW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.RefreshSkewM = n
		}
	}
	if v := os.Getenv("OMNIGATE_WATCH_KEEPALIVE_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.KeepaliveInterval = n
		}
	}
	if v := os.Getenv("OMNIGATE_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxConcurrent = n
		}
	}
	if v := os.Getenv("OMNIGATE_KEEPALIVE_WINDOW"); v != "" {
		c.KeepaliveWindow = v
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	if c.Listen == "" {
		c.Listen = ":7866"
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	if c.Watch.PollMinutes <= 0 {
		c.Watch.PollMinutes = 30
	}
	if c.Watch.RefreshSkewM <= 0 {
		c.Watch.RefreshSkewM = 30
	}
	if c.Watch.KeepaliveInterval <= 0 {
		c.Watch.KeepaliveInterval = 15 // 默认保活间隔 15 分钟
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 1 // 上游并发会话释放慢，单账号串行最稳
	}
	return nil
}

// ToPoolConfig 转换为 pool.Config。
func (c *Config) ToPoolConfig() pool.Config {
	kw := c.KeepaliveWindow
	if kw == "" {
		kw = "10m"
	}
	dur, err := time.ParseDuration(kw)
	if err != nil {
		dur = 10 * time.Minute
	}
	return pool.Config{
		ErrThreshold:    c.Cooldown.ErrThresh,
		ErrCooldown:     c.ErrCooldownDur,
		SoftCooldown:    c.SoftRateDur,
		RefreshSkew:     time.Duration(c.Watch.RefreshSkewM) * time.Minute,
		MaxConcurrent:   c.MaxConcurrent,
		KeepaliveWindow: dur,
	}
}
