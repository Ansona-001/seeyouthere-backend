package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// auditEntry is one row for audit_log. Before/After are marshalled to JSON
// as-is (nil marshals to a NULL column); handlers pass whatever
// before/after shape makes sense for that action (often a *Row struct a
// CTE-based query already returned).
type auditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Before     any
	After      any
}

// writeAudit inserts one audit_log row. Called inside the same transaction
// as the mutation it records, using the tx-bound q the caller already has
// (never s.q directly), so the audit trail can never end up out of sync
// with the change it describes.
func (s *Server) writeAudit(ctx context.Context, q *store.Queries, r *http.Request, a auditEntry) error {
	before, err := marshalAuditValue(a.Before)
	if err != nil {
		return fmt.Errorf("audit: marshal before: %w", err)
	}
	after, err := marshalAuditValue(a.After)
	if err != nil {
		return fmt.Errorf("audit: marshal after: %w", err)
	}

	var ip *netip.Addr
	if addr := clientIPFrom(r.Context()); addr.IsValid() {
		ip = &addr
	}

	return q.InsertAuditLog(ctx, store.InsertAuditLogParams{
		ID:         uuid.Must(uuid.NewV7()),
		ActorID:    a.ActorID,
		Action:     a.Action,
		TargetType: a.TargetType,
		TargetID:   a.TargetID,
		Before:     before,
		After:      after,
		Ip:         ip,
	})
}

func marshalAuditValue(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
