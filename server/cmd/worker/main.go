package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"photography-workflow/server/internal/archive"
	"photography-workflow/server/internal/config"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/delivery"
	"photography-workflow/server/internal/integration"
	"photography-workflow/server/internal/media"
	"photography-workflow/server/internal/notice"
	"photography-workflow/server/internal/notify"
	"photography-workflow/server/internal/payment"
	"photography-workflow/server/internal/storage"
	"photography-workflow/server/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "config/config.yaml", "path to config file")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.CheckProduction(); err != nil {
		return err
	}
	if cfg.Database.WorkerURL == "" {
		return fmt.Errorf("database.worker_url is required for the worker")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The worker uses a dedicated least-privilege role, never the migration owner.
	pool, err := db.Open(ctx, cfg.Database.WorkerURL, cfg.Database.MaxConns, cfg.Database.MinConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	store, err := storage.NewMinIO(storage.MinIOOptions{
		Endpoint:      cfg.Storage.Endpoint,
		AccessKey:     cfg.Storage.AccessKey,
		SecretKey:     cfg.Storage.SecretKey,
		Bucket:        cfg.Storage.Bucket,
		Region:        cfg.Storage.Region,
		UseSSL:        cfg.Storage.UseSSL,
		PublicBaseURL: cfg.Storage.PublicBaseURL,
	})
	if err != nil {
		return err
	}

	// The sweeper needs the same provider the API uses, so an abandoned
	// reservation is closed against the real channel state.
	paymentProvider, err := payment.New(cfg.Payment.Provider, cfg.Payment.CallbackSecret,
		cfg.Payment.MerchantID, cfg.Payment.PayBaseURL, cfg.Payment.CallbackTolerance)
	if err != nil {
		return fmt.Errorf("build payment provider: %w", err)
	}
	// A stub retouch provider must stop the worker at startup, not at the first
	// retouch job.
	retouchProvider, err := integration.New(integration.Config{
		Provider: cfg.Integrations.Provider,
		Root:     cfg.Integrations.FolderWatch.Root,
	}, store)
	if err != nil {
		return fmt.Errorf("build retouch provider: %w", err)
	}

	w, err := worker.New(pool, store, log, worker.Options{
		ID:                   cfg.Worker.ID,
		PollInterval:         cfg.Worker.PollInterval,
		BatchSize:            cfg.Worker.BatchSize,
		PaymentProvider:      paymentProvider,
		PaymentSweepInterval: time.Minute,
		AlertsWebhookURL:     cfg.Alerts.WebhookURL,
		AlertsWebhookSecret:  cfg.Alerts.WebhookSecret,
		AlertsWebhookTimeout: cfg.Alerts.WebhookTimeout,
		Media: media.Config{
			WatermarkText: cfg.Gallery.WatermarkText,
			WatermarkFont: cfg.Gallery.WatermarkFont,
		},
		GalleryReminderLead: cfg.Gallery.ReminderLead,
		RetouchProvider:     retouchProvider,
		Integration: integration.ServiceConfig{
			OverdueAfter: cfg.Integrations.FolderWatch.OverdueAfter,
		},
		IntegrationInterval: cfg.Integrations.FolderWatch.PollInterval,
		Delivery: delivery.Config{
			PublicBaseURL:   cfg.Delivery.PublicBaseURL,
			SessionTTL:      cfg.Delivery.SessionTTL,
			DefaultTokenTTL: cfg.Delivery.DefaultTokenTTL,
			MaxTokenTTL:     cfg.Delivery.MaxTokenTTL,
			PresignTTL:      cfg.Delivery.PresignTTL,
			MaxDownloads:    cfg.Delivery.MaxDownloads,
			MaxAssets:       cfg.Delivery.MaxAssets,
			TempDir:         cfg.Delivery.TempDir,
			WebEdge:         cfg.Delivery.WebEdge,
			RequireWebSpec:  cfg.Delivery.RequireWebSpec,
		},
		Archive: archive.Config{
			SweepInterval:   cfg.Archive.SweepInterval,
			DefaultKeepDays: cfg.Archive.DefaultKeepDays,
			DefaultWarnDays: cfg.Archive.DefaultWarnDays,
			Timezone:        cfg.Schedule.Timezone,
		},
		Email: notify.EmailConfig{
			Enabled:  cfg.Notify.Email.Enabled,
			Host:     cfg.Notify.Email.Host,
			Port:     cfg.Notify.Email.Port,
			Username: cfg.Notify.Email.Username,
			Password: cfg.Notify.Email.Password,
			From:     cfg.Notify.Email.From,
			TLS:      cfg.Notify.Email.TLS,
			Timeout:  cfg.Notify.Email.Timeout,
		},
		PublicBaseURL: cfg.Gallery.PublicBaseURL,
		Timezone:      cfg.Schedule.Timezone,
		Reminders: notice.Config{
			Enabled:        cfg.Reminders.Enabled,
			ShootLeads:     cfg.Reminders.ShootLeads,
			AuthLead:       cfg.Reminders.AuthLead,
			HoldLead:       cfg.Reminders.HoldLead,
			Timezone:       cfg.Schedule.Timezone,
			WebhookURL:     cfg.Reminders.WebhookURL,
			WebhookSecret:  cfg.Reminders.WebhookSecret,
			WebhookTimeout: cfg.Reminders.WebhookTimeout,
		},
	})
	if err != nil {
		return err
	}
	log.Info("worker started", "id", cfg.Worker.ID, "batch_size", cfg.Worker.BatchSize,
		"poll_interval", cfg.Worker.PollInterval.String())
	return w.Run(ctx)
}
