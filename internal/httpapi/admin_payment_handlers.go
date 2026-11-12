package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

var (
	errPaymentNotPremium  = errors.New("admin: template is not premium")
	errPaymentAlreadyPaid = errors.New("admin: template already paid for this event")
	errPaymentNotFound    = errors.New("admin: payment not found")
	errPaymentTemplate    = errors.New("admin: event moved to another template")
	errPaymentRegion      = errors.New("admin: region not offered for template")
	errPaymentPrice       = errors.New("admin: confirmed amount is not the current price")
)

// isPriceRegion reports whether region is one a premium template can be sold in.
func isPriceRegion(region string) bool {
	return region == "IN" || region == "AE" || region == "INT"
}

// maxPaymentNoteRunes caps the free-text note recorded with a manual payment.
const maxPaymentNoteRunes = 200

// adminPaymentResp is one recorded premium-template unlock.
type adminPaymentResp struct {
	TemplateID   uuid.UUID `json:"template_id"`
	TemplateName string    `json:"template_name"`
	Region       string    `json:"region"`
	Currency     string    `json:"currency"`
	AmountMinor  int32     `json:"amount_minor"`
	Note         string    `json:"note"`
	CreatedAt    time.Time `json:"created_at"`
}

func newAdminPaymentResp(p store.EventPayment, templateName string) adminPaymentResp {
	return adminPaymentResp{
		TemplateID: p.TemplateID, TemplateName: templateName, Region: p.Region,
		Currency: p.Currency, AmountMinor: p.AmountMinor, Note: p.Note, CreatedAt: p.CreatedAt,
	}
}

// adminPaymentResps renders payments with their template names. Names come
// from one catalog query (the catalog is small); a template missing from it
// is looked up individually so a name is never dropped.
func (s *Server) adminPaymentResps(ctx context.Context, payments []store.EventPayment) ([]adminPaymentResp, error) {
	out := make([]adminPaymentResp, 0, len(payments))
	if len(payments) == 0 {
		return out, nil
	}
	rows, err := s.q.ListTemplatesAdmin(ctx)
	if err != nil {
		return nil, fmt.Errorf("list templates admin: %w", err)
	}
	names := make(map[uuid.UUID]string, len(rows))
	for _, t := range rows {
		names[t.ID] = t.Name
	}
	for _, p := range payments {
		name, ok := names[p.TemplateID]
		if !ok {
			t, err := s.q.GetTemplateAdmin(ctx, p.TemplateID)
			if err != nil {
				return nil, fmt.Errorf("get template admin: %w", err)
			}
			name = t.Name
			names[p.TemplateID] = name
		}
		out = append(out, newAdminPaymentResp(p, name))
	}
	return out, nil
}

// POST /v1/admin/events/{id}/payments {template_id, region, amount_minor, note?}
//
// handleAdminGrantEventPayment records an off-platform payment that unlocks
// the event's CURRENT premium template for publishing, priced in one region.
// The caller names the template so a grant can't land on a template the event
// has since switched away from. The caller also names the amount_minor they
// confirmed; it is recorded only if it equals the live price for the region
// (else 409 price_changed). Amount and currency recorded come from the DB. The
// event row is share-locked so a concurrent template switch waits. Grant and
// audit row share one transaction.
func (s *Server) handleAdminGrantEventPayment(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
		return
	}
	var body struct {
		TemplateID  *string    `json:"template_id"`
		Region      *string    `json:"region"`
		AmountMinor priceField `json:"amount_minor"`
		Note        *string    `json:"note"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	var issues []content.Issue
	var templateID uuid.UUID
	switch {
	case body.TemplateID == nil || *body.TemplateID == "":
		issues = append(issues, content.Issue{Path: "template_id", Code: "required", Message: "This field is required."})
	default:
		if templateID, err = uuid.Parse(*body.TemplateID); err != nil {
			issues = append(issues, content.Issue{Path: "template_id", Code: "invalid", Message: "Must be a template id."})
		}
	}
	region := ""
	switch {
	case body.Region == nil || *body.Region == "":
		issues = append(issues, content.Issue{Path: "region", Code: "required", Message: "This field is required."})
	case !isPriceRegion(*body.Region):
		issues = append(issues, content.Issue{Path: "region", Code: "invalid", Message: "Must be IN, AE or INT."})
	default:
		region = *body.Region
	}
	expected, _ := body.AmountMinor.value()
	if !body.AmountMinor.set || body.AmountMinor.null {
		issues = append(issues, content.Issue{Path: "amount_minor", Code: "required", Message: "This field is required."})
	} else {
		body.AmountMinor.check("amount_minor", &issues)
	}
	note := ""
	if body.Note != nil {
		var ok bool
		if note, ok = validateAdminReason(*body.Note, maxPaymentNoteRunes); !ok {
			issues = append(issues, content.Issue{Path: "note", Code: "invalid", Message: "Up to 200 characters, no control characters."})
		}
	}
	if len(issues) > 0 {
		writeErrorDetails(w, http.StatusBadRequest, "validation_failed", "Check the highlighted fields.", issues)
		return
	}

	callerID, _ := userIDFrom(r.Context())
	if !s.allow(w, r, limit{"admin:payment:grant:" + callerID.String(), 60, time.Hour}) {
		return
	}

	ctx := r.Context()
	var resp adminPaymentResp
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		currentTemplate, err := q.LockEventForPayment(ctx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errAdminEventNotFound
		}
		if err != nil {
			return fmt.Errorf("lock event for payment: %w", err)
		}
		if currentTemplate != templateID {
			return errPaymentTemplate
		}
		tpl, err := q.GetTemplateAdmin(ctx, templateID)
		if err != nil {
			return fmt.Errorf("get template admin: %w", err)
		}
		if !tpl.IsPremium {
			return errPaymentNotPremium
		}
		p, err := q.GrantEventPayment(ctx, store.GrantEventPaymentParams{
			ID: uuid.Must(uuid.NewV7()), EventID: eventID, TemplateID: tpl.ID, GrantedBy: &callerID, Region: region, Note: note,
			ExpectedAmountMinor: expected,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// The template is premium (checked above), so a live grant exists, the region isn't offered, or the price differs.
			paid, perr := q.EventTemplatePaid(ctx, store.EventTemplatePaidParams{EventID: eventID, TemplateID: tpl.ID})
			if perr != nil {
				return fmt.Errorf("check template payment: %w", perr)
			}
			if paid {
				return errPaymentAlreadyPaid
			}
			if region == "IN" {
				return errPaymentPrice
			}
			prices, perr := q.ListTemplatePrices(ctx, []uuid.UUID{tpl.ID})
			if perr != nil {
				return fmt.Errorf("list template prices: %w", perr)
			}
			for _, tp := range prices {
				if tp.Region == region {
					return errPaymentPrice
				}
			}
			return errPaymentRegion
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return errAdminEventNotFound
		}
		if err != nil {
			return fmt.Errorf("grant event payment: %w", err)
		}
		resp = newAdminPaymentResp(p, tpl.Name)
		// The note is free text an admin may paste contact details into, so only its presence is audited.
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "event.payment.grant", TargetType: "event", TargetID: eventID.String(),
			After: map[string]any{
				"template_id": tpl.ID, "region": p.Region, "currency": p.Currency, "amount_minor": p.AmountMinor, "has_note": note != "",
			},
		})
	})
	switch {
	case errors.Is(err, errAdminEventNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such event.")
	case errors.Is(err, errPaymentTemplate):
		writeError(w, http.StatusConflict, "template_changed", "This event's template has changed. Reload and try again.")
	case errors.Is(err, errPaymentRegion):
		writeError(w, http.StatusConflict, "region_unavailable", "This template isn't offered in that region.")
	case errors.Is(err, errPaymentPrice):
		writeError(w, http.StatusConflict, "price_changed", "The price changed — reload the page and check the amount.")
	case errors.Is(err, errPaymentNotPremium):
		writeError(w, http.StatusConflict, "not_premium", "This event's template isn't premium.")
	case errors.Is(err, errPaymentAlreadyPaid):
		writeError(w, http.StatusConflict, "already_paid", "This template is already unlocked for this event.")
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, map[string]any{"payment": resp})
	}
}

// DELETE /v1/admin/events/{id}/payments/{template_id}
//
// handleAdminRevokeEventPayment revokes a recorded unlock (recorded by
// mistake, refunded); the row is kept with who revoked it. A published event stays published; the payment gate
// applies to its next publish.
func (s *Server) handleAdminRevokeEventPayment(w http.ResponseWriter, r *http.Request) {
	eventID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such payment.")
		return
	}
	templateID, err := parseUUIDParam(r, "template_id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such payment.")
		return
	}
	callerID, _ := userIDFrom(r.Context())
	if !s.allow(w, r, limit{"admin:payment:revoke:" + callerID.String(), 60, time.Hour}) {
		return
	}

	ctx := r.Context()
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		p, err := q.RevokeEventPayment(ctx, store.RevokeEventPaymentParams{RevokedBy: callerID, EventID: eventID, TemplateID: templateID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errPaymentNotFound
		}
		if err != nil {
			return fmt.Errorf("revoke event payment: %w", err)
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "event.payment.revoke", TargetType: "event", TargetID: eventID.String(),
			Before: map[string]any{
				"payment_id": p.ID, "template_id": p.TemplateID, "granted_by": p.GrantedBy, "granted_at": p.CreatedAt,
				"has_note": p.Note != "", "region": p.Region, "currency": p.Currency, "amount_minor": p.AmountMinor,
			},
		})
	})
	switch {
	case errors.Is(err, errPaymentNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No such payment.")
	case err != nil:
		serverError(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
