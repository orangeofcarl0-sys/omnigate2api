package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
	"omnigate2api/internal/pool"
	"omnigate2api/internal/scheduler"
	"omnigate2api/internal/server"
	"omnigate2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)
	if len(auths) == 0 {
		log.Printf("WARNING: no accounts found. Run `omnigate2api login` first.")
	}

	if cfg.StateFile != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o700)
	}
	p, err := pool.New(auths, cfg.ToPoolConfig(), cfg.StateFile)
	if err != nil {
		log.Fatalf("build pool: %v", err)
	}

	c := upstream.New(120 * time.Second)

	sch := scheduler.New(scheduler.Config{
		Pool:              p,
		Enabled:           cfg.Watch.Enabled,
		Client:            c,
		PollInterval:      time.Duration(cfg.Watch.PollMinutes) * time.Minute,
		RefreshSkew:       time.Duration(cfg.Watch.RefreshSkewM) * time.Minute,
		KeepaliveInterval: time.Duration(cfg.Watch.KeepaliveInterval) * time.Minute,
	})

	h := server.NewHandler(server.Config{
		Pool:          p,
		Upstream:      c,
		APIKey:        cfg.APIKey,
		MaxRotate:     3,
		SoftCooldown:  cfg.SoftRateDur,
		ErrThreshold:  cfg.Cooldown.ErrThresh,
		ErrCooldown:   cfg.ErrCooldownDur,
		DefaultModel:  cfg.DefaultModel,
		ConvStateFile: cfg.StateFile + ".chats.json",
		WatchInfo: map[string]any{
			"enabled":              cfg.Watch.Enabled,
			"poll_minutes":         cfg.Watch.PollMinutes,
			"refresh_skew_minutes": cfg.Watch.RefreshSkewM,
			"keepalive_interval":   cfg.Watch.KeepaliveInterval,
			"max_concurrent":       cfg.MaxConcurrent,
			"keepalive_window":     "10m",
		},
		AuthDir:           cfg.AuthDir,
		DebugPromptDir:    os.Getenv("OMNIGATE_DEBUG_PROMPTS"),
		SessionMode:       os.Getenv("OMNIGATE_SESSION_MODE"),
		ToolchainOverride: os.Getenv("OMNIGATE_TOOLCHAIN"),
		Profiles:          buildProfiles(),
		Listen:            cfg.Listen,
		OAuthCallbackHost: cfg.OAuthCallbackHost,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("omnigate2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// buildProfiles 组装 Profile 注册表：内置 codearts + workbuddy + OMNIGATE_PROFILES_DIR 外部覆盖。
func buildProfiles() *adapt.Registry {
	r := adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)
	if dir := os.Getenv("OMNIGATE_PROFILES_DIR"); dir != "" {
		if err := r.LoadDir(dir); err != nil {
			log.Fatalf("profiles: %v", err)
		}
		log.Printf("profiles dir %s loaded: %v", dir, r.IDs())
	}
	return r
}
