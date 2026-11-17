package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

type Config struct {
	Env          string
	HTTP         HTTP
	Database     Database
	Auth         Auth
	Storage      Storage
	Worker       Worker
	ESign        ESign
	Payment      Payment
	Alerts       Alerts
	Invoice      Invoice
	Schedule     Schedule
	Reminders    Reminders
	Agent        Agent
	Gallery      Gallery
	Proof        Proof
	Integrations Integrations
	Notify       Notify
	Delivery     Delivery
	Archive      Archive
	PublicLeads  PublicLeads
	Customer     Customer
}

type Customer struct {
	WechatAppID     string
	WechatAppSecret string
	DevLogin        bool
	AccessTTL       time.Duration
	RefreshTTL      time.Duration
	PublicBaseURL   string
}

type PublicLeads struct {
	TenantID string
	Origins  []string
}

type HTTP struct {
	Addr           string
	RequestTimeout time.Duration
	CORSOrigins    []string
}

type Database struct {
	URL        string
	MigrateURL string
	WorkerURL  string
	MaxConns   int32
	MinConns   int32
}

type Auth struct {
	JWTSecret  string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

type Storage struct {
	Driver        string
	Endpoint      string
	AccessKey     string
	SecretKey     string
	Bucket        string
	UseSSL        bool
	Region        string
	PublicBaseURL string
}

type Worker struct {
	ID           string
	PollInterval time.Duration
	BatchSize    int
}

type ESign struct {
	Provider          string
	CallbackSecret    string
	SignBaseURL       string
	CallbackTolerance time.Duration
}

type Payment struct {
	Provider          string
	CallbackSecret    string
	MerchantID        string
	PayBaseURL        string
	CallbackTolerance time.Duration
	OrderTTL          time.Duration
}

// Alerts configures the security alert webhook. An empty URL disables delivery
// but keeps alerts in the database.
type Alerts struct {
	WebhookURL     string
	WebhookSecret  string
	WebhookTimeout time.Duration
}

// Invoice selects the invoicing adapter. "manual" records numbers issued in the
// tax system; production use requires an adapter that talks to the vendor.
type Invoice struct {
	Provider string
}

// Schedule interprets availability rules and hold expiry. Timezone applies to
// every tenant; multi-region scheduling needs a per-tenant zone.
type Schedule struct {
	Timezone *time.Location
	HoldTTL  time.Duration
}

// Reminders configures in-app shoot reminders and their signed webhook. Leads
// are measured before the shoot; a zero auth or hold lead disables that type.
type Reminders struct {
	Enabled        bool
	ShootLeads     []time.Duration
	AuthLead       time.Duration
	HoldLead       time.Duration
	WebhookURL     string
	WebhookSecret  string
	WebhookTimeout time.Duration
}

// Agent configures the Windows capture agent contract. MinVersion is echoed in
// heartbeats so an outdated device prompts for an upgrade; empty disables the
// check.
type Agent struct {
	MinVersion string
}

// Gallery configures customer selection galleries. Preview URLs are signed for
// PresignTTL; a customer session lasts SessionTTL but never outlives its share
// token. WatermarkText defaults to the Lumeo brand mark; WatermarkFont must be a
// font file that covers the watermark characters (production requires one).
type Gallery struct {
	PresignTTL     time.Duration
	SessionTTL     time.Duration
	SelectionBatch int
	MaxAssets      int
	WatermarkText  string
	WatermarkFont  string
	ReminderLead   time.Duration
	PublicBaseURL  string
}

// Proof configures customer proof sessions. A session never outlives its round
// token; tokens default to DueAt+TokenGrace so a round that runs late does not
// strand the customer.
type Proof struct {
	SessionTTL         time.Duration
	TokenGrace         time.Duration
	MaxAssets          int
	AnnotationMaxBytes int
}

// Integrations selects the retouch provider. manual keeps uploads in the admin
// app; folder_watch trades files through shared directories. Providers that are
// only interface stubs fail at startup instead of silently doing nothing.
type Integrations struct {
	Provider    string
	FolderWatch FolderWatch
}

type FolderWatch struct {
	Root         string
	PollInterval time.Duration
	OverdueAfter time.Duration
}

// Notify carries outbound customer channels. Email is the only real delivery
// path; SMS is an interface plus a sandbox recorder.
type Notify struct {
	Email Email
	SMS   SMS
}

type Email struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLS      string
	Timeout  time.Duration
}

type SMS struct {
	Provider string
}

// Delivery configures customer delivery packages. Links live in the customer
// app fragment; sessions are short-lived and never outlive their download
// token. PublicBaseURL falls back to gallery.public_base_url.
type Delivery struct {
	PublicBaseURL   string
	SessionTTL      time.Duration
	DefaultTokenTTL time.Duration
	MaxTokenTTL     time.Duration
	PresignTTL      time.Duration
	MaxDownloads    int
	MaxAssets       int
	TempDir         string
	WebEdge         int
	RequireWebSpec  bool
}

// Archive configures object verification and the tenant retention policy.
// Deletion is never automatic; the values only drive the retention warning.
type Archive struct {
	SweepInterval   time.Duration
	DefaultKeepDays int
	DefaultWarnDays int
}

type raw struct {
	Env  string `koanf:"env"`
	HTTP struct {
		Addr           string `koanf:"addr"`
		RequestTimeout string `koanf:"request_timeout"`
		CORSOrigins    string `koanf:"cors_origins"`
	} `koanf:"http"`
	Database struct {
		URL        string `koanf:"url"`
		MigrateURL string `koanf:"migrate_url"`
		WorkerURL  string `koanf:"worker_url"`
		MaxConns   int32  `koanf:"max_conns"`
		MinConns   int32  `koanf:"min_conns"`
	} `koanf:"database"`
	Auth struct {
		JWTSecret  string `koanf:"jwt_secret"`
		AccessTTL  string `koanf:"access_ttl"`
		RefreshTTL string `koanf:"refresh_ttl"`
	} `koanf:"auth"`
	Storage struct {
		Driver        string `koanf:"driver"`
		Endpoint      string `koanf:"endpoint"`
		AccessKey     string `koanf:"access_key"`
		SecretKey     string `koanf:"secret_key"`
		Bucket        string `koanf:"bucket"`
		UseSSL        bool   `koanf:"use_ssl"`
		Region        string `koanf:"region"`
		PublicBaseURL string `koanf:"public_base_url"`
	} `koanf:"storage"`
	Worker struct {
		ID           string `koanf:"id"`
		PollInterval string `koanf:"poll_interval"`
		BatchSize    int    `koanf:"batch_size"`
	} `koanf:"worker"`
	ESign struct {
		Provider          string `koanf:"provider"`
		CallbackSecret    string `koanf:"callback_secret"`
		SignBaseURL       string `koanf:"sign_base_url"`
		CallbackTolerance string `koanf:"callback_tolerance"`
	} `koanf:"esign"`
	Payment struct {
		Provider          string `koanf:"provider"`
		CallbackSecret    string `koanf:"callback_secret"`
		MerchantID        string `koanf:"merchant_id"`
		PayBaseURL        string `koanf:"pay_base_url"`
		CallbackTolerance string `koanf:"callback_tolerance"`
		OrderTTL          string `koanf:"order_ttl"`
	} `koanf:"payment"`
	Alerts struct {
		WebhookURL     string `koanf:"webhook_url"`
		WebhookSecret  string `koanf:"webhook_secret"`
		WebhookTimeout string `koanf:"webhook_timeout"`
	} `koanf:"alerts"`
	Invoice struct {
		Provider string `koanf:"provider"`
	} `koanf:"invoice"`
	Schedule struct {
		Timezone string `koanf:"timezone"`
		HoldTTL  string `koanf:"hold_ttl"`
	} `koanf:"schedule"`
	Reminders struct {
		Enabled        *bool  `koanf:"enabled"`
		ShootLeads     string `koanf:"shoot_leads"`
		AuthLead       string `koanf:"auth_lead"`
		HoldLead       string `koanf:"hold_lead"`
		WebhookURL     string `koanf:"webhook_url"`
		WebhookSecret  string `koanf:"webhook_secret"`
		WebhookTimeout string `koanf:"webhook_timeout"`
	} `koanf:"reminders"`
	Agent struct {
		MinVersion string `koanf:"min_version"`
	} `koanf:"agent"`
	Gallery struct {
		PresignTTL     string `koanf:"presign_ttl"`
		SessionTTL     string `koanf:"session_ttl"`
		SelectionBatch int    `koanf:"selection_batch"`
		MaxAssets      int    `koanf:"max_assets"`
		WatermarkText  string `koanf:"watermark_text"`
		WatermarkFont  string `koanf:"watermark_font"`
		ReminderLead   string `koanf:"reminder_lead"`
		PublicBaseURL  string `koanf:"public_base_url"`
	} `koanf:"gallery"`
	Proof struct {
		SessionTTL         string `koanf:"session_ttl"`
		TokenGrace         string `koanf:"token_grace"`
		MaxAssets          int    `koanf:"max_assets"`
		AnnotationMaxBytes int    `koanf:"annotation_max_bytes"`
	} `koanf:"proof"`
	Integrations struct {
		Provider    string `koanf:"provider"`
		FolderWatch struct {
			Root         string `koanf:"root"`
			PollInterval string `koanf:"poll_interval"`
			OverdueAfter string `koanf:"overdue_after"`
		} `koanf:"folder_watch"`
	} `koanf:"integrations"`
	Notify struct {
		Email struct {
			Enabled  *bool  `koanf:"enabled"`
			Host     string `koanf:"host"`
			Port     int    `koanf:"port"`
			Username string `koanf:"username"`
			Password string `koanf:"password"`
			From     string `koanf:"from"`
			TLS      string `koanf:"tls"`
			Timeout  string `koanf:"timeout"`
		} `koanf:"email"`
		SMS struct {
			Provider string `koanf:"provider"`
		} `koanf:"sms"`
	} `koanf:"notify"`
	Delivery struct {
		PublicBaseURL   string `koanf:"public_base_url"`
		SessionTTL      string `koanf:"session_ttl"`
		DefaultTokenTTL string `koanf:"default_token_ttl"`
		MaxTokenTTL     string `koanf:"max_token_ttl"`
		PresignTTL      string `koanf:"presign_ttl"`
		MaxDownloads    int    `koanf:"max_downloads"`
		MaxAssets       int    `koanf:"max_assets"`
		TempDir         string `koanf:"temp_dir"`
		WebEdge         int    `koanf:"web_edge"`
		RequireWebSpec  bool   `koanf:"require_web_spec"`
	} `koanf:"delivery"`
	Archive struct {
		SweepInterval   string `koanf:"sweep_interval"`
		DefaultKeepDays int    `koanf:"default_keep_days"`
		DefaultWarnDays int    `koanf:"default_warn_days"`
	} `koanf:"archive"`
	PublicLeads struct {
		TenantID string `koanf:"tenant_id"`
		Origins  string `koanf:"origins"`
	} `koanf:"public_leads"`
	Customer struct {
		WechatAppID     string `koanf:"wechat_app_id"`
		WechatAppSecret string `koanf:"wechat_app_secret"`
		DevLogin        bool   `koanf:"dev_login"`
		AccessTTL       string `koanf:"access_ttl"`
		RefreshTTL      string `koanf:"refresh_ttl"`
		PublicBaseURL   string `koanf:"public_base_url"`
	} `koanf:"customer"`
}

// Load reads config.yaml when present, then overlays PWF_* environment vars.
// PWF_DATABASE__URL maps to database.url.
func Load(path string) (*Config, error) {
	k := koanf.New(".")
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
				return nil, fmt.Errorf("load config %s: %w", path, err)
			}
		}
	}
	if err := k.Load(env.Provider("PWF_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "PWF_")), "__", ".")
	}), nil); err != nil {
		return nil, fmt.Errorf("load env: %w", err)
	}

	var r raw
	if err := k.Unmarshal("", &r); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	cfg := &Config{
		Env:  strings.ToLower(strings.TrimSpace(r.Env)),
		HTTP: HTTP{Addr: r.HTTP.Addr, CORSOrigins: splitConfigList(r.HTTP.CORSOrigins)},
		Database: Database{
			URL:        r.Database.URL,
			MigrateURL: r.Database.MigrateURL,
			WorkerURL:  r.Database.WorkerURL,
			MaxConns:   r.Database.MaxConns,
			MinConns:   r.Database.MinConns,
		},
		Auth: Auth{
			JWTSecret: r.Auth.JWTSecret,
		},
		Storage: Storage{
			Driver:        r.Storage.Driver,
			Endpoint:      r.Storage.Endpoint,
			AccessKey:     r.Storage.AccessKey,
			SecretKey:     r.Storage.SecretKey,
			Bucket:        r.Storage.Bucket,
			UseSSL:        r.Storage.UseSSL,
			Region:        r.Storage.Region,
			PublicBaseURL: r.Storage.PublicBaseURL,
		},
		Worker: Worker{
			ID:        r.Worker.ID,
			BatchSize: r.Worker.BatchSize,
		},
		ESign: ESign{
			Provider:       r.ESign.Provider,
			CallbackSecret: r.ESign.CallbackSecret,
			SignBaseURL:    r.ESign.SignBaseURL,
		},
		Payment: Payment{
			Provider:       r.Payment.Provider,
			CallbackSecret: r.Payment.CallbackSecret,
			MerchantID:     r.Payment.MerchantID,
			PayBaseURL:     r.Payment.PayBaseURL,
		},
		Alerts: Alerts{
			WebhookURL:    r.Alerts.WebhookURL,
			WebhookSecret: r.Alerts.WebhookSecret,
		},
		Invoice: Invoice{
			Provider: r.Invoice.Provider,
		},
		Schedule: Schedule{},
		Agent: Agent{
			MinVersion: strings.TrimSpace(r.Agent.MinVersion),
		},
		PublicLeads: PublicLeads{
			TenantID: strings.TrimSpace(r.PublicLeads.TenantID),
			Origins:  splitConfigList(r.PublicLeads.Origins),
		},
		Customer: Customer{
			WechatAppID:     strings.TrimSpace(r.Customer.WechatAppID),
			WechatAppSecret: strings.TrimSpace(r.Customer.WechatAppSecret),
			DevLogin:        r.Customer.DevLogin,
			PublicBaseURL:   strings.TrimRight(strings.TrimSpace(r.Customer.PublicBaseURL), "/"),
		},
	}

	var err error
	if cfg.Auth.AccessTTL, err = time.ParseDuration(r.Auth.AccessTTL); err != nil {
		return nil, fmt.Errorf("auth.access_ttl: %w", err)
	}
	if r.Customer.AccessTTL == "" {
		cfg.Customer.AccessTTL = 2 * time.Hour
	} else if cfg.Customer.AccessTTL, err = time.ParseDuration(r.Customer.AccessTTL); err != nil {
		return nil, fmt.Errorf("customer.access_ttl: %w", err)
	}
	if r.Customer.RefreshTTL == "" {
		cfg.Customer.RefreshTTL = 30 * 24 * time.Hour
	} else if cfg.Customer.RefreshTTL, err = time.ParseDuration(r.Customer.RefreshTTL); err != nil {
		return nil, fmt.Errorf("customer.refresh_ttl: %w", err)
	}
	if cfg.Customer.AccessTTL < 5*time.Minute || cfg.Customer.AccessTTL > 24*time.Hour {
		return nil, fmt.Errorf("customer.access_ttl must be between 5m and 24h")
	}
	if cfg.Customer.RefreshTTL < time.Hour || cfg.Customer.RefreshTTL > 365*24*time.Hour {
		return nil, fmt.Errorf("customer.refresh_ttl must be between 1h and 365d")
	}
	if cfg.Customer.DevLogin && cfg.Env == "production" {
		return nil, fmt.Errorf("customer.dev_login is not allowed in production")
	}
	if cfg.Auth.RefreshTTL, err = time.ParseDuration(r.Auth.RefreshTTL); err != nil {
		return nil, fmt.Errorf("auth.refresh_ttl: %w", err)
	}
	if r.HTTP.RequestTimeout == "" {
		cfg.HTTP.RequestTimeout = 15 * time.Second
	} else if cfg.HTTP.RequestTimeout, err = time.ParseDuration(r.HTTP.RequestTimeout); err != nil {
		return nil, fmt.Errorf("http.request_timeout: %w", err)
	}
	if r.Worker.PollInterval == "" {
		cfg.Worker.PollInterval = 2 * time.Second
	} else if cfg.Worker.PollInterval, err = time.ParseDuration(r.Worker.PollInterval); err != nil {
		return nil, fmt.Errorf("worker.poll_interval: %w", err)
	}
	if r.ESign.CallbackTolerance == "" {
		cfg.ESign.CallbackTolerance = 5 * time.Minute
	} else if cfg.ESign.CallbackTolerance, err = time.ParseDuration(r.ESign.CallbackTolerance); err != nil {
		return nil, fmt.Errorf("esign.callback_tolerance: %w", err)
	}
	if cfg.ESign.Provider == "" {
		cfg.ESign.Provider = "sandbox"
	}
	if r.Payment.CallbackTolerance == "" {
		cfg.Payment.CallbackTolerance = 5 * time.Minute
	} else if cfg.Payment.CallbackTolerance, err = time.ParseDuration(r.Payment.CallbackTolerance); err != nil {
		return nil, fmt.Errorf("payment.callback_tolerance: %w", err)
	}
	if r.Payment.OrderTTL == "" {
		cfg.Payment.OrderTTL = 30 * time.Minute
	} else if cfg.Payment.OrderTTL, err = time.ParseDuration(r.Payment.OrderTTL); err != nil {
		return nil, fmt.Errorf("payment.order_ttl: %w", err)
	}
	if cfg.Payment.Provider == "" {
		cfg.Payment.Provider = "sandbox"
	}
	if r.Alerts.WebhookTimeout == "" {
		cfg.Alerts.WebhookTimeout = 10 * time.Second
	} else if cfg.Alerts.WebhookTimeout, err = time.ParseDuration(r.Alerts.WebhookTimeout); err != nil {
		return nil, fmt.Errorf("alerts.webhook_timeout: %w", err)
	}
	if cfg.Alerts.WebhookURL != "" && len(cfg.Alerts.WebhookSecret) < 16 {
		return nil, fmt.Errorf("alerts.webhook_secret must be at least 16 characters when alerts.webhook_url is set")
	}
	if cfg.Invoice.Provider == "" {
		cfg.Invoice.Provider = "manual"
	}
	tz := strings.TrimSpace(r.Schedule.Timezone)
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	if cfg.Schedule.Timezone, err = time.LoadLocation(tz); err != nil {
		return nil, fmt.Errorf("schedule.timezone %q: %w", tz, err)
	}
	if r.Schedule.HoldTTL == "" {
		cfg.Schedule.HoldTTL = 30 * time.Minute
	} else if cfg.Schedule.HoldTTL, err = time.ParseDuration(r.Schedule.HoldTTL); err != nil {
		return nil, fmt.Errorf("schedule.hold_ttl: %w", err)
	}
	if cfg.Schedule.HoldTTL <= 0 {
		return nil, fmt.Errorf("schedule.hold_ttl must be positive")
	}
	cfg.Reminders.Enabled = true
	if r.Reminders.Enabled != nil {
		cfg.Reminders.Enabled = *r.Reminders.Enabled
	}
	cfg.Reminders.WebhookURL = strings.TrimSpace(r.Reminders.WebhookURL)
	cfg.Reminders.WebhookSecret = strings.TrimSpace(r.Reminders.WebhookSecret)
	leads := r.Reminders.ShootLeads
	if strings.TrimSpace(leads) == "" {
		leads = "24h,2h"
	}
	if cfg.Reminders.ShootLeads, err = parseLeads(leads); err != nil {
		return nil, fmt.Errorf("reminders.shoot_leads: %w", err)
	}
	if cfg.Reminders.AuthLead, err = parseOptionalLead(r.Reminders.AuthLead, 24*time.Hour); err != nil {
		return nil, fmt.Errorf("reminders.auth_lead: %w", err)
	}
	if cfg.Reminders.HoldLead, err = parseOptionalLead(r.Reminders.HoldLead, 10*time.Minute); err != nil {
		return nil, fmt.Errorf("reminders.hold_lead: %w", err)
	}
	if r.Reminders.WebhookTimeout == "" {
		cfg.Reminders.WebhookTimeout = 10 * time.Second
	} else if cfg.Reminders.WebhookTimeout, err = time.ParseDuration(r.Reminders.WebhookTimeout); err != nil {
		return nil, fmt.Errorf("reminders.webhook_timeout: %w", err)
	}
	if cfg.Reminders.WebhookTimeout <= 0 {
		return nil, fmt.Errorf("reminders.webhook_timeout must be positive")
	}
	if cfg.Reminders.WebhookURL != "" && len(cfg.Reminders.WebhookSecret) < 16 {
		return nil, fmt.Errorf("reminders.webhook_secret must be at least 16 characters when reminders.webhook_url is set")
	}
	if len(cfg.Agent.MinVersion) > 64 {
		return nil, fmt.Errorf("agent.min_version must be at most 64 characters")
	}
	cfg.Gallery = Gallery{
		PresignTTL:     15 * time.Minute,
		SessionTTL:     2 * time.Hour,
		SelectionBatch: 200,
		MaxAssets:      5000,
		ReminderLead:   24 * time.Hour,
		WatermarkText:  strings.TrimSpace(r.Gallery.WatermarkText),
		WatermarkFont:  strings.TrimSpace(r.Gallery.WatermarkFont),
		PublicBaseURL:  strings.TrimRight(strings.TrimSpace(r.Gallery.PublicBaseURL), "/"),
	}
	if cfg.Gallery.WatermarkText == "" {
		cfg.Gallery.WatermarkText = "Lumeo"
	}
	if r.Gallery.PresignTTL != "" {
		if cfg.Gallery.PresignTTL, err = time.ParseDuration(r.Gallery.PresignTTL); err != nil {
			return nil, fmt.Errorf("gallery.presign_ttl: %w", err)
		}
	}
	if r.Gallery.SessionTTL != "" {
		if cfg.Gallery.SessionTTL, err = time.ParseDuration(r.Gallery.SessionTTL); err != nil {
			return nil, fmt.Errorf("gallery.session_ttl: %w", err)
		}
	}
	if r.Gallery.ReminderLead != "" {
		if cfg.Gallery.ReminderLead, err = time.ParseDuration(r.Gallery.ReminderLead); err != nil {
			return nil, fmt.Errorf("gallery.reminder_lead: %w", err)
		}
	}
	if v := r.Gallery.SelectionBatch; v > 0 {
		cfg.Gallery.SelectionBatch = v
	}
	if v := r.Gallery.MaxAssets; v > 0 {
		cfg.Gallery.MaxAssets = v
	}
	if cfg.Gallery.PresignTTL < time.Minute || cfg.Gallery.PresignTTL > time.Hour {
		return nil, fmt.Errorf("gallery.presign_ttl must be between 1m and 1h")
	}
	if cfg.Gallery.SessionTTL < 5*time.Minute || cfg.Gallery.SessionTTL > 24*time.Hour {
		return nil, fmt.Errorf("gallery.session_ttl must be between 5m and 24h")
	}
	if cfg.Gallery.SelectionBatch > 500 {
		return nil, fmt.Errorf("gallery.selection_batch must be at most 500")
	}
	if cfg.Gallery.MaxAssets > 10000 {
		return nil, fmt.Errorf("gallery.max_assets must be at most 10000")
	}
	if cfg.Gallery.ReminderLead < 0 || cfg.Gallery.ReminderLead > 168*time.Hour {
		return nil, fmt.Errorf("gallery.reminder_lead must be between 0s and 168h")
	}
	if len(cfg.Gallery.WatermarkText) > 120 {
		return nil, fmt.Errorf("gallery.watermark_text must be at most 120 characters")
	}
	if cfg.Gallery.WatermarkFont != "" {
		if _, statErr := os.Stat(cfg.Gallery.WatermarkFont); statErr != nil {
			return nil, fmt.Errorf("gallery.watermark_font: %w", statErr)
		}
	}
	cfg.Proof = Proof{
		SessionTTL:         4 * time.Hour,
		TokenGrace:         7 * 24 * time.Hour,
		MaxAssets:          500,
		AnnotationMaxBytes: 16384,
	}
	if r.Proof.SessionTTL != "" {
		if cfg.Proof.SessionTTL, err = time.ParseDuration(r.Proof.SessionTTL); err != nil {
			return nil, fmt.Errorf("proof.session_ttl: %w", err)
		}
	}
	if r.Proof.TokenGrace != "" {
		if cfg.Proof.TokenGrace, err = time.ParseDuration(r.Proof.TokenGrace); err != nil {
			return nil, fmt.Errorf("proof.token_grace: %w", err)
		}
	}
	if v := r.Proof.MaxAssets; v > 0 {
		cfg.Proof.MaxAssets = v
	}
	if v := r.Proof.AnnotationMaxBytes; v > 0 {
		cfg.Proof.AnnotationMaxBytes = v
	}
	if cfg.Proof.SessionTTL < 5*time.Minute || cfg.Proof.SessionTTL > 24*time.Hour {
		return nil, fmt.Errorf("proof.session_ttl must be between 5m and 24h")
	}
	if cfg.Proof.TokenGrace < time.Hour || cfg.Proof.TokenGrace > 720*time.Hour {
		return nil, fmt.Errorf("proof.token_grace must be between 1h and 720h")
	}
	if cfg.Proof.MaxAssets > 5000 {
		return nil, fmt.Errorf("proof.max_assets must be at most 5000")
	}
	if cfg.Proof.AnnotationMaxBytes < 256 || cfg.Proof.AnnotationMaxBytes > 65536 {
		return nil, fmt.Errorf("proof.annotation_max_bytes must be between 256 and 65536")
	}

	cfg.Integrations = Integrations{
		Provider: strings.ToLower(strings.TrimSpace(r.Integrations.Provider)),
		FolderWatch: FolderWatch{
			Root:         strings.TrimSpace(r.Integrations.FolderWatch.Root),
			PollInterval: 30 * time.Second,
			OverdueAfter: 72 * time.Hour,
		},
	}
	if cfg.Integrations.Provider == "" {
		cfg.Integrations.Provider = "manual"
	}
	switch cfg.Integrations.Provider {
	case "manual", "folder_watch", "ftp", "pixcake", "xiangxiang":
	default:
		return nil, fmt.Errorf("integrations.provider %q is not supported (manual, folder_watch, ftp, pixcake, xiangxiang)", cfg.Integrations.Provider)
	}
	if r.Integrations.FolderWatch.PollInterval != "" {
		if cfg.Integrations.FolderWatch.PollInterval, err = time.ParseDuration(r.Integrations.FolderWatch.PollInterval); err != nil {
			return nil, fmt.Errorf("integrations.folder_watch.poll_interval: %w", err)
		}
	}
	if r.Integrations.FolderWatch.OverdueAfter != "" {
		if cfg.Integrations.FolderWatch.OverdueAfter, err = time.ParseDuration(r.Integrations.FolderWatch.OverdueAfter); err != nil {
			return nil, fmt.Errorf("integrations.folder_watch.overdue_after: %w", err)
		}
	}
	if cfg.Integrations.FolderWatch.PollInterval < 5*time.Second || cfg.Integrations.FolderWatch.PollInterval > time.Hour {
		return nil, fmt.Errorf("integrations.folder_watch.poll_interval must be between 5s and 1h")
	}
	if cfg.Integrations.FolderWatch.OverdueAfter < time.Hour || cfg.Integrations.FolderWatch.OverdueAfter > 720*time.Hour {
		return nil, fmt.Errorf("integrations.folder_watch.overdue_after must be between 1h and 720h")
	}
	if cfg.Integrations.Provider == "folder_watch" && cfg.Integrations.FolderWatch.Root == "" {
		return nil, fmt.Errorf("integrations.folder_watch.root is required when integrations.provider is folder_watch")
	}

	cfg.Notify = Notify{
		Email: Email{
			Host:     strings.TrimSpace(r.Notify.Email.Host),
			Port:     r.Notify.Email.Port,
			Username: strings.TrimSpace(r.Notify.Email.Username),
			Password: r.Notify.Email.Password,
			From:     strings.TrimSpace(r.Notify.Email.From),
			TLS:      strings.ToLower(strings.TrimSpace(r.Notify.Email.TLS)),
			Timeout:  20 * time.Second,
		},
		SMS: SMS{Provider: strings.ToLower(strings.TrimSpace(r.Notify.SMS.Provider))},
	}
	if r.Notify.Email.Enabled != nil {
		cfg.Notify.Email.Enabled = *r.Notify.Email.Enabled
	}
	if cfg.Notify.Email.TLS == "" {
		cfg.Notify.Email.TLS = "starttls"
	}
	switch cfg.Notify.Email.TLS {
	case "starttls", "tls", "none":
	default:
		return nil, fmt.Errorf("notify.email.tls must be starttls, tls or none")
	}
	if r.Notify.Email.Timeout != "" {
		if cfg.Notify.Email.Timeout, err = time.ParseDuration(r.Notify.Email.Timeout); err != nil {
			return nil, fmt.Errorf("notify.email.timeout: %w", err)
		}
	}
	if cfg.Notify.Email.Timeout <= 0 || cfg.Notify.Email.Timeout > 2*time.Minute {
		return nil, fmt.Errorf("notify.email.timeout must be between 0s and 2m")
	}
	if cfg.Notify.Email.Port == 0 {
		cfg.Notify.Email.Port = 587
	}
	if cfg.Notify.Email.Port < 1 || cfg.Notify.Email.Port > 65535 {
		return nil, fmt.Errorf("notify.email.port must be between 1 and 65535")
	}
	if cfg.Notify.Email.Enabled {
		if cfg.Notify.Email.Host == "" || cfg.Notify.Email.From == "" {
			return nil, fmt.Errorf("notify.email.host and notify.email.from are required when email is enabled")
		}
	}
	switch cfg.Notify.SMS.Provider {
	case "", "sandbox":
	default:
		return nil, fmt.Errorf("notify.sms.provider %q is not supported (empty or sandbox)", cfg.Notify.SMS.Provider)
	}
	cfg.Delivery = Delivery{
		PublicBaseURL:   strings.TrimRight(strings.TrimSpace(r.Delivery.PublicBaseURL), "/"),
		SessionTTL:      30 * time.Minute,
		DefaultTokenTTL: 336 * time.Hour,
		MaxTokenTTL:     720 * time.Hour,
		PresignTTL:      5 * time.Minute,
		MaxDownloads:    20,
		MaxAssets:       300,
		TempDir:         strings.TrimSpace(r.Delivery.TempDir),
		WebEdge:         2048,
		RequireWebSpec:  r.Delivery.RequireWebSpec,
	}
	if r.Delivery.SessionTTL != "" {
		if cfg.Delivery.SessionTTL, err = time.ParseDuration(r.Delivery.SessionTTL); err != nil {
			return nil, fmt.Errorf("delivery.session_ttl: %w", err)
		}
	}
	if r.Delivery.DefaultTokenTTL != "" {
		if cfg.Delivery.DefaultTokenTTL, err = time.ParseDuration(r.Delivery.DefaultTokenTTL); err != nil {
			return nil, fmt.Errorf("delivery.default_token_ttl: %w", err)
		}
	}
	if r.Delivery.MaxTokenTTL != "" {
		if cfg.Delivery.MaxTokenTTL, err = time.ParseDuration(r.Delivery.MaxTokenTTL); err != nil {
			return nil, fmt.Errorf("delivery.max_token_ttl: %w", err)
		}
	}
	if r.Delivery.PresignTTL != "" {
		if cfg.Delivery.PresignTTL, err = time.ParseDuration(r.Delivery.PresignTTL); err != nil {
			return nil, fmt.Errorf("delivery.presign_ttl: %w", err)
		}
	}
	if v := r.Delivery.MaxDownloads; v > 0 {
		cfg.Delivery.MaxDownloads = v
	}
	if v := r.Delivery.MaxAssets; v > 0 {
		cfg.Delivery.MaxAssets = v
	}
	if v := r.Delivery.WebEdge; v > 0 {
		cfg.Delivery.WebEdge = v
	}
	if cfg.Delivery.SessionTTL < 5*time.Minute || cfg.Delivery.SessionTTL > 24*time.Hour {
		return nil, fmt.Errorf("delivery.session_ttl must be between 5m and 24h")
	}
	if cfg.Delivery.DefaultTokenTTL < time.Hour || cfg.Delivery.DefaultTokenTTL > cfg.Delivery.MaxTokenTTL {
		return nil, fmt.Errorf("delivery.default_token_ttl must be between 1h and delivery.max_token_ttl")
	}
	if cfg.Delivery.MaxTokenTTL > 720*time.Hour {
		return nil, fmt.Errorf("delivery.max_token_ttl must be at most 720h")
	}
	if cfg.Delivery.PresignTTL < time.Minute || cfg.Delivery.PresignTTL > time.Hour {
		return nil, fmt.Errorf("delivery.presign_ttl must be between 1m and 1h")
	}
	if cfg.Delivery.MaxDownloads > 1000 {
		return nil, fmt.Errorf("delivery.max_downloads must be at most 1000")
	}
	if cfg.Delivery.MaxAssets > 5000 {
		return nil, fmt.Errorf("delivery.max_assets must be at most 5000")
	}
	if cfg.Delivery.WebEdge < 512 || cfg.Delivery.WebEdge > 4096 {
		return nil, fmt.Errorf("delivery.web_edge must be between 512 and 4096")
	}
	if cfg.Delivery.TempDir != "" {
		if info, statErr := os.Stat(cfg.Delivery.TempDir); statErr != nil {
			return nil, fmt.Errorf("delivery.temp_dir: %w", statErr)
		} else if !info.IsDir() {
			return nil, fmt.Errorf("delivery.temp_dir must be a directory")
		}
	}

	cfg.Archive = Archive{
		SweepInterval:   5 * time.Minute,
		DefaultKeepDays: 365,
		DefaultWarnDays: 30,
	}
	if r.Archive.SweepInterval != "" {
		if cfg.Archive.SweepInterval, err = time.ParseDuration(r.Archive.SweepInterval); err != nil {
			return nil, fmt.Errorf("archive.sweep_interval: %w", err)
		}
	}
	if v := r.Archive.DefaultKeepDays; v > 0 {
		cfg.Archive.DefaultKeepDays = v
	}
	if v := r.Archive.DefaultWarnDays; v > 0 {
		cfg.Archive.DefaultWarnDays = v
	}
	if cfg.Archive.SweepInterval < time.Minute || cfg.Archive.SweepInterval > 24*time.Hour {
		return nil, fmt.Errorf("archive.sweep_interval must be between 1m and 24h")
	}
	if cfg.Archive.DefaultKeepDays < 1 || cfg.Archive.DefaultKeepDays > 3650 {
		return nil, fmt.Errorf("archive.default_keep_days must be between 1 and 3650")
	}
	if cfg.Archive.DefaultWarnDays < 0 || cfg.Archive.DefaultWarnDays >= cfg.Archive.DefaultKeepDays {
		return nil, fmt.Errorf("archive.default_warn_days must be less than archive.default_keep_days")
	}
	if cfg.Env == "" {
		cfg.Env = "development"
	}
	if cfg.Gallery.PublicBaseURL == "" && cfg.Env != "production" {
		// The customer H5 dev server; production must configure its own origin.
		cfg.Gallery.PublicBaseURL = "http://localhost:5174"
	}
	if cfg.Delivery.PublicBaseURL == "" {
		cfg.Delivery.PublicBaseURL = cfg.Gallery.PublicBaseURL
	}
	if cfg.Worker.BatchSize <= 0 {
		cfg.Worker.BatchSize = 10
	}
	if cfg.Worker.ID == "" {
		if host, herr := os.Hostname(); herr == nil {
			cfg.Worker.ID = host + "-" + fmt.Sprint(os.Getpid())
		} else {
			cfg.Worker.ID = "worker"
		}
	}

	if cfg.HTTP.Addr == "" {
		cfg.HTTP.Addr = ":8080"
	}
	if cfg.Database.MaxConns <= 0 {
		cfg.Database.MaxConns = 20
	}
	if cfg.Database.MinConns <= 0 {
		cfg.Database.MinConns = 2
	}
	if cfg.Database.MinConns > cfg.Database.MaxConns {
		cfg.Database.MinConns = cfg.Database.MaxConns
	}
	if cfg.Database.URL == "" {
		return nil, fmt.Errorf("database.url is required")
	}
	if cfg.Database.MigrateURL == "" {
		cfg.Database.MigrateURL = cfg.Database.URL
	}
	if cfg.Auth.JWTSecret == "" {
		return nil, fmt.Errorf("auth.jwt_secret is required")
	}
	if cfg.Auth.AccessTTL <= 0 || cfg.Auth.RefreshTTL <= 0 {
		return nil, fmt.Errorf("auth ttl must be positive")
	}
	if cfg.Storage.Driver == "" {
		cfg.Storage.Driver = "minio"
	}
	if cfg.Storage.Driver != "minio" {
		return nil, fmt.Errorf("storage.driver %q is not supported (supported: minio)", cfg.Storage.Driver)
	}
	if cfg.Storage.Endpoint == "" {
		return nil, fmt.Errorf("storage.endpoint is required")
	}
	if cfg.Storage.Bucket == "" {
		return nil, fmt.Errorf("storage.bucket is required")
	}
	return cfg, nil
}

func splitConfigList(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			items = append(items, part)
		}
	}
	return items
}

// parseLeads accepts a comma-separated duration list, rejects non-positive or
// over-long entries and returns unique leads in descending order.
func parseLeads(raw string) ([]time.Duration, error) {
	seen := make(map[time.Duration]struct{})
	var out []time.Duration
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, err
		}
		if d <= 0 || d > 720*time.Hour {
			return nil, fmt.Errorf("lead %q must be greater than zero and at most 720h", part)
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one lead is required")
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out, nil
}

func parseOptionalLead(raw string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < 0 || d > 720*time.Hour {
		return 0, fmt.Errorf("lead must be between 0s and 720h")
	}
	return d, nil
}

// devSecretMarkers are substrings that only appear in the checked-in example
// configuration. Production must inject secrets from a secret manager, never
// inherit these values.
var devSecretMarkers = []string{
	"change-me", "changeme", "dev-only", "dev-secret", "replace-me",
	"example", "sandbox", "pwf-secret",
}

// devSecret reports whether value is empty or still looks like a development
// default. A false positive only rejects an unusual production secret, while a
// false negative would ship a publicly known one.
func devSecret(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return true
	}
	for _, marker := range devSecretMarkers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

// CheckProduction refuses to boot production deployments on development
// providers or development secrets. Sandbox adapters accept forged callbacks by
// design and must never handle real money or contracts. This build ships only
// sandbox adapters, so env=production is expected to fail until a real
// payment.Provider and esign.Provider are implemented and verified; that is the
// launch gate, not a supported configuration.
func (c *Config) CheckProduction() error {
	switch c.Env {
	case "development", "production", "test":
	case "":
		return fmt.Errorf("env must be development, production or test")
	default:
		return fmt.Errorf("unknown env %q (allowed: development, production, test)", c.Env)
	}
	if c.Env != "production" {
		return nil
	}
	// An empty provider defaults to sandbox in the factory, so both spellings
	// must be refused here; otherwise a production config that forgot the
	// provider would silently run the sandbox adapter.
	paymentProvider := strings.TrimSpace(c.Payment.Provider)
	if paymentProvider == "" {
		paymentProvider = "sandbox"
	}
	if paymentProvider == "sandbox" {
		return fmt.Errorf("payment provider is sandbox (explicit or by empty default) when env=production: this build ships only the sandbox adapter; implement and verify a real payment.Provider before enabling production")
	}
	esignProvider := strings.TrimSpace(c.ESign.Provider)
	if esignProvider == "" {
		esignProvider = "sandbox"
	}
	if esignProvider == "sandbox" {
		return fmt.Errorf("esign provider is sandbox (explicit or by empty default) when env=production: this build ships only the sandbox adapter; implement and verify a real esign.Provider before enabling production")
	}
	if len(c.Auth.JWTSecret) < 32 || devSecret(c.Auth.JWTSecret) {
		return fmt.Errorf("auth.jwt_secret must be a random value of at least 32 characters when env=production")
	}
	if devSecret(c.Storage.SecretKey) {
		return fmt.Errorf("storage.secret_key must not use a development default when env=production")
	}
	if c.Payment.CallbackSecret != "" && devSecret(c.Payment.CallbackSecret) {
		return fmt.Errorf("payment.callback_secret must not use a development default when env=production")
	}
	if c.ESign.CallbackSecret != "" && devSecret(c.ESign.CallbackSecret) {
		return fmt.Errorf("esign.callback_secret must not use a development default when env=production")
	}
	if c.Alerts.WebhookURL != "" && devSecret(c.Alerts.WebhookSecret) {
		return fmt.Errorf("alerts.webhook_secret must not use a development default when env=production")
	}
	if c.Reminders.WebhookURL != "" && devSecret(c.Reminders.WebhookSecret) {
		return fmt.Errorf("reminders.webhook_secret must not use a development default when env=production")
	}
	// Share links must point at the customer app, and watermarked previews need
	// a font that covers the studio name; the embedded fallback is Latin-only.
	if c.Gallery.PublicBaseURL == "" {
		return fmt.Errorf("gallery.public_base_url is required when env=production")
	}
	if !strings.HasPrefix(c.Gallery.PublicBaseURL, "https://") {
		return fmt.Errorf("gallery.public_base_url must use https when env=production")
	}
	// Delivery links default to the gallery origin, so an unset
	// delivery.public_base_url is valid once the gallery origin is https.
	if c.Delivery.PublicBaseURL != "" && !strings.HasPrefix(c.Delivery.PublicBaseURL, "https://") {
		return fmt.Errorf("delivery.public_base_url must use https when env=production")
	}
	if c.Gallery.WatermarkFont == "" {
		return fmt.Errorf("gallery.watermark_font is required when env=production")
	}
	// Proof invitation links carry one-time tokens; clear-text SMTP would leak
	// them on the wire even though the body is wiped after delivery.
	if !c.Notify.Email.Enabled {
		return fmt.Errorf("notify.email.enabled is required when env=production")
	}
	if c.Notify.Email.TLS == "none" {
		return fmt.Errorf("notify.email.tls must not be none when env=production")
	}
	if c.Notify.SMS.Provider == "sandbox" {
		return fmt.Errorf("notify.sms.provider %q is not allowed when env=production", c.Notify.SMS.Provider)
	}
	return nil
}
