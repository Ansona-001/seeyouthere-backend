// Package config loads API settings from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
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
