// Package config loads API settings from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env      string // "development" or "production"
	HTTPAddr string

	DatabaseURL string
	ValkeyURL   string

	// Browser origins allowed to call the API with credentials, e.g. https://seeyouthere.at.
	AppOrigins []string
	// Cookie domain, e.g. ".seeyouthere.at" so the web app and API share the session. Empty for host-only.
	CookieDomain string
	CookieSecure bool
	SessionTTL   time.Duration

	// Secret used to HMAC login codes. At least 32 bytes.
	AuthSecret []byte

	// Trust the rightmost X-Forwarded-For entry (set when running behind Caddy).
	TrustProxy bool

	SMTPAddr string
	SMTPUser string
	SMTPPass string
	MailFrom string

	// RSVP* optionally configure a second SMTP identity used only for
	// yes-RSVP calendar-invite confirmations (internal/jobs
	// SendRSVPConfirmationWorker.CalendarSender). All four are optional;
	// when unset, that email falls back to the primary SMTP identity above.
	RSVPSMTPAddr string
	RSVPSMTPUser string
	RSVPSMTPPass string
	RSVPMailFrom string

	// SiteURL is the public web origin (e.g. https://seeyouthere.at), used to
	// build links in emails and event `url` fields.
	SiteURL string
	// MediaRoot is the filesystem root for uploaded/processed images (see
	// internal/media). Created and writability-checked by media.NewStore.
	MediaRoot string
	// R2* configure Cloudflare R2 as the media object store. All four are set
	// together or not at all; unset, renditions live on local disk under
	// MediaRoot/objects. R2SecretAccessKey must never be logged or printed.
	R2Endpoint        string
	R2Bucket          string
	R2AccessKeyID     string
	R2SecretAccessKey string
	// MediaUserQuotaBytes caps one user's stored photos; MediaTotalQuotaBytes
	// caps all photos (keeps the bucket inside the free tier). Decimal MB.
	MediaUserQuotaBytes  int64
	MediaTotalQuotaBytes int64
	// TOTPKey seals/opens admin TOTP secrets at rest (AES-256-GCM). Exactly 32 bytes.
	TOTPKey []byte
	// AdminAlertEmail receives notify_report emails. Empty disables them.
	AdminAlertEmail string
	// MailDailyInviteCap is the global daily cap on send_invite jobs.
	MailDailyInviteCap int
	// AdminMFATTL is how long an admin session's step-up verification lasts.
	AdminMFATTL time.Duration
}

func (c Config) IsProduction() bool { return c.Env == "production" }

// R2Configured reports whether media is stored in R2 rather than on disk.
func (c Config) R2Configured() bool { return c.R2Endpoint != "" }

// String and GoString keep the R2 secret out of any %v / %+v / %#v output.
func (c Config) String() string   { return "config.Config{redacted}" }
func (c Config) GoString() string { return c.String() }

var r2BucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

const (
	megabyte         = 1_000_000
	r2EndpointSuffix = ".r2.cloudflarestorage.com"
)

func Load() (Config, error) {
	c := Config{
		Env:          env("APP_ENV", "development"),
		HTTPAddr:     env("HTTP_ADDR", ":8080"),
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		ValkeyURL:    env("VALKEY_URL", "redis://localhost:6380/0"),
		CookieDomain: os.Getenv("COOKIE_DOMAIN"),
		SMTPAddr:     os.Getenv("SMTP_ADDR"),
		SMTPUser:     os.Getenv("SMTP_USER"),
		SMTPPass:     os.Getenv("SMTP_PASS"),
		MailFrom:     env("MAIL_FROM", "See You There <hello@seeyouthere.at>"),
		AuthSecret:   []byte(os.Getenv("AUTH_SECRET")),

		RSVPSMTPAddr: os.Getenv("RSVP_SMTP_ADDR"),
		RSVPSMTPUser: os.Getenv("RSVP_SMTP_USER"),
		RSVPSMTPPass: os.Getenv("RSVP_SMTP_PASS"),
		RSVPMailFrom: os.Getenv("RSVP_MAIL_FROM"),

		SiteURL:         env("SITE_URL", "http://localhost:3100"),
		AdminAlertEmail: os.Getenv("ADMIN_ALERT_EMAIL"),

		R2Endpoint:        os.Getenv("R2_ENDPOINT"),
		R2Bucket:          os.Getenv("R2_BUCKET"),
		R2AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		R2SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
	}

	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.SMTPAddr == "" {
		errs = append(errs, errors.New("SMTP_ADDR is required"))
	}
	// Zoho (production) requires auth; a local catcher like Mailpit doesn't, and
	// net/smtp.SendMail already skips AUTH when SMTPUser is empty (internal/mail).
	if c.IsProduction() && (c.SMTPUser == "" || c.SMTPPass == "") {
		errs = append(errs, errors.New("SMTP_USER and SMTP_PASS are required in production"))
	}
	if len(c.AuthSecret) < 32 {
		errs = append(errs, errors.New("AUTH_SECRET must be at least 32 bytes"))
	}

	// RSVP_SMTP_USER/PASS/RSVP_MAIL_FROM form one identity: if any is set,
	// require all three (ADDR defaults to the primary SMTPAddr below, so
	// it alone doesn't trigger this). Unset entirely, the feature falls
	// back to the primary SMTP identity (never a hard failure).
	if c.RSVPSMTPUser != "" || c.RSVPSMTPPass != "" || c.RSVPMailFrom != "" {
		var missing []string
		if c.RSVPSMTPUser == "" {
			missing = append(missing, "RSVP_SMTP_USER")
		}
		if c.RSVPSMTPPass == "" {
			missing = append(missing, "RSVP_SMTP_PASS")
		}
		if c.RSVPMailFrom == "" {
			missing = append(missing, "RSVP_MAIL_FROM")
		}
		if len(missing) > 0 {
			errs = append(errs, fmt.Errorf("%s must be set together", strings.Join(missing, ", ")))
		}
		if c.RSVPMailFrom != "" {
			if _, err := mail.ParseAddress(c.RSVPMailFrom); err != nil {
				errs = append(errs, fmt.Errorf("RSVP_MAIL_FROM: %q is not a valid address", c.RSVPMailFrom))
			}
		}
		if c.RSVPSMTPAddr == "" {
			c.RSVPSMTPAddr = c.SMTPAddr
		}
	}

	if u, err := url.Parse(c.SiteURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		errs = append(errs, fmt.Errorf("SITE_URL: %q must be an absolute http(s) origin", c.SiteURL))
	}

	defaultMediaRoot := "./data/media"
	if c.IsProduction() {
		defaultMediaRoot = "/data/media"
	}
	c.MediaRoot = env("MEDIA_ROOT", defaultMediaRoot)
	if c.MediaRoot == "" {
		errs = append(errs, errors.New("MEDIA_ROOT must not be empty"))
	}

	errs = append(errs, c.validateR2()...)

	userMB, err := strconv.ParseInt(env("MEDIA_USER_QUOTA_MB", "200"), 10, 64)
	if err != nil || userMB < 1 || userMB > 1_000_000 {
		errs = append(errs, errors.New("MEDIA_USER_QUOTA_MB must be an integer between 1 and 1000000"))
	}
	totalMB, err := strconv.ParseInt(env("MEDIA_TOTAL_QUOTA_MB", "9000"), 10, 64)
	if err != nil || totalMB < 100 || totalMB > 10_000_000 {
		errs = append(errs, errors.New("MEDIA_TOTAL_QUOTA_MB must be an integer between 100 and 10000000"))
	} else if userMB >= 1 && userMB <= 1_000_000 && totalMB < userMB {
		errs = append(errs, errors.New("MEDIA_TOTAL_QUOTA_MB must be at least MEDIA_USER_QUOTA_MB"))
	} else {
		c.MediaUserQuotaBytes = userMB * megabyte
		c.MediaTotalQuotaBytes = totalMB * megabyte
	}

	if raw := os.Getenv("TOTP_KEY"); raw == "" {
		errs = append(errs, errors.New("TOTP_KEY is required"))
	} else if key, err := base64.StdEncoding.DecodeString(raw); err != nil || len(key) != 32 {
		errs = append(errs, errors.New("TOTP_KEY must be base64 for exactly 32 bytes"))
	} else {
		c.TOTPKey = key
	}

	if c.AdminAlertEmail != "" {
		if addr, err := mail.ParseAddress(c.AdminAlertEmail); err != nil || addr.Address != c.AdminAlertEmail {
			errs = append(errs, fmt.Errorf("ADMIN_ALERT_EMAIL: %q is not a valid address", c.AdminAlertEmail))
		}
	}

	inviteCap, err := strconv.Atoi(env("MAIL_DAILY_INVITE_CAP", "300"))
	if err != nil || inviteCap < 1 || inviteCap > 100000 {
		errs = append(errs, errors.New("MAIL_DAILY_INVITE_CAP must be an integer between 1 and 100000"))
	} else {
		c.MailDailyInviteCap = inviteCap
	}

	if c.AdminMFATTL, err = time.ParseDuration(env("ADMIN_MFA_TTL", "12h")); err != nil {
		errs = append(errs, fmt.Errorf("ADMIN_MFA_TTL: %w", err))
	} else if c.AdminMFATTL < 15*time.Minute || c.AdminMFATTL > 24*time.Hour {
		errs = append(errs, errors.New("ADMIN_MFA_TTL must be between 15m and 24h"))
	}

	for _, o := range strings.Split(env("APP_ORIGINS", "http://localhost:3100"), ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if u, err := url.Parse(o); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
			errs = append(errs, fmt.Errorf("APP_ORIGINS: %q must be scheme://host[:port]", o))
			continue
		}
		c.AppOrigins = append(c.AppOrigins, o)
	}

	if c.CookieSecure, err = boolEnv("COOKIE_SECURE", c.IsProduction()); err != nil {
		errs = append(errs, err)
	}
	if c.TrustProxy, err = boolEnv("TRUST_PROXY", false); err != nil {
		errs = append(errs, err)
	}
	if c.SessionTTL, err = time.ParseDuration(env("SESSION_TTL", "720h")); err != nil {
		errs = append(errs, fmt.Errorf("SESSION_TTL: %w", err))
	}
	if c.IsProduction() && !c.CookieSecure {
		errs = append(errs, errors.New("COOKIE_SECURE must be true in production"))
	}

	return c, errors.Join(errs...)
}

// validateR2 checks the all-or-none R2 variables. Errors never include the
// secret or the access key id.
func (c Config) validateR2() []error {
	if c.R2Endpoint == "" && c.R2Bucket == "" && c.R2AccessKeyID == "" && c.R2SecretAccessKey == "" {
		return nil
	}
	var errs []error
	var missing []string
	for _, v := range []struct{ name, val string }{
		{"R2_ENDPOINT", c.R2Endpoint}, {"R2_BUCKET", c.R2Bucket},
		{"R2_ACCESS_KEY_ID", c.R2AccessKeyID}, {"R2_SECRET_ACCESS_KEY", c.R2SecretAccessKey},
	} {
		if v.val == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("R2_ENDPOINT, R2_BUCKET, R2_ACCESS_KEY_ID and R2_SECRET_ACCESS_KEY must be set together (missing %s)", strings.Join(missing, ", ")))
	}
	if c.R2Endpoint != "" {
		u, err := url.Parse(c.R2Endpoint)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
			errs = append(errs, errors.New("R2_ENDPOINT must be scheme://host[:port] with no path, query or credentials"))
		case c.IsProduction() && (u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), r2EndpointSuffix)):
			errs = append(errs, errors.New("R2_ENDPOINT must be https://<account>"+r2EndpointSuffix+" in production"))
		}
	}
	if c.R2Bucket != "" && !r2BucketRE.MatchString(c.R2Bucket) {
		errs = append(errs, fmt.Errorf("R2_BUCKET: %q is not a valid bucket name", c.R2Bucket))
	}
	return errs
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func boolEnv(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}
