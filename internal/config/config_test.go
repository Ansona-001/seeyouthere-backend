package config

import (
	"fmt"
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

func setR2Env(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("R2_ENDPOINT", endpoint)
	t.Setenv("R2_BUCKET", "syt-media")
	t.Setenv("R2_ACCESS_KEY_ID", "akid")
	t.Setenv("R2_SECRET_ACCESS_KEY", "s3cr3t-do-not-leak")
}

func TestLoad_R2(t *testing.T) {
	const prodEP = "https://abc123.r2.cloudflarestorage.com"
	cases := []struct {
		name    string
		env     string
		setup   func(t *testing.T)
		wantErr bool
		wantR2  bool
	}{
		{"none set", "development", func(t *testing.T) {}, false, false},
		{"all set dev", "development", func(t *testing.T) { setR2Env(t, "http://127.0.0.1:9000") }, false, true},
		{"all set prod", "production", func(t *testing.T) { setR2Env(t, prodEP) }, false, true},
		{"trailing slash ok", "production", func(t *testing.T) { setR2Env(t, prodEP+"/") }, false, true},
		{"only endpoint", "development", func(t *testing.T) { t.Setenv("R2_ENDPOINT", prodEP) }, true, false},
		{"only secret", "development", func(t *testing.T) { t.Setenv("R2_SECRET_ACCESS_KEY", "x") }, true, false},
		{"missing key id", "development", func(t *testing.T) {
			setR2Env(t, prodEP)
			t.Setenv("R2_ACCESS_KEY_ID", "")
		}, true, false},
		{"prod http", "production", func(t *testing.T) { setR2Env(t, "http://abc123.r2.cloudflarestorage.com") }, true, false},
		{"prod wrong host", "production", func(t *testing.T) { setR2Env(t, "https://example.com") }, true, false},
		{"prod suffix trick", "production", func(t *testing.T) { setR2Env(t, "https://r2.cloudflarestorage.com.evil.test") }, true, false},
		{"path", "development", func(t *testing.T) { setR2Env(t, "http://localhost:9000/bucket") }, true, false},
		{"query", "development", func(t *testing.T) { setR2Env(t, "http://localhost:9000?x=1") }, true, false},
		{"userinfo", "development", func(t *testing.T) { setR2Env(t, "http://u:p@localhost:9000") }, true, false},
		{"bad scheme", "development", func(t *testing.T) { setR2Env(t, "ftp://localhost") }, true, false},
		{"bad bucket upper", "development", func(t *testing.T) {
			setR2Env(t, "http://localhost:9000")
			t.Setenv("R2_BUCKET", "Bucket")
		}, true, false},
		{"bad bucket short", "development", func(t *testing.T) {
			setR2Env(t, "http://localhost:9000")
			t.Setenv("R2_BUCKET", "ab")
		}, true, false},
		{"bad bucket dash edge", "development", func(t *testing.T) {
			setR2Env(t, "http://localhost:9000")
			t.Setenv("R2_BUCKET", "-abc")
		}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv("APP_ENV", tc.env)
			if tc.env == "production" {
				t.Setenv("SMTP_USER", "u")
				t.Setenv("SMTP_PASS", "p")
			}
			tc.setup(t)
			c, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "s3cr3t-do-not-leak") {
				t.Errorf("error leaks secret: %v", err)
			}
			if err == nil && c.R2Configured() != tc.wantR2 {
				t.Errorf("R2Configured() = %v, want %v", c.R2Configured(), tc.wantR2)
			}
		})
	}
}

func TestConfig_StringRedactsSecret(t *testing.T) {
	c := Config{R2SecretAccessKey: "s3cr3t-do-not-leak"}
	for _, s := range []string{c.String(), fmt.Sprintf("%v %+v %#v", c, c, c)} {
		if strings.Contains(s, "s3cr3t-do-not-leak") {
			t.Errorf("output leaks secret: %s", s)
		}
	}
}

func TestLoad_MediaQuotas(t *testing.T) {
	cases := []struct {
		name, user, total string
		wantErr           bool
		wantUser, wantTot int64
	}{
		{"defaults", "", "", false, 200_000_000, 9_000_000_000},
		{"custom", "50", "500", false, 50_000_000, 500_000_000},
		{"equal", "100", "100", false, 100_000_000, 100_000_000},
		{"user zero", "0", "", true, 0, 0},
		{"user too big", "1000001", "10000000", true, 0, 0},
		{"user nan", "abc", "", true, 0, 0},
		{"total too small", "50", "99", true, 0, 0},
		{"total too big", "", "10000001", true, 0, 0},
		{"total below user", "500", "400", true, 0, 0},
		{"total nan", "", "1.5", true, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv("MEDIA_USER_QUOTA_MB", tc.user)
			t.Setenv("MEDIA_TOTAL_QUOTA_MB", tc.total)
			c, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (c.MediaUserQuotaBytes != tc.wantUser || c.MediaTotalQuotaBytes != tc.wantTot) {
				t.Errorf("quotas = %d/%d, want %d/%d", c.MediaUserQuotaBytes, c.MediaTotalQuotaBytes, tc.wantUser, tc.wantTot)
			}
		})
	}
}
