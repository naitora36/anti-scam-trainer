package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/marlendd/anti-scam-trainer/internal/feedback"

	"github.com/marlendd/anti-scam-trainer/internal/progress"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "config.yaml", "server configuration file")
	flag.Parse()

	cfg := config.MustLoad(configPath)
	log := mustMakeLogger(cfg.LogLevel)

	if err := run(&cfg, log); err != nil {
		log.Error("server stopped with error", "error", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config, log *slog.Logger) error {
	// ---------- progress ----------
	progressRepo := progress.NewPgRepository(db, log)
	progressService := progress.NewService(progressRepo, evaluator)
	progressHandler := progress.NewHandler(progressService, log)

	// ---------- feedback ----------
	openRouterKey := os.Getenv("OPENROUTER_API_KEY")
	if openRouterKey == "" {
		log.Warn("OPENROUTER_API_KEY is not set, feedback generation will fail")
	}

	llmProvider := feedback.NewOpenRouterLLMProvider(
		openRouterKey,
		"https://openrouter.ai/api/v1",
		"openai/gpt-oss-20b:free",
	)

	feedbackRepo := feedback.NewPgRepository(db, log)
	feedbackService := feedback.NewService(feedbackRepo, llmProvider, log)
	feedbackHandler := feedback.NewHandler(feedbackService, log)

	mux := http.NewServeMux()

	// progress not protected routes
	mux.HandleFunc("GET /api/v1/leaderboard", progressHandler.GetLeaderboard)
	// progress protected routes
	mux.Handle("GET /api/v1/profile/role-progress", requireAuth(http.HandlerFunc(progressHandler.GetMyRoleStats)))
	mux.Handle("GET /api/v1/profile/categories-progress", requireAuth(http.HandlerFunc(progressHandler.GetMyCategoryDashboard)))
	mux.Handle("GET /api/v1/profile/puzzle", requireAuth(http.HandlerFunc(progressHandler.GetMyPuzzleProgress)))
	mux.Handle("GET /api/v1/attempts/{id}/result", requireAuth(http.HandlerFunc(progressHandler.GetStatsOfAttempt)))
	mux.Handle("GET /api/v1/profile/rank-history", requireAuth(http.HandlerFunc(progressHandler.GetMyRankHistory)))
	mux.Handle("GET /api/v1/profile/summary", requireAuth(http.HandlerFunc(progressHandler.GetMySummary)))
	// feedback routes
	mux.Handle("GET /api/v1/attempts/{id}/feedback", requireAuth(http.HandlerFunc(feedbackHandler.GetAttemptFeedback)))
	addr := ":" + cfg.Port
	if cfg.Port == "" {
		addr = ":8080"
	}

	server := &http.Server{
		Addr:        addr,
		ReadTimeout: cfg.Timeout,
		Handler:     handler,
	}

	// ---------- graceful shutdown ----------
	serverErr := make(chan error, 1)
	go func() {
		log.Info("server run success", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("server closed unexpectedly: %w", err)
			return
		}
		serverErr <- nil
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return err
	case sig := <-stop:
		log.Info("shutdown signal received", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}

	log.Info("server stopped gracefully")
	return nil
}

func mustMakeLogger(logLevel string) *slog.Logger {
	var level slog.Level
	switch logLevel {
	case "DEBUG":
		level = slog.LevelDebug
	case "INFO":
		level = slog.LevelInfo
	case "ERROR":
		level = slog.LevelError
	default:
		panic("unknown log level: " + logLevel)
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})

	return slog.New(handler)
}
