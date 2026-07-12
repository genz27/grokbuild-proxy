package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GreyGunG/grokbuild-proxy/internal/admin"
	"github.com/GreyGunG/grokbuild-proxy/internal/anthropic"
	"github.com/GreyGunG/grokbuild-proxy/internal/auth"
	"github.com/GreyGunG/grokbuild-proxy/internal/config"
	"github.com/GreyGunG/grokbuild-proxy/internal/httpserver"
	"github.com/GreyGunG/grokbuild-proxy/internal/lb"
	"github.com/GreyGunG/grokbuild-proxy/internal/openai"
	"github.com/GreyGunG/grokbuild-proxy/internal/proxy"
	"github.com/GreyGunG/grokbuild-proxy/internal/storage"
	"github.com/GreyGunG/grokbuild-proxy/internal/upstream"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	logger := newLogger("info")
	slog.SetDefault(logger)
	configPath := flag.String("config", "", "path to config.yaml (defaults to config.yaml/config.example.yaml when present)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	path := strings.TrimSpace(*configPath)
	if path == "" {
		path = defaultConfigPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		fail(logger, "config_invalid", err)
	}
	logger = newLogger(cfg.Logging.Level)
	slog.SetDefault(logger)
	if value := strings.TrimSpace(os.Getenv("LISTEN")); value != "" {
		cfg.Listen = value
	}
	if envTrue("ALLOW_PUBLIC_LISTEN") {
		cfg.AllowPublicListen = true
	}
	if err := cfg.ValidateListen(cfg.Listen); err != nil {
		fail(logger, "listen_invalid", err)
	}

	store, err := storage.New(cfg.DataDir)
	if err != nil {
		fail(logger, "storage_open_failed", err)
	}
	defer store.Close()

	apiKey, adminKey, genAPI, genAdmin, err := store.EnsureBootstrapKeys(cfg.APIKey, cfg.AdminKey)
	if err != nil {
		fail(logger, "bootstrap_keys_failed", err)
	}
	if genAPI || genAdmin {
		logger.Info("bootstrap_keys_generated", "path", cfg.DataDir+"/meta.json")
	}
	if !genAPI && !genAdmin && (strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.AdminKey) == "") {
		logger.Info("bootstrap_keys_loaded", "path", cfg.DataDir+"/meta.json")
	}
	if apiKey != "" && adminKey != "" && apiKey == adminKey {
		fail(logger, "bootstrap_keys_invalid", fmt.Errorf("api_key and admin_key must differ"))
	}
	cfg.APIKey = apiKey
	cfg.AdminKey = adminKey

	oauth := &auth.OAuthClient{
		HTTPClient: &http.Client{Timeout: cfg.RequestTimeout()},
		Issuer:     cfg.OAuth.Issuer,
		ClientID:   cfg.OAuth.ClientID,
		Scope:      cfg.OAuth.Scope,
	}
	refresher := &auth.Refresher{
		OAuth:   oauth,
		Skew:    cfg.RefreshSkew(),
		Timeout: cfg.RequestTimeout(),
	}

	up := upstream.NewClient(upstream.Config{
		BaseURL:          cfg.Upstream.BaseURL,
		ClientVersion:    cfg.Upstream.ClientVersion,
		ClientIdentifier: cfg.Upstream.ClientIdentifier,
		TokenAuth:        cfg.Upstream.TokenAuth,
		UserAgent:        cfg.Upstream.UserAgent,
		RequestTimeout:   cfg.RequestTimeout(),
	})

	healthQ := storage.NewHealthQueue(store, 2*time.Second)
	healthQ.Start()
	defer healthQ.Stop()
	selector := lb.New(cfg.LB).SetHealthStore(healthQ)

	exec := &proxy.Executor{
		Store:                        store,
		Selector:                     selector,
		Upstream:                     up,
		Refresher:                    refresher,
		UsageQueue:                   healthQ,
		Logger:                       logger,
		RequestID:                    httpserver.RequestIDFromContext,
		MaxAttempts:                  cfg.MaxAttempts(),
		FreeUsageExhaustedCooldown:   cfg.FreeUsageExhaustedCooldown(),
	}
	var prefetcher *proxy.Prefetcher
	if cfg.PrefetchEnabled() {
		prefetcher = &proxy.Prefetcher{
			Store:       store,
			Executor:    exec,
			Interval:    cfg.PrefetchInterval(),
			Skew:        cfg.RefreshSkew(),
			MaxPerTick:  cfg.PrefetchMaxPerTick(),
			Concurrency: cfg.PrefetchConcurrency(),
			Logger:      logger,
		}
		prefetcher.Start()
		defer prefetcher.Stop()
		logger.Info("prefetch_enabled",
			"interval_sec", int(cfg.PrefetchInterval().Seconds()),
			"max_per_tick", cfg.PrefetchMaxPerTick(),
			"concurrency", cfg.PrefetchConcurrency(),
			"soft_demote_on_429", cfg.SoftDemoteOn429Enabled(),
		)
	} else {
		logger.Info("prefetch_disabled")
	}

	oai := &openai.Handlers{
		Post:    exec.Post,
		MaxBody: cfg.Limits.MaxBodyBytes,
	}
	anth := &anthropic.Handlers{
		Post:    exec.Post,
		Cfg:     cfg.Anthropic,
		MaxBody: cfg.Limits.MaxBodyBytes,
		ResolveModel: func(m string) string {
			return cfg.ResolveModel(m)
		},
	}

	adm := &admin.Handlers{
		Store:    store,
		Tokens:   exec,
		OAuth:    oauth,
		Config:   cfg,
		AdminKey: adminKey,
		Version:  version,
		MaxBody:  cfg.Limits.MaxBodyBytes,
		Metrics: func() any {
			return map[string]any{
				"path": exec.PathSnapshot(),
			}
		},
	}

	handler := httpserver.New(httpserver.Options{
		Config:    cfg,
		AdminKey:  adminKey,
		Store:     store,
		OpenAI:    oai,
		Anthropic: anth,
		Admin:     adm,
		ModelList: exec,
		Version:   version,
		Logger:    logger,
	})

	addr := cfg.Listen
	srv := httpserver.NewServer(addr, handler, cfg.RequestTimeout())

	go func() {
		logger.Info("server_listening", "version", version, "address", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server_failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	sig := <-stop
	logger.Info("shutdown_signal", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown_failed", "error", err)
	}
}

func fail(logger *slog.Logger, event string, err error) {
	logger.Error(event, "error", err)
	os.Exit(1)
}

func newLogger(level string) *slog.Logger {
	var selected slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		selected = slog.LevelDebug
	case "warn", "warning":
		selected = slog.LevelWarn
	case "error":
		selected = slog.LevelError
	default:
		selected = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: selected}))
}

func defaultConfigPath() string {
	candidates := []string{"config.yaml", "config.example.yaml"}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
