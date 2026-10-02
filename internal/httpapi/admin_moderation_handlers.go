package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

var (
	errAdminEventNotFound = errors.New("admin: event not found")
	errAdminMediaNotFound = errors.New("admin: media not found")
)

// --- reports ---

var validReportStatuses = map[string]bool{
	"open": true, "reviewing": true, "dismissed": true, "taken_down": true, "restored": true,
}

type adminReportEventResp struct {
	ID         uuid.UUID `json:"id"`
	Slug       *string   `json:"slug"`
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	OwnerEmail string    `json:"owner_email"`
}

type adminReportResp struct {
	ID                  uuid.UUID            `json:"id"`
	Event               adminReportEventResp `json:"event"`
	Reason              string               `json:"reason"`
	Details             string               `json:"details"`
	Status              string               `json:"status"`
	OpenReportsForEvent int64                `json:"open_reports_for_event"`
	CreatedAt           time.Time            `json:"created_at"`
}

// GET /v1/admin/reports?status=open&cursor
func (s *Server) handleListAdminReports(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "open"
	}
	if !validReportStatuses[status] {
		writeError(w, http.StatusBadRequest, "validation_failed", "Unknown status filter.")
		return
	}
	limitN := clampLimit(r.URL.Query().Get("limit"), 25, 100)
	createdAt, id, ok := decodeAdminCursor(w, r)
	if !ok {
		return
	}

	rows, err := s.q.ListReportsAdmin(r.Context(), store.ListReportsAdminParams{
		Status: status, CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN),
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list reports admin: %w", err))
		return
	}
	items := make([]adminReportResp, len(rows))
	for i, row := range rows {
		items[i] = adminReportResp{
			ID: row.ID,
			Event: adminReportEventResp{
				ID: row.EventID, Slug: row.EventSlug, Title: row.EventTitle, Status: row.EventStatus,
				OwnerEmail: derefOrEmpty(row.OwnerEmail),
			},
			Reason: row.Reason, Details: row.Details, Status: row.Status,
			OpenReportsForEvent: row.OpenReportsForEvent, CreatedAt: row.CreatedAt,
		}
	}
	var nextCursor *string
	if len(rows) == limitN {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": items, "next_cursor": nextCursor})
}

// POST /v1/admin/reports/{id}/dismiss {note?}
func (s *Server) handleDismissReport(w http.ResponseWriter, r *http.Request) {
	reportID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such report.")
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if !decodeJSONIfPresent(w, r, &body) {
		return
	}
	note, ok := validateAdminReason(body.Note, 500)
	if !ok {
		writeError(w, http.StatusBadRequest, "validation_failed", "Check the note field.")
		return
	}

	callerID, _ := userIDFrom(r.Context())
	ctx := r.Context()
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		row, err := q.DismissReport(ctx, store.DismissReportParams{HandledBy: callerID, ReportID: reportID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAdminReportNotFound
		}
		if err != nil {
			return fmt.Errorf("dismiss report: %w", err)
		}
		after := map[string]string{"status": row.NewStatus}
		if note != "" {
			after["note"] = note
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "report.dismiss", TargetType: "report", TargetID: reportID.String(),
			Before: map[string]string{"status": row.OldStatus}, After: after,
		})
	})
	if err != nil {
		if errors.Is(err, errAdminReportNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such report.")
			return
		}
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

var errAdminReportNotFound = errors.New("admin: report not found")

// --- events ---

var validAdminEventStatuses = map[string]bool{"draft": true, "published": true, "hidden": true, "taken_down": true}

type adminEventSummaryResp struct {
	ID           uuid.UUID  `json:"id"`
	Slug         *string    `json:"slug"`
	Title        string     `json:"title"`
	OccasionSlug string     `json:"occasion_slug"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	PublishedAt  *time.Time `json:"published_at"`
	DeletedAt    *time.Time `json:"deleted_at"`
	OwnerEmail   string     `json:"owner_email"`
}

// GET /v1/admin/events?q&status&cursor&limit
func (s *Server) handleListAdminEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limitN := clampLimit(query.Get("limit"), 25, 100)

	var status *string
	if v := strings.TrimSpace(query.Get("status")); v != "" {
		if !validAdminEventStatuses[v] {
			writeError(w, http.StatusBadRequest, "validation_failed", "Unknown status filter.")
			return
		}
		status = &v
	}

	ctx := r.Context()
	qtext := strings.TrimSpace(query.Get("q"))
	items := []adminEventSummaryResp{}
	var nextCursor *string

	switch {
	case qtext != "":
		prefix, ok := buildPrefixQuery(qtext)
		if !ok {
			break
		}
		createdAt, id, ok := decodeAdminCursor(w, r)
		if !ok {
			return
		}
		rows, err := s.q.SearchEventsAdminText(ctx, store.SearchEventsAdminTextParams{
			PrefixQuery: prefix, Status: status, CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN),
		})
		if err != nil {
			serverError(w, r, fmt.Errorf("search events text: %w", err))
			return
		}
		for _, row := range rows {
			items = append(items, adminEventSummaryResp{
				row.ID, row.Slug, row.Title, row.OccasionSlug, row.Status, row.CreatedAt, row.PublishedAt, row.DeletedAt,
				derefOrEmpty(row.OwnerEmail),
			})
		}
		if len(rows) == limitN {
			nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
			nextCursor = &nc
		}

	case status != nil:
		createdAt, id, ok := decodeAdminCursor(w, r)
		if !ok {
			return
		}
		rows, err := s.q.SearchEventsAdminByStatus(ctx, store.SearchEventsAdminByStatusParams{
			Status: *status, CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN),
		})
		if err != nil {
			serverError(w, r, fmt.Errorf("search events by status: %w", err))
			return
		}
		for _, row := range rows {
			items = append(items, adminEventSummaryResp{
				row.ID, row.Slug, row.Title, row.OccasionSlug, row.Status, row.CreatedAt, row.PublishedAt, row.DeletedAt,
				derefOrEmpty(row.OwnerEmail),
			})
		}
		if len(rows) == limitN {
			nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
			nextCursor = &nc
		}

	default:
		createdAt, id, ok := decodeAdminCursor(w, r)
		if !ok {
			return
		}
		rows, err := s.q.SearchEventsAdmin(ctx, store.SearchEventsAdminParams{CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN)})
		if err != nil {
			serverError(w, r, fmt.Errorf("search events: %w", err))
			return
		}
		for _, row := range rows {
			items = append(items, adminEventSummaryResp{
				row.ID, row.Slug, row.Title, row.OccasionSlug, row.Status, row.CreatedAt, row.PublishedAt, row.DeletedAt,
				derefOrEmpty(row.OwnerEmail),
			})
		}
		if len(rows) == limitN {
			nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
			nextCursor = &nc
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"events": items, "next_cursor": nextCursor})
}

// adminEventResp is the §4.1 Event shape plus the owner's email, used by
// the admin event detail response.
type adminEventResp struct {
	eventResp
	OwnerEmail *string `json:"owner_email"`
}

// buildAdminEventResponse renders GetEventAdmin's row through the same
// content/theme/media pipeline the host-facing event response uses. Role is
// set to "owner" as a placeholder (an admin isn't a member of the event, so
// there's no real role to report, and the admin UI never reads this field);
// LatestVersion falls back to the pinned version itself, since GetEventAdmin
// doesn't compute the template's latest published version the way the
// host-facing queries do — the "update available" hint is a host-editor
// concept and admin has its own explicit template/flags control instead.
func (s *Server) buildAdminEventResponse(ctx context.Context, q *store.Queries, row store.GetEventAdminRow) (adminEventResp, error) {
	base, err := s.buildEventResponse(ctx, q, eventRow{
		Event: row.Event, Role: "owner", TemplateSlug: row.TemplateSlug, TemplateName: row.TemplateName,
		Manifest: row.Manifest, AssetsPath: row.AssetsPath, LatestVersion: row.Event.TemplateVersion,
	})
	if err != nil {
		return adminEventResp{}, err
	}
	return adminEventResp{eventResp: base, OwnerEmail: row.OwnerEmail}, nil
}

// adminEventReportResp is the report shape embedded in the admin event
// detail response: unlike adminReportResp (the moderation queue's per-report
// row), the caller here already has the event, so there's no event/owner
// join and no per-event open-report count to repeat on every row.
type adminEventReportResp struct {
	ID        uuid.UUID  `json:"id"`
	Reason    string     `json:"reason"`
	Details   string     `json:"details"`
	Status    string     `json:"status"`
	HandledBy *uuid.UUID `json:"handled_by"`
	HandledAt *time.Time `json:"handled_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// GET /v1/admin/events/{id}
func (s *Server) handleGetAdminEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	ctx := r.Context()
	row, err := s.q.GetEventAdmin(ctx, eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get event admin: %w", err))
		return
	}
	resp, err := s.buildAdminEventResponse(ctx, s.q, row)
	if err != nil {
		serverError(w, r, fmt.Errorf("build admin event response: %w", err))
		return
	}

	createdAt, id, ok := decodeAdminCursor(w, r)
	if !ok {
		return
	}
	reportRows, err := s.q.ListReportsForEvent(ctx, store.ListReportsForEventParams{
		EventID: eventID, CursorCreatedAt: createdAt, CursorID: id, Lim: 50,
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list reports for event: %w", err))
		return
	}
	reports := make([]adminEventReportResp, len(reportRows))
	for i, rr := range reportRows {
		reports[i] = adminEventReportResp{
			ID: rr.ID, Reason: rr.Reason, Details: rr.Details, Status: rr.Status,
			HandledBy: rr.HandledBy, HandledAt: rr.HandledAt, CreatedAt: rr.CreatedAt,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"event": resp,
		"counts": map[string]int64{
			"rsvps":  row.RsvpCount,
			"guests": row.GuestCount,
			"media":  row.MediaCount,
		},
		"reports": reports,
	})
}

// POST /v1/admin/events/{id}/takedown {reason}
func (s *Server) handleAdminTakedownEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	reason, ok := validateAdminReason(body.Reason, 500)
	if !ok {
		writeError(w, http.StatusBadRequest, "validation_failed", "Check the reason field.")
		return
	}

	callerID, _ := userIDFrom(r.Context())
	ctx := r.Context()
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		row, err := q.TakeDownEvent(ctx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAdminEventNotFound
		}
		if err != nil {
			return fmt.Errorf("take down event: %w", err)
		}
		if _, err := q.ResolveOpenReportsForEvent(ctx, store.ResolveOpenReportsForEventParams{HandledBy: callerID, EventID: eventID}); err != nil {
			return fmt.Errorf("resolve open reports: %w", err)
		}
		if _, err := s.jobs.InsertTx(ctx, tx, jobs.MediaVisibilityArgs{EventID: eventID}, nil); err != nil {
			return fmt.Errorf("enqueue media visibility: %w", err)
		}
		if row.OwnerID != nil {
			if _, err := s.jobs.InsertTx(ctx, tx, jobs.NotifyTakedownArgs{EventID: eventID, Action: "takedown"}, nil); err != nil {
				return fmt.Errorf("enqueue notify takedown: %w", err)
			}
		}
		after := map[string]string{"status": row.NewStatus}
		if reason != "" {
			after["reason"] = reason
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "event.takedown", TargetType: "event", TargetID: eventID.String(),
			Before: map[string]string{"status": row.OldStatus}, After: after,
		})
	})
	if err != nil {
		if errors.Is(err, errAdminEventNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such event.")
			return
		}
		serverError(w, r, err)
		return
	}
	s.respondWithAdminEvent(w, r, eventID)
}

// POST /v1/admin/events/{id}/restore
func (s *Server) handleAdminRestoreEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	callerID, _ := userIDFrom(r.Context())
	ctx := r.Context()
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		row, err := q.RestoreEvent(ctx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAdminEventNotFound
		}
		if err != nil {
			return fmt.Errorf("restore event: %w", err)
		}
		if _, err := q.RestoreReportsForEvent(ctx, store.RestoreReportsForEventParams{HandledBy: callerID, EventID: eventID}); err != nil {
			return fmt.Errorf("restore reports: %w", err)
		}
		// Safe as of the MediaVisibilityWorker rewrite: a restore that lands
		// on "published" no longer bulk-merges pending/quarantine into
		// public/. The worker re-reads each media row's own moderation_status
		// and places it individually (approved -> public, pending -> pending,
		// rejected -> deleted), so an unapproved or rejected guest photo is
		// never exposed just because the event itself is public again.
		if _, err := s.jobs.InsertTx(ctx, tx, jobs.MediaVisibilityArgs{EventID: eventID}, nil); err != nil {
			return fmt.Errorf("enqueue media visibility: %w", err)
		}
		if row.OwnerID != nil {
			if _, err := s.jobs.InsertTx(ctx, tx, jobs.NotifyTakedownArgs{EventID: eventID, Action: "restore"}, nil); err != nil {
				return fmt.Errorf("enqueue notify takedown: %w", err)
			}
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "event.restore", TargetType: "event", TargetID: eventID.String(),
			Before: map[string]string{"status": row.OldStatus}, After: map[string]string{"status": row.NewStatus},
		})
	})
	if err != nil {
		if errors.Is(err, errAdminEventNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such event.")
			return
		}
		serverError(w, r, err)
		return
	}
	s.respondWithAdminEvent(w, r, eventID)
}

// PATCH /v1/admin/events/{id}/flags {remove_branding?, template_slug?}
func (s *Server) handleAdminSetEventFlags(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	var body struct {
		RemoveBranding *bool   `json:"remove_branding"`
		TemplateSlug   *string `json:"template_slug"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	ctx := r.Context()
	var templateID *uuid.UUID
	var templateVersion *int32
	if body.TemplateSlug != nil {
		tpl, err := s.q.GetPublishedTemplateBySlug(ctx, store.GetPublishedTemplateBySlugParams{
			Slug: *body.TemplateSlug, AllowPremium: true,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "unknown_template", "Unknown template.")
			return
		}
		if err != nil {
			serverError(w, r, fmt.Errorf("get published template: %w", err))
			return
		}
		templateID, templateVersion = &tpl.ID, &tpl.Version
	}

	callerID, _ := userIDFrom(r.Context())
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		row, err := q.SetEventFlags(ctx, store.SetEventFlagsParams{
			RemoveBranding: body.RemoveBranding, TemplateID: templateID, TemplateVersion: templateVersion,
			EventID: eventID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAdminEventNotFound
		}
		if err != nil {
			return fmt.Errorf("set event flags: %w", err)
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "event.flags", TargetType: "event", TargetID: eventID.String(),
			Before: map[string]any{"remove_branding": row.OldRemoveBranding, "template_id": row.OldTemplateID, "template_version": row.OldTemplateVersion},
			After:  map[string]any{"remove_branding": row.RemoveBranding, "template_id": row.TemplateID, "template_version": row.TemplateVersion},
		})
	})
	if err != nil {
		if errors.Is(err, errAdminEventNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such event.")
			return
		}
		serverError(w, r, err)
		return
	}
	s.respondWithAdminEvent(w, r, eventID)
}

// respondWithAdminEvent re-reads and renders the event, for handlers that
// mutate it and then answer with its new state (§4.1's "response: {event}").
func (s *Server) respondWithAdminEvent(w http.ResponseWriter, r *http.Request, eventID uuid.UUID) {
	ctx := r.Context()
	row, err := s.q.GetEventAdmin(ctx, eventID)
	if err != nil {
		serverError(w, r, fmt.Errorf("get event admin: %w", err))
		return
	}
	resp, err := s.buildAdminEventResponse(ctx, s.q, row)
	if err != nil {
		serverError(w, r, fmt.Errorf("build admin event response: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": resp})
}

// --- guest media moderation queue ---

var validMediaStatuses = map[string]bool{"pending": true, "approved": true, "rejected": true}

type adminMediaEventResp struct {
	ID    uuid.UUID `json:"id"`
	Slug  *string   `json:"slug"`
	Title string    `json:"title"`
}

type adminMediaItemResp struct {
	ID               uuid.UUID           `json:"id"`
	Src              string              `json:"src"`
	Width            int32               `json:"width"`
	Height           int32               `json:"height"`
	SizeBytes        int64               `json:"size_bytes"`
	UploadedBy       string              `json:"uploaded_by"`
	ModerationStatus string              `json:"moderation_status"`
	GuestName        *string             `json:"guest_name"`
	CreatedAt        time.Time           `json:"created_at"`
	Event            adminMediaEventResp `json:"event"`
}

// GET /v1/admin/media?status&event_id&cursor&limit
func (s *Server) handleListAdminMedia(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	status := query.Get("status")
	if status == "" {
		status = "pending"
	}
	if !validMediaStatuses[status] {
		writeError(w, http.StatusBadRequest, "validation_failed", "Unknown status filter.")
		return
	}
	var eventID *uuid.UUID
	if v := query.Get("event_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "Invalid event_id.")
			return
		}
		eventID = &id
	}
	limitN := clampLimit(query.Get("limit"), 25, 100)
	createdAt, id, ok := decodeAdminCursor(w, r)
	if !ok {
		return
	}

	rows, err := s.q.ListGuestMediaAdmin(r.Context(), store.ListGuestMediaAdminParams{
		Status: status, EventID: eventID, CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN),
	})
	if err != nil {
		serverError(w, r, fmt.Errorf("list guest media admin: %w", err))
		return
	}
	items := make([]adminMediaItemResp, len(rows))
	for i, row := range rows {
		items[i] = adminMediaItemResp{
			ID: row.ID, Src: mediaSrc(row.EventID, row.ID), Width: row.Width, Height: row.Height,
			SizeBytes: row.SizeBytes, UploadedBy: "guest", ModerationStatus: row.ModerationStatus,
			GuestName: row.GuestName, CreatedAt: row.CreatedAt,
			Event: adminMediaEventResp{ID: row.EventID, Slug: row.EventSlug, Title: row.EventTitle},
		}
	}
	var nextCursor *string
	if len(rows) == limitN {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"media": items, "next_cursor": nextCursor})
}

// GET /v1/admin/media/{id}/file?w=480
func (s *Server) handleGetAdminMediaFile(w http.ResponseWriter, r *http.Request) {
	mediaID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such file.")
		return
	}
	width, err := strconv.Atoi(r.URL.Query().Get("w"))
	if err != nil || (width != 480 && width != 1080) {
		writeError(w, http.StatusBadRequest, "validation_failed", "w must be 480 or 1080.")
		return
	}

	ctx := r.Context()
	row, err := s.q.GetMediaAdmin(ctx, mediaID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such file.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get media admin: %w", err))
		return
	}

	f, err := s.media.Open(row.EventID, mediaID, width)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found", "No such file.")
			return
		}
		serverError(w, r, fmt.Errorf("open media file: %w", err))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		serverError(w, r, fmt.Errorf("stat media file: %w", err))
		return
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// POST /v1/admin/media/{id}/reject
//
// Distinct from the host-facing approve/reject pair (media_handlers.go),
// which is scoped to one event a host/editor already has access to: this
// is the global moderation queue, support/moderator-role gated, and only
// exposes reject (rejecting anywhere, without needing event membership).
func (s *Server) handleAdminRejectMedia(w http.ResponseWriter, r *http.Request) {
	mediaID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such media.")
		return
	}
	callerID, _ := userIDFrom(r.Context())
	ctx := r.Context()

	var row store.RejectMediaAdminRow
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		var err error
		row, err = q.RejectMediaAdmin(ctx, mediaID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAdminMediaNotFound
		}
		if err != nil {
			return fmt.Errorf("reject media admin: %w", err)
		}
		// Enqueue the durable takedown (the MediaVisibilityWorker deletes
		// rejected media per-file, re-reading moderation_status) inside the
		// same tx as the reject, so the file is guaranteed to be cleaned up
		// even if the synchronous delete below fails or the process crashes
		// first. That delete is now just best-effort, low-latency cleanup.
		if _, err := s.jobs.InsertTx(ctx, tx, jobs.MediaVisibilityArgs{EventID: row.EventID}, nil); err != nil {
			return fmt.Errorf("enqueue media visibility: %w", err)
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "media.reject", TargetType: "media", TargetID: mediaID.String(),
			Before: map[string]string{"moderation_status": row.OldStatus}, After: map[string]string{"moderation_status": row.NewStatus},
		})
	})
	if err != nil {
		if errors.Is(err, errAdminMediaNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such media.")
			return
		}
		serverError(w, r, err)
		return
	}
	if err := s.media.DeleteMedia(row.EventID, mediaID); err != nil {
		slog.WarnContext(ctx, "delete rejected media files", "err", err, "media_id", mediaID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"media": mediaResp{
		ID: row.ID, Src: mediaSrc(row.EventID, row.ID), UploadedBy: row.UploadedBy, ModerationStatus: row.NewStatus,
	}})
}
