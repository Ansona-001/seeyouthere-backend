// Command api runs the See You There HTTP API and background job workers.
// Run with no arguments to serve; run `api grant-role <email> <role>` or
// `api reset-mfa <email>` for one-off admin bootstrapping against the
// configured database, with no HTTP listener or job workers started.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	netmail "net/mail"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed the IANA database: distroless has no /usr/share/zoneinfo

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/config"
	"github.com/ansonarose/seeyouthere-backend/internal/database"
	"github.com/ansonarose/seeyouthere-backend/internal/httpapi"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/mail"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// A Zoho alias is a legitimate way to send RSVP_MAIL_FROM from a
	// different address than the authenticating RSVP_SMTP_USER, so this is
	// a warning, not a config error — but a typo'd alias is exactly what
	// produced the 553 relay error the primary SMTP identity hit earlier.
	if cfg.RSVPMailFrom != "" {
		if addr, err := netmail.ParseAddress(cfg.RSVPMailFrom); err == nil && !strings.EqualFold(addr.Address, cfg.RSVPSMTPUser) {
			slog.Warn("RSVP_MAIL_FROM address differs from RSVP_SMTP_USER; confirm it's a verified alias or Zoho will reject it with a 553")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}

	if len(args) > 0 {
		return runCLI(ctx, pool, args)
	}
	return runServer(ctx, cfg, pool)
}

func runServer(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) error {
	redisOpts, err := redis.ParseURL(cfg.ValkeyURL)
	if err != nil {
		return fmt.Errorf("VALKEY_URL: %w", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping valkey: %w", err)
	}

	mediaStore, err := media.NewStore(cfg.MediaRoot)
	if err != nil {
		return fmt.Errorf("media store: %w", err)
	}

	tokens, err := token.NewKeys(cfg.AuthSecret)
	if err != nil {
		return fmt.Errorf("token keys: %w", err)
	}

	sender := &mail.SMTPSender{Addr: cfg.SMTPAddr, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.MailFrom}
	// calendarSender is a separate SMTP identity for yes-RSVP calendar-invite
	// confirmations when RSVP_MAIL_FROM is configured; otherwise it's the
	// same sender used for every other email.
	var calendarSender mail.Sender = sender
	if cfg.RSVPMailFrom != "" {
		calendarSender = &mail.SMTPSender{Addr: cfg.RSVPSMTPAddr, User: cfg.RSVPSMTPUser, Pass: cfg.RSVPSMTPPass, From: cfg.RSVPMailFrom}
	}
	jobClient, err := jobs.NewClient(pool, sender, calendarSender, store.New(pool), mediaStore, tokens, ratelimit.New(rdb), cfg.AdminAlertEmail, cfg.SiteURL)
	if err != nil {
		return fmt.Errorf("river client: %w", err)
	}
	if err := jobClient.Start(ctx); err != nil {
		return fmt.Errorf("start river: %w", err)
	}

	srv, err := httpapi.NewServer(cfg, pool, rdb, jobClient)
	if err != nil {
		return fmt.Errorf("httpapi: %w", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	httpErr := httpSrv.Shutdown(shutdownCtx)
	riverErr := jobClient.Stop(shutdownCtx)
	return errors.Join(httpErr, riverErr)
}

// adminRoles are the roles grant-role may assign (§4.11).
var adminRoles = map[string]bool{"support": true, "moderator": true, "super_admin": true}

func runCLI(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	switch args[0] {
	case "grant-role":
		if len(args) != 3 {
			return errors.New("usage: api grant-role <email> <role>")
		}
		return cliGrantRole(ctx, pool, args[1], args[2])
	case "reset-mfa":
		if len(args) != 2 {
			return errors.New("usage: api reset-mfa <email>")
		}
		return cliResetMFA(ctx, pool, args[1])
	default:
		return fmt.Errorf("unknown command %q (want grant-role or reset-mfa)", args[0])
	}
}

// cliFindUserByEmail is the exact-address lookup shared by both
// subcommands. It deliberately does not create a user: an admin should
// already have an account (from logging in once) before being granted a
// role or having MFA reset, so a typo'd email fails loudly instead of
// silently provisioning a phantom account.
func cliFindUserByEmail(ctx context.Context, q *store.Queries, email string) (uuid.UUID, error) {
	rows, err := q.SearchUsersAdminByEmail(ctx, store.SearchUsersAdminByEmailParams{Email: email})
	if err != nil {
		return uuid.Nil, fmt.Errorf("find user: %w", err)
	}
	if len(rows) == 0 {
		return uuid.Nil, fmt.Errorf("no user with email %q (they must log in at least once first)", email)
	}
	return rows[0].ID, nil
}

// cliGrantRole grants role to the user with email, auditing the change
// with actor_id NULL to mark it as an operator action, not an in-app one.
func cliGrantRole(ctx context.Context, pool *pgxpool.Pool, email, role string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return errors.New("email is required")
	}
	if !adminRoles[role] {
		return fmt.Errorf("unknown role %q (want support, moderator or super_admin)", role)
	}

	q := store.New(pool)
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		qtx := q.WithTx(tx)
		userID, err := cliFindUserByEmail(ctx, qtx, email)
		if err != nil {
			return err
		}
		n, err := qtx.GrantRole(ctx, store.GrantRoleParams{UserID: userID, Role: role})
		if err != nil {
			return fmt.Errorf("grant role: %w", err)
		}
		if n == 0 {
			slog.Info("role already held", "user_id", userID, "role", role)
			return nil
		}
		after, _ := json.Marshal(map[string]string{"role": role})
		if err := qtx.InsertAuditLog(ctx, store.InsertAuditLogParams{
			ID:         uuid.Must(uuid.NewV7()),
			Action:     "cli.role_grant",
			TargetType: "user",
			TargetID:   userID.String(),
			After:      after,
		}); err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		slog.Info("granted role", "user_id", userID, "role", role)
		return nil
	})
}

// cliResetMFA clears the user's TOTP secret and every session's MFA
// step-up, for when an admin has lost their authenticator.
func cliResetMFA(ctx context.Context, pool *pgxpool.Pool, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return errors.New("email is required")
	}

	q := store.New(pool)
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		qtx := q.WithTx(tx)
		userID, err := cliFindUserByEmail(ctx, qtx, email)
		if err != nil {
			return err
		}
		if _, err := qtx.ResetUserTOTP(ctx, userID); err != nil {
			return fmt.Errorf("reset totp: %w", err)
		}
		if err := qtx.InsertAuditLog(ctx, store.InsertAuditLogParams{
			ID:         uuid.Must(uuid.NewV7()),
			Action:     "cli.mfa_reset",
			TargetType: "user",
			TargetID:   userID.String(),
		}); err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		slog.Info("reset mfa", "user_id", userID)
		return nil
	})
}
