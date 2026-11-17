package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/archive"
	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/brief"
	"photography-workflow/server/internal/changeorder"
	"photography-workflow/server/internal/client"
	"photography-workflow/server/internal/config"
	"photography-workflow/server/internal/contract"
	"photography-workflow/server/internal/customerportal"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/delivery"
	"photography-workflow/server/internal/device"
	"photography-workflow/server/internal/esign"
	"photography-workflow/server/internal/gallery"
	"photography-workflow/server/internal/httpx"
	"photography-workflow/server/internal/idempotency"
	"photography-workflow/server/internal/ingest"
	"photography-workflow/server/internal/integration"
	"photography-workflow/server/internal/invoice"
	"photography-workflow/server/internal/lead"
	"photography-workflow/server/internal/notice"
	"photography-workflow/server/internal/notify"
	"photography-workflow/server/internal/payment"
	"photography-workflow/server/internal/prep"
	"photography-workflow/server/internal/project"
	"photography-workflow/server/internal/proof"
	"photography-workflow/server/internal/quote"
	"photography-workflow/server/internal/schedule"
	"photography-workflow/server/internal/signing"
	"photography-workflow/server/internal/storage"
	"photography-workflow/server/internal/workflow"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.Database.URL, cfg.Database.MaxConns, cfg.Database.MinConns)
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
	if err := store.EnsureBucket(ctx); err != nil {
		return fmt.Errorf("ensure storage bucket: %w", err)
	}

	tokens := auth.NewTokens(cfg.Auth.JWTSecret, cfg.Auth.AccessTTL)
	authSvc := auth.NewService(pool, tokens, cfg.Auth.RefreshTTL, log)
	projectSvc := project.NewService(pool, log)
	ingestSvc := ingest.NewService(pool, store, log, ingest.Config{
		PresignTTL:    cfg.Gallery.PresignTTL,
		WatermarkText: cfg.Gallery.WatermarkText,
	})
	gallerySvc := gallery.NewService(pool, store, gallery.Config{
		DefaultWatermark: cfg.Gallery.WatermarkText,
		PublicBaseURL:    cfg.Gallery.PublicBaseURL,
		MaxAssets:        cfg.Gallery.MaxAssets,
		PresignTTL:       cfg.Gallery.PresignTTL,
	}, log)
	galleryTokens := auth.NewGalleryTokens(cfg.Auth.JWTSecret)
	galleryCustomerSvc := gallery.NewCustomerService(pool, store, galleryTokens, gallery.CustomerConfig{
		SessionTTL: cfg.Gallery.SessionTTL,
		PresignTTL: cfg.Gallery.PresignTTL,
		BatchLimit: cfg.Gallery.SelectionBatch,
	})
	proofTokens := auth.NewProofTokens(cfg.Auth.JWTSecret)
	proofSvc := proof.NewService(pool, store, proofTokens, proof.Config{
		PublicBaseURL:      cfg.Gallery.PublicBaseURL,
		PresignTTL:         cfg.Gallery.PresignTTL,
		SessionTTL:         cfg.Proof.SessionTTL,
		TokenGrace:         cfg.Proof.TokenGrace,
		MaxAssets:          cfg.Proof.MaxAssets,
		AnnotationMaxBytes: cfg.Proof.AnnotationMaxBytes,
		RetouchProvider:    cfg.Integrations.Provider,
		DefaultWatermark:   cfg.Gallery.WatermarkText,
	}, log)
	proofCustomerSvc := proof.NewCustomerService(proofSvc, proofTokens, proof.CustomerConfig{
		SessionTTL: cfg.Proof.SessionTTL,
		PresignTTL: cfg.Gallery.PresignTTL,
		PageLimit:  40,
	}, log)
	proofSvc.SetNotifier(notify.NewProofNotifier(notify.ProofNotifierConfig{
		PublicBaseURL: cfg.Gallery.PublicBaseURL,
		EmailEnabled:  cfg.Notify.Email.Enabled,
		Timezone:      cfg.Schedule.Timezone,
	}, log))
	smsProvider, err := notify.NewSMSProvider(cfg.Notify.SMS.Provider, log)
	if err != nil {
		return fmt.Errorf("build sms provider: %w", err)
	}
	if smsNotifier := notify.NewSMSNotifier(smsProvider, cfg.Gallery.PublicBaseURL); smsNotifier != nil {
		proofSvc.SetSMS(smsNotifier)
	}
	deviceSvc := device.NewService(pool, cfg.Agent.MinVersion, log)
	clientSvc := client.NewService(pool, log)
	leadSvc := lead.NewService(pool, log)
	briefSvc := brief.NewService(pool, log)
	quoteSvc := quote.NewService(pool, log)
	quoteSvc.SetNotifier(notify.NewQuoteNotifier(notify.QuoteNotifierConfig{
		EmailEnabled: cfg.Notify.Email.Enabled,
		ClientURL:    cfg.Customer.PublicBaseURL,
	}, log))

	alertSvc := alert.NewService(pool, alert.Config{
		WebhookURL:     cfg.Alerts.WebhookURL,
		WebhookSecret:  cfg.Alerts.WebhookSecret,
		WebhookTimeout: cfg.Alerts.WebhookTimeout,
	}, log)

	deliveryTokens := auth.NewDeliveryTokens(cfg.Auth.JWTSecret)
	deliverySvc := delivery.NewService(pool, store, deliveryTokens, delivery.Config{
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
	}, log)
	deliverySvc.SetNotifier(notify.NewDeliveryNotifier(notify.DeliveryNotifierConfig{
		EmailEnabled: cfg.Notify.Email.Enabled,
		Timezone:     cfg.Schedule.Timezone,
	}, log))
	deliverySvc.SetAlerts(alertSvc)

	archiveSvc := archive.NewService(pool, store, archive.Config{
		SweepInterval:   cfg.Archive.SweepInterval,
		DefaultKeepDays: cfg.Archive.DefaultKeepDays,
		DefaultWarnDays: cfg.Archive.DefaultWarnDays,
		Timezone:        cfg.Schedule.Timezone,
	}, log)
	archiveSvc.SetAlerts(alertSvc)

	// A stub provider must fail at startup, never silently accept retouch work.
	retouchProvider, err := integration.New(integration.Config{
		Provider: cfg.Integrations.Provider,
		Root:     cfg.Integrations.FolderWatch.Root,
	}, store)
	if err != nil {
		return fmt.Errorf("build retouch provider: %w", err)
	}
	proofSvc.SetExporter(integration.NewDispatcher(retouchProvider.Name()))
	integrationSvc := integration.NewService(pool, retouchProvider, proofSvc, alertSvc, integration.ServiceConfig{
		OverdueAfter: cfg.Integrations.FolderWatch.OverdueAfter,
	}, log)

	esignProvider, err := esign.New(cfg.ESign.Provider, cfg.ESign.CallbackSecret,
		cfg.ESign.SignBaseURL, cfg.ESign.CallbackTolerance)
	if err != nil {
		return fmt.Errorf("build esign provider: %w", err)
	}
	contractSvc := contract.NewService(pool, esignProvider, log)
	contractSvc.SetAlerts(alertSvc)

	paymentProvider, err := payment.New(cfg.Payment.Provider, cfg.Payment.CallbackSecret,
		cfg.Payment.MerchantID, cfg.Payment.PayBaseURL, cfg.Payment.CallbackTolerance)
	if err != nil {
		return fmt.Errorf("build payment provider: %w", err)
	}
	paymentSvc := payment.NewService(pool, paymentProvider, cfg.Payment.OrderTTL, log)
	gallerySvc.SetPayments(paymentSvc)
	paymentSvc.SetAlerts(alertSvc)

	invoiceProvider, err := invoice.NewProvider(cfg.Invoice.Provider)
	if err != nil {
		return fmt.Errorf("build invoice provider: %w", err)
	}
	invoiceSvc := invoice.NewService(pool, store, invoiceProvider, alertSvc, log)
	changeOrderSvc := changeorder.NewService(pool, esignProvider, paymentSvc, alertSvc, log)
	noticeSvc := notice.NewService(pool, notice.Config{
		Enabled:        cfg.Reminders.Enabled,
		ShootLeads:     cfg.Reminders.ShootLeads,
		AuthLead:       cfg.Reminders.AuthLead,
		HoldLead:       cfg.Reminders.HoldLead,
		Timezone:       cfg.Schedule.Timezone,
		WebhookURL:     cfg.Reminders.WebhookURL,
		WebhookSecret:  cfg.Reminders.WebhookSecret,
		WebhookTimeout: cfg.Reminders.WebhookTimeout,
	}, log)
	scheduleSvc := schedule.NewService(pool, cfg.Schedule.Timezone, cfg.Schedule.HoldTTL, alertSvc, noticeSvc, log)
	workflowSvc := workflow.NewService(pool, store, log, cfg.Schedule.Timezone)
	prepSvc := prep.NewService(pool, cfg.Schedule.Timezone, noticeSvc, log)
	paymentSvc.SetHooks(invoiceSvc, changeOrderSvc, scheduleSvc, gallerySvc)

	authHandler := auth.NewHandler(authSvc, log)
	projectHandler := project.NewHandler(projectSvc, log)
	ingestHandler := ingest.NewHandler(ingestSvc, log)
	galleryHandler := gallery.NewHandler(gallerySvc, log)
	galleryCustomerHandler := gallery.NewCustomerHandler(galleryCustomerSvc, galleryTokens, pool, log)
	galleryCustomerHandler.SetPayments(paymentSvc)
	proofCustomerHandler := proof.NewCustomerHandler(proofCustomerSvc, proofTokens, log)
	proofHandler := proof.NewHandler(proofSvc, log)
	deliveryHandler := delivery.NewHandler(deliverySvc, log)
	archiveHandler := archive.NewHandler(archiveSvc, log)
	deliveryCustomerSvc := delivery.NewCustomerService(pool, store, deliveryTokens, delivery.CustomerConfig{
		SessionTTL: cfg.Delivery.SessionTTL,
		PresignTTL: cfg.Delivery.PresignTTL,
		PageLimit:  40,
	}, log)
	deliveryCustomerHandler := delivery.NewCustomerHandler(deliveryCustomerSvc, deliveryTokens, log)
	customerPortalHandler := customerportal.NewHandler(pool, cfg.Auth.JWTSecret, customerportal.Config{
		AppID: cfg.Customer.WechatAppID, AppSecret: cfg.Customer.WechatAppSecret, DevLogin: cfg.Customer.DevLogin,
		AccessTTL: cfg.Customer.AccessTTL, RefreshTTL: cfg.Customer.RefreshTTL,
		Timezone: cfg.Schedule.Timezone, HoldTTL: cfg.Schedule.HoldTTL, PublicBase: cfg.Customer.PublicBaseURL,
	}, log)
	customerPortalHandler.SetQuoteService(quoteSvc)
	customerPortalHandler.SetPaymentService(paymentSvc)
	integrationHandler := integration.NewHandler(integrationSvc, log)
	deviceHandler := device.NewHandler(deviceSvc, log)
	clientHandler := client.NewHandler(clientSvc, log)
	leadHandler := lead.NewHandler(leadSvc, log)
	publicLeadTenant := uuid.Nil
	if cfg.PublicLeads.TenantID != "" {
		var parseErr error
		publicLeadTenant, parseErr = uuid.Parse(cfg.PublicLeads.TenantID)
		if parseErr != nil {
			return fmt.Errorf("public_leads.tenant_id: %w", parseErr)
		}
	}
	publicLeadHandler := lead.NewPublicHandler(leadSvc, publicLeadTenant, cfg.PublicLeads.Origins)
	briefHandler := brief.NewHandler(briefSvc, log)
	quoteHandler := quote.NewHandler(quoteSvc, log)
	contractHandler := contract.NewHandler(contractSvc, log)
	paymentHandler := payment.NewHandler(paymentSvc, paymentProvider, log)
	alertHandler := alert.NewHandler(alertSvc, log)
	invoiceHandler := invoice.NewHandler(invoiceSvc, log)
	changeOrderHandler := changeorder.NewHandler(changeOrderSvc, log)
	scheduleHandler := schedule.NewHandler(scheduleSvc, log)
	prepHandler := prep.NewHandler(prepSvc, log)
	noticeHandler := notice.NewHandler(noticeSvc, log)
	workflowHandler := workflow.NewHandler(workflowSvc, log)
	signingHandler := signing.NewHandler(pool, esignProvider, contractSvc, changeOrderSvc, log)

	r := chi.NewRouter()
	metrics := httpx.DefaultMetrics()
	r.Use(httpx.Common(log, cfg.HTTP.RequestTimeout))
	r.Use(httpx.CORS(cfg.HTTP.CORSOrigins))
	r.Use(metrics.Middleware())
	r.Get("/healthz", healthHandler(pool, store))
	r.Handle("/metrics", metrics)
	signingHandler.Register(r)
	paymentHandler.RegisterWebhook(r)
	customerAPI := chi.NewRouter()
	galleryCustomerHandler.RegisterRoutes(customerAPI)
	proofCustomerHandler.RegisterRoutes(customerAPI)
	deliveryCustomerHandler.RegisterRoutes(customerAPI)
	r.Mount("/g/v1", customerAPI)
	clientAPI := chi.NewRouter()
	customerPortalHandler.Register(clientAPI)
	r.Mount("/c/v1", clientAPI)
	r.Route("/api/v1", func(r chi.Router) {
		authHandler.Register(r)
		publicLeadHandler.Register(r)
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth(tokens))
			r.Use(idempotency.Middleware(pool, log))
			projectHandler.Register(r)
			clientHandler.Register(r)
			leadHandler.Register(r)
			briefHandler.Register(r)
			quoteHandler.Register(r)
			contractHandler.Register(r)
			paymentHandler.Register(r)
			alertHandler.Register(r)
			invoiceHandler.Register(r)
			changeOrderHandler.Register(r)
			scheduleHandler.Register(r)
			prepHandler.Register(r)
			noticeHandler.Register(r)
			workflowHandler.Register(r)
			ingestHandler.RegisterAdmin(r)
			galleryHandler.Register(r)
			proofHandler.Register(r)
			deliveryHandler.Register(r)
			archiveHandler.Register(r)
			integrationHandler.Register(r)
		})
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth(tokens))
		r.Use(idempotency.Middleware(pool, log))
		ingestHandler.RegisterAgent(r)
		deviceHandler.RegisterAgent(r)
	})

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.HTTP.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	log.Info("shutting down")
	return srv.Shutdown(shutdownCtx)
}

func healthHandler(pool *pgxpool.Pool, store storage.Storage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		status := http.StatusOK
		dbState := "ok"
		storeState := "ok"
		if err := pool.Ping(ctx); err != nil {
			dbState = err.Error()
			status = http.StatusServiceUnavailable
		}
		if err := store.Health(ctx); err != nil {
			storeState = err.Error()
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  http.StatusText(status),
			"db":      dbState,
			"storage": storeState,
		})
	}
}
