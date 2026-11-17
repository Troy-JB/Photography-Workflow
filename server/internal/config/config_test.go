package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const baseYAML = `
http:
  addr: ":9000"
database:
  url: "postgres://base"
  max_conns: 5
  min_conns: 1
auth:
  jwt_secret: "base-secret"
  access_ttl: "10m"
  refresh_ttl: "24h"
storage:
  driver: "minio"
  endpoint: "localhost:9000"
  bucket: "base"
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvOverlay(t *testing.T) {
	path := writeConfig(t, baseYAML)
	t.Setenv("PWF_HTTP__ADDR", ":7777")
	t.Setenv("PWF_HTTP__CORS_ORIGINS", " https://admin.example.com, http://localhost:5175 ")
	t.Setenv("PWF_DATABASE__MAX_CONNS", "12")
	t.Setenv("PWF_DATABASE__MIN_CONNS", "4")
	t.Setenv("PWF_AUTH__ACCESS_TTL", "30m")
	t.Setenv("PWF_STORAGE__USE_SSL", "true")
	t.Setenv("PWF_STORAGE__BUCKET", "from-env")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTP.Addr != ":7777" {
		t.Fatalf("addr = %q", cfg.HTTP.Addr)
	}
	wantOrigins := []string{"https://admin.example.com", "http://localhost:5175"}
	if len(cfg.HTTP.CORSOrigins) != len(wantOrigins) {
		t.Fatalf("cors origins = %#v", cfg.HTTP.CORSOrigins)
	}
	for i := range wantOrigins {
		if cfg.HTTP.CORSOrigins[i] != wantOrigins[i] {
			t.Fatalf("cors origins = %#v", cfg.HTTP.CORSOrigins)
		}
	}
	if cfg.Database.MaxConns != 12 || cfg.Database.MinConns != 4 {
		t.Fatalf("conns = %d/%d", cfg.Database.MaxConns, cfg.Database.MinConns)
	}
	if cfg.Auth.AccessTTL != 30*time.Minute {
		t.Fatalf("access ttl = %v", cfg.Auth.AccessTTL)
	}
	if !cfg.Storage.UseSSL {
		t.Fatal("use_ssl should be true")
	}
	if cfg.Storage.Bucket != "from-env" {
		t.Fatalf("bucket = %q", cfg.Storage.Bucket)
	}
	if cfg.Database.MigrateURL != cfg.Database.URL {
		t.Fatalf("migrate_url should default to url, got %q", cfg.Database.MigrateURL)
	}
	if cfg.Storage.Driver != "minio" {
		t.Fatalf("driver = %q", cfg.Storage.Driver)
	}
	if cfg.Gallery.WatermarkText != "Lumeo" {
		t.Fatalf("default gallery watermark = %q, want Lumeo", cfg.Gallery.WatermarkText)
	}
}

func TestCustomerPortalConfig(t *testing.T) {
	path := writeConfig(t, baseYAML)
	t.Setenv("PWF_CUSTOMER__DEV_LOGIN", "true")
	t.Setenv("PWF_CUSTOMER__ACCESS_TTL", "3h")
	t.Setenv("PWF_CUSTOMER__REFRESH_TTL", "96h")
	t.Setenv("PWF_CUSTOMER__PUBLIC_BASE_URL", "https://client.example.com/")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Customer.DevLogin || cfg.Customer.AccessTTL != 3*time.Hour || cfg.Customer.RefreshTTL != 96*time.Hour {
		t.Fatalf("customer auth config = %+v", cfg.Customer)
	}
	if cfg.Customer.PublicBaseURL != "https://client.example.com" {
		t.Fatalf("customer public base URL = %q", cfg.Customer.PublicBaseURL)
	}
}

func TestCustomerDevLoginRejectedInProduction(t *testing.T) {
	path := writeConfig(t, "env: production\n"+baseYAML)
	t.Setenv("PWF_CUSTOMER__DEV_LOGIN", "true")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "customer.dev_login") {
		t.Fatalf("Load error = %v, want production dev-login rejection", err)
	}
}

func TestLoadClampsMinConns(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Database.MinConns != 1 {
		t.Fatalf("min conns = %d, want 1", cfg.Database.MinConns)
	}

	t.Setenv("PWF_DATABASE__MIN_CONNS", "50")
	t.Setenv("PWF_DATABASE__MAX_CONNS", "8")
	cfg, err = Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Database.MinConns != 8 {
		t.Fatalf("min conns should clamp to max, got %d", cfg.Database.MinConns)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		env  [2]string
		yaml string
		want string
	}{
		{"unsupported driver", [2]string{"PWF_STORAGE__DRIVER", "s3"}, baseYAML, "not supported"},
		{"bad duration", [2]string{"PWF_AUTH__ACCESS_TTL", "soon"}, baseYAML, "access_ttl"},
		{"missing url", [2]string{"PWF_DATABASE__URL", ""}, baseYAML, "database.url"},
		{"missing bucket", [2]string{"PWF_STORAGE__BUCKET", ""}, baseYAML, "storage.bucket"},
		{"missing secret", [2]string{"PWF_AUTH__JWT_SECRET", ""}, baseYAML, "jwt_secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(c.env[0], c.env[1])
			_, err := Load(writeConfig(t, c.yaml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want substring %q", err, c.want)
			}
		})
	}
}

func TestLoadAgentMinVersion(t *testing.T) {
	t.Setenv("PWF_AGENT__MIN_VERSION", "  0.4.0 ")
	cfg, err := Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.MinVersion != "0.4.0" {
		t.Fatalf("min version = %q, want trimmed 0.4.0", cfg.Agent.MinVersion)
	}

	t.Setenv("PWF_AGENT__MIN_VERSION", strings.Repeat("1", 65))
	if _, err := Load(writeConfig(t, baseYAML)); err == nil || !strings.Contains(err.Error(), "agent.min_version") {
		t.Fatalf("error = %v, want agent.min_version length failure", err)
	}
}

func TestLoadPublicLeadConfig(t *testing.T) {
	t.Setenv("PWF_PUBLIC_LEADS__TENANT_ID", "tenant-id")
	t.Setenv("PWF_PUBLIC_LEADS__ORIGINS", " https://lumeo.example, http://localhost:5175 ,, ")
	cfg, err := Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicLeads.TenantID != "tenant-id" {
		t.Fatalf("tenant id = %q", cfg.PublicLeads.TenantID)
	}
	want := []string{"https://lumeo.example", "http://localhost:5175"}
	if len(cfg.PublicLeads.Origins) != len(want) {
		t.Fatalf("origins = %#v", cfg.PublicLeads.Origins)
	}
	for i := range want {
		if cfg.PublicLeads.Origins[i] != want[i] {
			t.Fatalf("origins = %#v", cfg.PublicLeads.Origins)
		}
	}
}

func TestCheckProduction(t *testing.T) {
	cfg := &Config{Env: "development", Payment: Payment{Provider: "sandbox"}, ESign: ESign{Provider: "sandbox"}}
	if err := cfg.CheckProduction(); err != nil {
		t.Fatalf("development should allow sandbox: %v", err)
	}

	cfg.Env = "production"
	// An empty provider defaults to sandbox in the factory, so production must
	// reject the unset spelling too.
	cfg.Payment.Provider = ""
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "payment provider") {
		t.Fatalf("error = %v, want empty payment provider rejection", err)
	}
	cfg.Payment.Provider = "sandbox"
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "payment provider") {
		t.Fatalf("error = %v, want payment provider rejection", err)
	}
	cfg.Payment.Provider = "wechat"
	cfg.ESign.Provider = ""
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "esign provider") {
		t.Fatalf("error = %v, want empty esign provider rejection", err)
	}
	cfg.ESign.Provider = "fadada"
	cfg.Auth.JWTSecret = strings.Repeat("k", 48)
	cfg.Storage.SecretKey = strings.Repeat("s", 32)
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "gallery.public_base_url") {
		t.Fatalf("error = %v, want production gallery origin rejection", err)
	}
	cfg.Gallery.PublicBaseURL = "http://gallery.example.com"
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("error = %v, want https enforcement", err)
	}
	cfg.Gallery.PublicBaseURL = "https://gallery.example.com"
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "watermark_font") {
		t.Fatalf("error = %v, want watermark font rejection", err)
	}
	cfg.Gallery.WatermarkFont = "C:/Windows/Fonts/msyh.ttc"
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "notify.email.enabled") {
		t.Fatalf("error = %v, want proof email requirement", err)
	}
	cfg.Notify.Email.Enabled = true
	cfg.Notify.Email.TLS = "none"
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "notify.email.tls") {
		t.Fatalf("error = %v, want clear-text SMTP rejection", err)
	}
	cfg.Notify.Email.TLS = "starttls"
	cfg.Notify.SMS.Provider = "sandbox"
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "notify.sms.provider") {
		t.Fatalf("error = %v, want production SMS sandbox rejection", err)
	}
	cfg.Notify.SMS.Provider = ""
	if err := cfg.CheckProduction(); err != nil {
		t.Fatalf("production with configured gallery should pass: %v", err)
	}
	// The delivery origin defaults to the gallery origin; an explicit plain-HTTP
	// override is still rejected.
	cfg.Delivery.PublicBaseURL = "http://delivery.example.com"
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "delivery.public_base_url") {
		t.Fatalf("error = %v, want delivery https enforcement", err)
	}
	cfg.Delivery.PublicBaseURL = ""

	cfg.Env = "staging"
	if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), "unknown env") {
		t.Fatalf("error = %v, want unknown env rejection", err)
	}
}

func TestCheckProductionRejectsDevelopmentSecrets(t *testing.T) {
	valid := func() *Config {
		return &Config{
			Env:     "production",
			Auth:    Auth{JWTSecret: strings.Repeat("k", 48)},
			Storage: Storage{SecretKey: strings.Repeat("s", 32)},
			Payment: Payment{Provider: "wechatpay"},
			ESign:   ESign{Provider: "esignbao"},
			Gallery: Gallery{PublicBaseURL: "https://gallery.example.com", WatermarkFont: "font.ttf"},
			Notify:  Notify{Email: Email{Enabled: true, TLS: "starttls"}},
		}
	}
	cfg := valid()
	if err := cfg.CheckProduction(); err != nil {
		t.Fatalf("baseline production config should pass: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"jwt default", func(c *Config) { c.Auth.JWTSecret = "dev-only-change-me" }, "auth.jwt_secret"},
		{"jwt too short", func(c *Config) { c.Auth.JWTSecret = strings.Repeat("k", 31) }, "auth.jwt_secret"},
		{"storage default", func(c *Config) { c.Storage.SecretKey = "pwf-secret" }, "storage.secret_key"},
		{"payment callback default", func(c *Config) { c.Payment.CallbackSecret = "dev-only-payment-secret-change-me" }, "payment.callback_secret"},
		{"esign callback default", func(c *Config) { c.ESign.CallbackSecret = "dev-only-esign-secret-change-me" }, "esign.callback_secret"},
		{"alerts webhook default", func(c *Config) {
			c.Alerts.WebhookURL = "https://alerts.example.com/hook"
			c.Alerts.WebhookSecret = "dev-only-alerts-secret-change-me"
		}, "alerts.webhook_secret"},
		{"reminders webhook default", func(c *Config) {
			c.Reminders.WebhookURL = "https://reminders.example.com/hook"
			c.Reminders.WebhookSecret = "dev-only-reminders-secret-change-me"
		}, "reminders.webhook_secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			if err := cfg.CheckProduction(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadProofDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Proof
	if p.SessionTTL != 4*time.Hour || p.TokenGrace != 168*time.Hour ||
		p.MaxAssets != 500 || p.AnnotationMaxBytes != 16384 {
		t.Fatalf("proof defaults = %+v", p)
	}

	t.Setenv("PWF_PROOF__SESSION_TTL", "30m")
	t.Setenv("PWF_PROOF__TOKEN_GRACE", "48h")
	t.Setenv("PWF_PROOF__MAX_ASSETS", "50")
	t.Setenv("PWF_PROOF__ANNOTATION_MAX_BYTES", "4096")
	cfg, err = Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Proof.SessionTTL != 30*time.Minute || cfg.Proof.TokenGrace != 48*time.Hour ||
		cfg.Proof.MaxAssets != 50 || cfg.Proof.AnnotationMaxBytes != 4096 {
		t.Fatalf("proof overrides = %+v", cfg.Proof)
	}

	for _, tc := range []struct{ name, env, value, want string }{
		{"bad session ttl", "PWF_PROOF__SESSION_TTL", "soon", "proof.session_ttl"},
		{"short session ttl", "PWF_PROOF__SESSION_TTL", "1m", "between 5m and 24h"},
		{"bad grace", "PWF_PROOF__TOKEN_GRACE", "soon", "proof.token_grace"},
		{"long grace", "PWF_PROOF__TOKEN_GRACE", "1000h", "between 1h and 720h"},
		{"many assets", "PWF_PROOF__MAX_ASSETS", "9999", "proof.max_assets"},
		{"tiny annotation cap", "PWF_PROOF__ANNOTATION_MAX_BYTES", "1", "annotation_max_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			if _, err := Load(writeConfig(t, baseYAML)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadIntegrations(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Integrations.Provider != "manual" {
		t.Fatalf("default provider = %q", cfg.Integrations.Provider)
	}
	if cfg.Integrations.FolderWatch.PollInterval != 30*time.Second ||
		cfg.Integrations.FolderWatch.OverdueAfter != 72*time.Hour {
		t.Fatalf("folder watch defaults = %+v", cfg.Integrations.FolderWatch)
	}

	t.Setenv("PWF_INTEGRATIONS__PROVIDER", "folder_watch")
	if _, err := Load(writeConfig(t, baseYAML)); err == nil || !strings.Contains(err.Error(), "folder_watch.root") {
		t.Fatalf("error = %v, want missing root rejection", err)
	}

	t.Setenv("PWF_INTEGRATIONS__FOLDER_WATCH__ROOT", t.TempDir())
	t.Setenv("PWF_INTEGRATIONS__FOLDER_WATCH__POLL_INTERVAL", "1m")
	t.Setenv("PWF_INTEGRATIONS__FOLDER_WATCH__OVERDUE_AFTER", "24h")
	cfg, err = Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Integrations.FolderWatch.PollInterval != time.Minute ||
		cfg.Integrations.FolderWatch.OverdueAfter != 24*time.Hour {
		t.Fatalf("folder watch overrides = %+v", cfg.Integrations.FolderWatch)
	}

	// ftp/pixcake/xiangxiang parse cleanly here; the provider factory refuses
	// them at startup so operators get a single explicit failure.
	t.Setenv("PWF_INTEGRATIONS__PROVIDER", "ftp")
	if _, err := Load(writeConfig(t, baseYAML)); err != nil {
		t.Fatalf("ftp should load until the provider factory rejects it: %v", err)
	}

	for _, tc := range []struct{ name, value, want string }{
		{"unknown provider", "acme", "not supported"},
		{"fast poll", "2s", "poll_interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "fast poll" {
				t.Setenv("PWF_INTEGRATIONS__PROVIDER", "folder_watch")
				t.Setenv("PWF_INTEGRATIONS__FOLDER_WATCH__POLL_INTERVAL", tc.value)
			} else {
				t.Setenv("PWF_INTEGRATIONS__PROVIDER", tc.value)
			}
			if _, err := Load(writeConfig(t, baseYAML)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadNotifyEmail(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	e := cfg.Notify.Email
	if e.Enabled || e.Port != 587 || e.TLS != "starttls" || e.Timeout != 20*time.Second {
		t.Fatalf("email defaults = %+v", e)
	}

	t.Setenv("PWF_NOTIFY__EMAIL__ENABLED", "true")
	if _, err := Load(writeConfig(t, baseYAML)); err == nil || !strings.Contains(err.Error(), "notify.email.host") {
		t.Fatalf("error = %v, want enabled host requirement", err)
	}

	t.Setenv("PWF_NOTIFY__EMAIL__HOST", "smtp.example.com")
	t.Setenv("PWF_NOTIFY__EMAIL__FROM", "studio@example.com")
	t.Setenv("PWF_NOTIFY__EMAIL__TLS", "tls")
	t.Setenv("PWF_NOTIFY__EMAIL__PORT", "465")
	cfg, err = Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Notify.Email.Enabled || cfg.Notify.Email.TLS != "tls" || cfg.Notify.Email.Port != 465 {
		t.Fatalf("email overrides = %+v", cfg.Notify.Email)
	}

	for _, tc := range []struct{ name, env, value, want string }{
		{"bad tls", "PWF_NOTIFY__EMAIL__TLS", "plain", "notify.email.tls"},
		{"bad port", "PWF_NOTIFY__EMAIL__PORT", "70000", "notify.email.port"},
		{"bad timeout", "PWF_NOTIFY__EMAIL__TIMEOUT", "soon", "notify.email.timeout"},
		{"unknown sms", "PWF_NOTIFY__SMS__PROVIDER", "aliyun", "notify.sms.provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			if _, err := Load(writeConfig(t, baseYAML)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadScheduleDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Schedule.Timezone == nil || cfg.Schedule.Timezone.String() != "Asia/Shanghai" {
		t.Fatalf("default timezone = %v", cfg.Schedule.Timezone)
	}
	if cfg.Schedule.HoldTTL != 30*time.Minute {
		t.Fatalf("default hold ttl = %v", cfg.Schedule.HoldTTL)
	}

	t.Setenv("PWF_SCHEDULE__TIMEZONE", "America/New_York")
	t.Setenv("PWF_SCHEDULE__HOLD_TTL", "5s")
	cfg, err = Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Schedule.Timezone.String() != "America/New_York" || cfg.Schedule.HoldTTL != 5*time.Second {
		t.Fatalf("schedule override = %v/%v", cfg.Schedule.Timezone, cfg.Schedule.HoldTTL)
	}

	for _, tc := range []struct{ name, env, value, want string }{
		{"bad timezone", "PWF_SCHEDULE__TIMEZONE", "Mars/Olympus", "schedule.timezone"},
		{"bad ttl", "PWF_SCHEDULE__HOLD_TTL", "soon", "schedule.hold_ttl"},
		{"zero ttl", "PWF_SCHEDULE__HOLD_TTL", "0s", "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			if _, err := Load(writeConfig(t, baseYAML)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadReminderDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := cfg.Reminders
	if !r.Enabled || r.AuthLead != 24*time.Hour || r.HoldLead != 10*time.Minute ||
		r.WebhookTimeout != 10*time.Second {
		t.Fatalf("reminder defaults = %+v", r)
	}
	if len(r.ShootLeads) != 2 || r.ShootLeads[0] != 24*time.Hour || r.ShootLeads[1] != 2*time.Hour {
		t.Fatalf("default shoot leads = %v", r.ShootLeads)
	}

	t.Setenv("PWF_REMINDERS__ENABLED", "false")
	t.Setenv("PWF_REMINDERS__SHOOT_LEADS", "48h,1h,48h")
	t.Setenv("PWF_REMINDERS__AUTH_LEAD", "0s")
	t.Setenv("PWF_REMINDERS__HOLD_LEAD", "5m")
	t.Setenv("PWF_REMINDERS__WEBHOOK_URL", "http://127.0.0.1:9000/hook")
	t.Setenv("PWF_REMINDERS__WEBHOOK_SECRET", "0123456789abcdef")
	cfg, err = Load(writeConfig(t, baseYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r = cfg.Reminders
	if r.Enabled {
		t.Fatal("enabled should be false")
	}
	if len(r.ShootLeads) != 2 || r.ShootLeads[0] != 48*time.Hour || r.ShootLeads[1] != time.Hour {
		t.Fatalf("shoot leads = %v", r.ShootLeads)
	}
	if r.AuthLead != 0 || r.HoldLead != 5*time.Minute || r.WebhookURL == "" || r.WebhookSecret == "" {
		t.Fatalf("reminder overrides = %+v", r)
	}
}

func TestLoadRejectsBadReminderValues(t *testing.T) {
	cases := []struct{ name, env, value, want string }{
		{"bad lead", "PWF_REMINDERS__SHOOT_LEADS", "soon", "shoot_leads"},
		{"negative lead", "PWF_REMINDERS__SHOOT_LEADS", "-1h", "greater than zero"},
		{"too long", "PWF_REMINDERS__SHOOT_LEADS", "1000h", "at most 720h"},
		{"empty leads", "PWF_REMINDERS__SHOOT_LEADS", ",", "at least one lead"},
		{"negative auth lead", "PWF_REMINDERS__AUTH_LEAD", "-1m", "auth_lead"},
		{"bad timeout", "PWF_REMINDERS__WEBHOOK_TIMEOUT", "soon", "webhook_timeout"},
		{"zero timeout", "PWF_REMINDERS__WEBHOOK_TIMEOUT", "0s", "must be positive"},
		{"url without secret", "PWF_REMINDERS__WEBHOOK_URL", "http://hook", "webhook_secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(c.env, c.value)
			if _, err := Load(writeConfig(t, baseYAML)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want substring %q", err, c.want)
			}
		})
	}
}

func TestLoadMissingFileUsesEnv(t *testing.T) {
	t.Setenv("PWF_DATABASE__URL", "postgres://env")
	t.Setenv("PWF_AUTH__JWT_SECRET", "env-secret")
	t.Setenv("PWF_AUTH__ACCESS_TTL", "5m")
	t.Setenv("PWF_AUTH__REFRESH_TTL", "1h")
	t.Setenv("PWF_STORAGE__ENDPOINT", "localhost:9000")
	t.Setenv("PWF_STORAGE__BUCKET", "env-bucket")

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Database.URL != "postgres://env" || cfg.Storage.Bucket != "env-bucket" {
		t.Fatalf("env config not applied: %+v", cfg)
	}
	if cfg.Database.MaxConns != 20 || cfg.Database.MinConns != 2 {
		t.Fatalf("conn defaults = %d/%d", cfg.Database.MaxConns, cfg.Database.MinConns)
	}
}
