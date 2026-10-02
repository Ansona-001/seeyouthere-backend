package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// parseInt32Param parses an int32 chi URL param (template version numbers).
func parseInt32Param(r *http.Request, name string) (int32, error) {
	n, err := strconv.ParseInt(chi.URLParam(r, name), 10, 32)
	if err != nil {
		return 0, err
	}
	return int32(n), nil
}

// bytesReader adapts a []byte for json.NewDecoder without importing
// bytes.NewReader at every call site.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// maxAdminSearchRune bounds a free-text admin search box, same limit as the
// host-facing guest search (§4.6's q).
const maxAdminSearchRune = 80

// firstPageCursor is the keyset sentinel every admin DESC list (newest
// first) uses for its first page: no real row can be newer than this.
func firstPageCursor() (time.Time, uuid.UUID) {
	return time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), uuid.Max
}

// decodeAdminCursor parses the ?cursor= query param, defaulting to the
// first-page sentinel when absent. It writes a 400 invalid_cursor response
// and returns ok=false on a malformed cursor.
func decodeAdminCursor(w http.ResponseWriter, r *http.Request) (createdAt time.Time, id uuid.UUID, ok bool) {
	createdAt, id = firstPageCursor()
	if c := r.URL.Query().Get("cursor"); c != "" {
		var err error
		createdAt, id, err = decodeCursor(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid cursor.")
			return time.Time{}, uuid.Nil, false
		}
	}
	return createdAt, id, true
}

// decodeJSONIfPresent decodes r's body into dst only when the request
// actually has a JSON body: several admin actions take an entirely optional
// request (dismiss's note, restore, sessions/revoke, role grant/revoke,
// mfa/reset, template version publish) and the frontend's api() helper
// omits Content-Type/body altogether when it has nothing to send, rather
// than sending "{}". Absent means dst keeps its zero value; a body that IS
// present must still be well-formed JSON.
func decodeJSONIfPresent(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Header.Get("Content-Type") == "" {
		return true
	}
	return decodeJSON(w, r, dst)
}

// buildPrefixQuery turns free-text search into a to_tsquery('simple', ...)
// prefix query built entirely from sanitised, alphanumeric-only tokens
// joined with " & " and suffixed ":*", so the caller's text can never be
// read as tsquery syntax (no injection of &, |, !, (, ) operators) even
// though it reaches to_tsquery as a plain, parameterised string argument.
// ok is false when the input has no indexable tokens at all, in which case
// callers fall back to an unfiltered list.
func buildPrefixQuery(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if utf8.RuneCountInString(raw) > maxAdminSearchRune {
		runes := []rune(raw)
		raw = string(runes[:maxAdminSearchRune])
	}
	const maxTokens = 8
	var tokens []string
	for _, field := range strings.Fields(raw) {
		var b strings.Builder
		for _, r := range field {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(unicode.ToLower(r))
			}
		}
		if b.Len() > 0 {
			tokens = append(tokens, b.String()+":*")
		}
		if len(tokens) >= maxTokens {
			break
		}
	}
	if len(tokens) == 0 {
		return "", false
	}
	return strings.Join(tokens, " & "), true
}

// resolveActorEmails looks up the email of every distinct, non-nil actor id
// in ids. It's a small, bounded loop (audit pages are limited to ≤100 rows,
// user-detail pages to 20) rather than a new batch query: this is a
// low-traffic admin-only surface, so the N+1 here trades a little latency
// for not touching internal/database. A missing or soft-deleted actor is
// left out of the map; callers treat that as "unknown"/"system".
func (s *Server) resolveActorEmails(ctx context.Context, ids []*uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		if id == nil || *id == uuid.Nil {
			continue
		}
		if _, ok := out[*id]; ok {
			continue
		}
		u, err := s.q.GetUserByID(ctx, *id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get user %s: %w", *id, err)
		}
		out[*id] = u.Email
	}
	return out, nil
}

// adminAuditEntryResp is the §4.11 audit log shape used by every list/detail
// endpoint that surfaces audit rows.
type adminAuditEntryResp struct {
	ID         uuid.UUID   `json:"id"`
	ActorID    *uuid.UUID  `json:"actor_id"`
	ActorEmail *string     `json:"actor_email"`
	Action     string      `json:"action"`
	TargetType string      `json:"target_type"`
	TargetID   string      `json:"target_id"`
	Before     interface{} `json:"before"`
	After      interface{} `json:"after"`
	IP         *string     `json:"ip"`
	CreatedAt  time.Time   `json:"created_at"`
}

// buildAuditEntries renders raw AuditLog rows into the response shape,
// resolving actor emails in one bounded batch.
func (s *Server) buildAuditEntries(ctx context.Context, rows []store.AuditLog) ([]adminAuditEntryResp, error) {
	ids := make([]*uuid.UUID, len(rows))
	for i, row := range rows {
		ids[i] = row.ActorID
	}
	emails, err := s.resolveActorEmails(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]adminAuditEntryResp, len(rows))
	for i, row := range rows {
		var email *string
		if row.ActorID != nil {
			if e, ok := emails[*row.ActorID]; ok {
				email = &e
			}
		}
		var ip *string
		if row.Ip != nil {
			s := row.Ip.String()
			ip = &s
		}
		out[i] = adminAuditEntryResp{
			ID: row.ID, ActorID: row.ActorID, ActorEmail: email, Action: row.Action,
			TargetType: row.TargetType, TargetID: row.TargetID,
			Before: rawJSONOrNil(row.Before), After: rawJSONOrNil(row.After),
			IP: ip, CreatedAt: row.CreatedAt,
		}
	}
	return out, nil
}

// rawJSONOrNil re-exposes a nullable jsonb column as its parsed value (or
// nil), so audit_log.before/after serialise as real JSON objects instead of
// base64-encoded byte strings.
func rawJSONOrNil(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	return v
}
