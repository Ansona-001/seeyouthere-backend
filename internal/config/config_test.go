package config

import (
	"strings"
	"testing"
)

// setBaseEnv sets every env var Load requires to succeed, so each test
// below only has to vary the RSVP_* ones it's actually testing.
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5433/db")
	t.Setenv("SMTP_ADDR", "smtp.zoho.com:587")
	t.Setenv("AUTH_SECRET", strings.Repeat("a", 32))
	t.Setenv("SITE_URL", "http://localhost:3100")
	t.Setenv("TOTP_KEY", "ZGV2LW9ubHktdG90cC1rZXktY2hhbmdlLW1lLTMyYmI=")
	t.Setenv("APP_ENV", "development")
}

func TestLoad_RSVPSMTP_AllUnset_NoError(t *testing.T) {
	setBaseEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.RSVPSMTPAddr != "" || c.RSVPSMTPUser != "" || c.RSVPSMTPPass != "" || c.RSVPMailFrom != "" {
		t.Errorf("expected all RSVP fields empty, got %+v", c)
	}
}

func TestLoad_RSVPSMTP_PartiallySet_Errors(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"only user":     func(t *testing.T) { t.Setenv("RSVP_SMTP_USER", "u") },
		"only pass":     func(t *testing.T) { t.Setenv("RSVP_SMTP_PASS", "p") },
		"only from":     func(t *testing.T) { t.Setenv("RSVP_MAIL_FROM", "RSVP <rsvp@seeyouthere.at>") },
		"user and pass": func(t *testing.T) { t.Setenv("RSVP_SMTP_USER", "u"); t.Setenv("RSVP_SMTP_PASS", "p") },
	}
	for name, setEnv := range cases {
		t.Run(name, func(t *testing.T) {
			setBaseEnv(t)
			setEnv(t)
			_, err := Load()
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestLoad_RSVPSMTP_AllSet_DefaultsAddr(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("RSVP_SMTP_USER", "u")
	t.Setenv("RSVP_SMTP_PASS", "p")
	t.Setenv("RSVP_MAIL_FROM", "RSVP <rsvp@seeyouthere.at>")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.RSVPSMTPAddr != c.SMTPAddr {
		t.Errorf("RSVPSMTPAddr = %q, want default to SMTPAddr %q", c.RSVPSMTPAddr, c.SMTPAddr)
	}
}

func TestLoad_RSVPSMTP_ExplicitAddrKept(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("RSVP_SMTP_USER", "u")
	t.Setenv("RSVP_SMTP_PASS", "p")
	t.Setenv("RSVP_MAIL_FROM", "RSVP <rsvp@seeyouthere.at>")
	t.Setenv("RSVP_SMTP_ADDR", "smtp.zoho.eu:587")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.RSVPSMTPAddr != "smtp.zoho.eu:587" {
		t.Errorf("RSVPSMTPAddr = %q, want explicit value kept", c.RSVPSMTPAddr)
	}
}

func TestLoad_RSVPSMTP_InvalidMailFrom_Errors(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("RSVP_SMTP_USER", "u")
	t.Setenv("RSVP_SMTP_PASS", "p")
	t.Setenv("RSVP_MAIL_FROM", "not-an-address")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for invalid RSVP_MAIL_FROM")
	}
}
