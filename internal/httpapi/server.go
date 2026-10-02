// Package httpapi exposes the JSON API under /v1.
package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ansonarose/seeyouthere-backend/internal/auth"
	"github.com/ansonarose/seeyouthere-backend/internal/config"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
	"github.com/ansonarose/seeyouthere-backend/internal/passhash"
	"github.com/ansonarose/seeyouthere-backend/internal/ratelimit"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
	"github.com/ansonarose/seeyouthere-backend/internal/token"
	"github.com/ansonarose/seeyouthere-backend/internal/totp"
)

// passwordHashConcurrency and imageProcessConcurrency bound the two
// CPU/RAM-heavy operations on a shared 1 vCPU host (decisions 2 and 8 in
// the build-out plan): at most this many Argon2id/image jobs run at once,
// everyone else queues or gets a 503.
const (
	passwordHashConcurrency = 2
	imageProcessConcurrency = 1
)

type Server struct {
	cfg      config.Config
	pool     *pgxpool.Pool
	rdb      *redis.Client
	q        *store.Queries
	jobs     *jobs.Client
	codes    *auth.Codes
	sessions *auth.Sessions
	limiter  *ratelimit.Limiter

	tokens *token.Keys
	hasher *passhash.Hasher
	totp   *totp.Sealer
	media  *media.Store
	images *media.Processor
}

// NewServer wires the API's dependencies. It can fail: deriving token
// subkeys, building the TOTP sealer and preparing MEDIA_ROOT are all
// checked at startup rather than on the first request that needs them.
func NewServer(cfg config.Config, pool *pgxpool.Pool, rdb *redis.Client, jobClient *jobs.Client) (*Server, error) {
	q := store.New(pool)

	tokens, err := token.NewKeys(cfg.AuthSecret)
	if err != nil {
		return nil, fmt.Errorf("httpapi: %w", err)
	}
	sealer, err := totp.NewSealer(cfg.TOTPKey)
	if err != nil {
		return nil, fmt.Errorf("httpapi: %w", err)
	}
	mediaStore, err := media.NewStore(cfg.MediaRoot)
	if err != nil {
		return nil, fmt.Errorf("httpapi: %w", err)
	}

	return &Server{
		cfg:      cfg,
		pool:     pool,
		rdb:      rdb,
		q:        q,
		jobs:     jobClient,
		codes:    auth.NewCodes(rdb, cfg.AuthSecret),
		sessions: auth.NewSessions(q, rdb, cfg.SessionTTL),
		limiter:  ratelimit.New(rdb),
		tokens:   tokens,
		hasher:   passhash.New(passwordHashConcurrency),
		totp:     sealer,
		media:    mediaStore,
		images:   media.NewProcessor(imageProcessConcurrency),
	}, nil
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.clientIP)
	r.Use(logRequests)
	r.Use(middleware.Recoverer)
	r.Use(s.cors)
	r.Use(s.csrf)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.handleReady)

	r.Route("/v1", func(r chi.Router) {
		// The default request deadline. Routes that need longer (uploads,
		// CSV export) or their own ResponseController deadlines register
		// in a sibling group without this middleware instead of here.
		r.Use(middleware.Timeout(15 * time.Second))
		r.Use(secureHeaders)
		r.Use(s.loadSession)

		r.Post("/auth/code", s.handleRequestCode)
		r.Post("/auth/verify", s.handleVerifyCode)
		r.Post("/auth/logout", s.handleLogout)

		r.Group(func(r chi.Router) {
			r.Use(requireUser)
			r.Get("/me", s.handleMe)
			r.Patch("/me", s.handlePatchMe)
			r.Delete("/me", s.handleDeleteAccount)
			r.Route("/me/sessions", func(r chi.Router) {
				r.Get("/", s.handleListSessions)
				r.Post("/revoke-others", s.handleRevokeOtherSessions)
				r.Delete("/{id}", s.handleDeleteSession)
			})
		})

		r.Get("/occasions", s.handleListOccasions)
		r.Get("/templates", s.handleListTemplates)

		// Events and drafts: most of these accept either a session or an
		// anonymous draft cookie (§4.3's "S/D" auth column), so auth is
		// checked inside each handler rather than with requireUser here.
		r.Route("/events", func(r chi.Router) {
			r.Post("/", s.handleCreateEvent)
			r.Get("/", s.handleListEvents)
			r.Route("/{id}", func(r chi.Router) {
				r.Get("/", s.handleGetEvent)
				r.Patch("/", s.handlePatchEvent)
				r.Delete("/", s.handleDeleteEvent)
				r.Post("/template/upgrade", s.handleUpgradeEventTemplate)
				r.Patch("/settings", s.handleUpdateEventSettings)
				r.Post("/publish", s.handlePublishEvent)
				r.Post("/unpublish", s.handleUnpublishEvent)

				// Upload (POST /media) is registered separately, below,
				// outside this group's 15s middleware.Timeout: it needs its
				// own, much longer deadline (extendUploadDeadline).
				r.Route("/media", func(r chi.Router) {
					r.Get("/", s.handleListEventMedia)
					r.Route("/{mediaID}", func(r chi.Router) {
						r.Get("/file", s.handleGetEventMediaFile)
						r.Post("/approve", s.handleApproveMedia)
						r.Post("/reject", s.handleRejectMedia)
						r.Delete("/", s.handleDeleteEventMedia)
					})
				})

				r.Route("/guests", func(r chi.Router) {
					r.Get("/", s.handleListGuests)
					r.Post("/", s.handleCreateGuest)
					r.Post("/import", s.handleImportGuests)
					r.Post("/send-invites", s.handleSendInvites)
					r.Route("/{guestID}", func(r chi.Router) {
						r.Patch("/", s.handleUpdateGuest)
						r.Delete("/", s.handleDeleteGuest)
						r.Post("/rotate-link", s.handleRotateGuestToken)
					})
				})

				r.Route("/members", func(r chi.Router) {
					r.Get("/", s.handleListMembers)
					r.Post("/", s.handleAddMember)
					r.Route("/{userID}", func(r chi.Router) {
						r.Patch("/", s.handleUpdateMemberRole)
						r.Delete("/", s.handleDeleteMember)
					})
				})

				// CSV export (GET /rsvps.csv) is registered separately,
				// below, outside this group's 15s middleware.Timeout: it
				// needs its own 60s deadline (extendExportDeadline).
				r.Route("/rsvps", func(r chi.Router) {
					r.Get("/", s.handleListRSVPs)
					r.Get("/summary", s.handleRSVPSummary)
					r.Delete("/{rsvpID}", s.handleDeleteRSVP)
				})
			})
		})
		r.Get("/slugs/{slug}", s.handleCheckSlug)

		// Public event page and its gate (§4.4): no session required, and
		// none of these routes read one either (a signed-in owner still
		// previews through /v1/events/{id}).
		r.Route("/public", func(r chi.Router) {
			r.Route("/events/{slug}", func(r chi.Router) {
				r.Get("/", s.handleGetPublicEvent)
				r.Get("/photos", s.handleListPublicPhotos)
				r.Post("/unlock", s.handleUnlockPublicEvent)
				r.Post("/reports", s.handleCreateReport)
				r.Put("/rsvp", s.handlePutPublicRSVP)
			})
			r.Post("/invites/accept", s.handleAcceptInvite)
			r.Post("/rsvp-links/accept", s.handleAcceptRSVPLink)
		})

		// Admin (§4.11). MFA enrol/verify need a session and a role but
		// deliberately not the MFA step-up itself (a user who isn't
		// verified yet must still be able to reach them), so they check
		// roles themselves via loadAdminContext instead of requireAdmin.
		// Every other route sits behind requireAdmin(minRole), which also
		// enforces the ADMIN_MFA_TTL step-up. Template version background
		// upload is registered later, outside the 15s timeout group, next
		// to the other raw-body upload routes.
		r.Route("/admin", func(r chi.Router) {
			r.Post("/mfa/enroll", s.handleAdminMFAEnroll)
			r.Post("/mfa/verify", s.handleAdminMFAVerify)

			r.Group(func(r chi.Router) {
				r.Use(s.requireAdmin("support"))
				r.Get("/overview", s.handleAdminOverview)
				r.Get("/users", s.handleListAdminUsers)
				r.Get("/users/{id}", s.handleGetAdminUser)
				r.Post("/users/{id}/sessions/revoke", s.handleAdminRevokeUserSessions)
				r.Get("/events", s.handleListAdminEvents)
				r.Get("/events/{id}", s.handleGetAdminEvent)
			})

			r.Group(func(r chi.Router) {
				r.Use(s.requireAdmin("moderator"))
				// POST .../status is moderator-gated here; ban is
				// additionally restricted to super_admin inside the handler.
				r.Post("/users/{id}/status", s.handleSetAdminUserStatus)
				r.Get("/reports", s.handleListAdminReports)
				r.Post("/reports/{id}/dismiss", s.handleDismissReport)
				r.Post("/events/{id}/takedown", s.handleAdminTakedownEvent)
				r.Post("/events/{id}/restore", s.handleAdminRestoreEvent)
				r.Get("/media", s.handleListAdminMedia)
				r.Get("/media/{id}/file", s.handleGetAdminMediaFile)
				r.Post("/media/{id}/reject", s.handleAdminRejectMedia)
				r.Route("/slug-blocklist", func(r chi.Router) {
					r.Get("/", s.handleListSlugBlocklist)
					r.Post("/", s.handleAddSlugBlockTerm)
					r.Delete("/{term}", s.handleDeleteSlugBlockTerm)
				})
				r.Get("/audit", s.handleListAdminAudit)
			})

			r.Group(func(r chi.Router) {
				r.Use(s.requireAdmin("super_admin"))
				r.Post("/users/{id}/roles/{role}", s.handleAdminUserRole)
				r.Delete("/users/{id}/roles/{role}", s.handleAdminUserRole)
				r.Post("/users/{id}/mfa/reset", s.handleAdminResetUserMFA)
				r.Patch("/events/{id}/flags", s.handleAdminSetEventFlags)
				r.Route("/templates", func(r chi.Router) {
					r.Get("/", s.handleListAdminTemplates)
					r.Post("/", s.handleCreateAdminTemplate)
					r.Route("/{id}", func(r chi.Router) {
						r.Get("/", s.handleGetAdminTemplate)
						r.Patch("/", s.handleUpdateAdminTemplate)
						r.Post("/versions", s.handleCreateAdminTemplateVersion)
						r.Route("/versions/{v}", func(r chi.Router) {
							r.Put("/", s.handleUpdateAdminTemplateVersion)
							r.Post("/publish", s.handleAdminPublishTemplateVersion)
							r.Get("/preview", s.handleAdminPreviewTemplateVersion)
						})
					})
				})
			})
		})
	})

	// Upload routes: their own deadline instead of the 15s /v1 group timeout
	// (§6, build-out plan). Same secureHeaders/loadSession stack as the /v1
	// group, applied directly since they sit outside it.
	r.Group(func(r chi.Router) {
		r.Use(secureHeaders, s.loadSession, extendUploadDeadline)
		r.Post("/v1/events/{id}/media", s.handleUploadEventMedia)
		r.Post("/v1/public/events/{slug}/photos", s.handleUploadGuestPhoto)
		r.With(s.requireAdmin("super_admin")).Post("/v1/admin/templates/{id}/versions/{v}/background", s.handleUploadAdminTemplateBackground)
	})

	// CSV export: its own 60s deadline instead of the 15s /v1 group timeout
	// (§4.5, build-out plan).
	r.Group(func(r chi.Router) {
		r.Use(secureHeaders, s.loadSession, extendExportDeadline)
		r.Get("/v1/events/{id}/rsvps.csv", s.handleExportRSVPsCSV)
	})

	// Dev convenience only: production serves /media/* from Caddy, straight
	// off the volume, before requests ever reach the API.
	if !s.cfg.IsProduction() {
		r.Get("/media/*", s.devMediaHandler())
	}

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "No such endpoint.")
	})
	return r
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "Database unavailable.")
		return
	}
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "Valkey unavailable.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
