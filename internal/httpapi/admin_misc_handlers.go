package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// --- slug blocklist ---

// blockTermRe matches §4.11's term shape.
var blockTermRe = regexp.MustCompile(`^[a-z0-9-]{2,50}$`)

type slugBlocklistEntryResp struct {
	Term   string `json:"term"`
	Reason string `json:"reason"`
}

// GET /v1/admin/slug-blocklist?cursor
func (s *Server) handleListSlugBlocklist(w http.ResponseWriter, r *http.Request) {
	limitN := clampLimit(r.URL.Query().Get("limit"), 25, 100)
	after := r.URL.Query().Get("cursor")

	rows, err := s.q.ListBlocklist(r.Context(), store.ListBlocklistParams{AfterTerm: after, Lim: int32(limitN)})
	if err != nil {
		serverError(w, r, fmt.Errorf("list blocklist: %w", err))
		return
	}
	items := make([]slugBlocklistEntryResp, len(rows))
	for i, row := range rows {
		items[i] = slugBlocklistEntryResp{Term: row.Term, Reason: row.Reason}
	}
	var nextCursor *string
	if len(rows) == limitN {
		nc := rows[len(rows)-1].Term
		nextCursor = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"terms": items, "next_cursor": nextCursor})
}

// POST /v1/admin/slug-blocklist {term, reason}
func (s *Server) handleAddSlugBlockTerm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Term   string `json:"term"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !blockTermRe.MatchString(body.Term) {
		writeError(w, http.StatusBadRequest, "validation_failed", "Term must be 2-50 lower-case letters, digits and hyphens.")
		return
	}
	reason, ok := validateAdminReason(body.Reason, 200)
	if !ok || reason == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "Reason is required.")
		return
	}

	callerID, _ := userIDFrom(r.Context())
	ctx := r.Context()
	err := s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		n, err := q.AddBlockTerm(ctx, store.AddBlockTermParams{Term: body.Term, Reason: reason})
		if err != nil {
			return fmt.Errorf("add block term: %w", err)
		}
		if n == 0 {
			return nil // already present; idempotent
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "slug_blocklist.add", TargetType: "slug_blocklist", TargetID: body.Term,
			After: slugBlocklistEntryResp{Term: body.Term, Reason: reason},
		})
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, slugBlocklistEntryResp{Term: body.Term, Reason: reason})
}

// DELETE /v1/admin/slug-blocklist/{term}
func (s *Server) handleDeleteSlugBlockTerm(w http.ResponseWriter, r *http.Request) {
	term := chi.URLParam(r, "term")
	callerID, _ := userIDFrom(r.Context())
	ctx := r.Context()

	err := s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		row, err := q.DeleteBlockTerm(ctx, term)
		if errors.Is(err, pgx.ErrNoRows) {
			return errBlockTermNotFound
		}
		if err != nil {
			return fmt.Errorf("delete block term: %w", err)
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "slug_blocklist.remove", TargetType: "slug_blocklist", TargetID: term,
			Before: slugBlocklistEntryResp{Term: row.Term, Reason: row.Reason},
		})
	})
	if err != nil {
		if errors.Is(err, errBlockTermNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such term.")
			return
		}
		serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errBlockTermNotFound = errors.New("admin: block term not found")

// --- audit log ---

// GET /v1/admin/audit?actor_id&target_type&target_id&cursor&limit
func (s *Server) handleListAdminAudit(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limitN := clampLimit(query.Get("limit"), 25, 100)
	createdAt, id, ok := decodeAdminCursor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var rows []store.AuditLog
	var err error
	switch {
	case query.Get("actor_id") != "":
		actorID, parseErr := uuid.Parse(query.Get("actor_id"))
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "Invalid actor_id.")
			return
		}
		rows, err = s.q.ListAuditByActor(ctx, store.ListAuditByActorParams{
			ActorID: actorID, CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN),
		})
	case query.Get("target_type") != "" && query.Get("target_id") != "":
		rows, err = s.q.ListAuditForTarget(ctx, store.ListAuditForTargetParams{
			TargetType: query.Get("target_type"), TargetID: query.Get("target_id"),
			CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN),
		})
	default:
		rows, err = s.q.ListAuditLog(ctx, store.ListAuditLogParams{CursorCreatedAt: createdAt, CursorID: id, Lim: int32(limitN)})
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("list audit log: %w", err))
		return
	}

	entries, err := s.buildAuditEntries(ctx, rows)
	if err != nil {
		serverError(w, r, err)
		return
	}
	var nextCursor *string
	if len(rows) == limitN {
		nc := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next_cursor": nextCursor})
}
