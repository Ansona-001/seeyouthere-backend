package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// maxReportDetailsRunes matches the reports_details_len_check CHECK constraint.
const maxReportDetailsRunes = 1000

var validReportReasons = map[string]bool{
	"phishing": true, "spam": true, "harassment": true, "illegal": true, "other": true,
}

// normalizeReportDetails trims and canonicalises line endings the way
// internal/content does for free text, and rejects control characters
// (other than newline) and Unicode bidi-override code points, so a report
// can't smuggle terminal escapes or spoofed text into the admin queue.
func normalizeReportDetails(s string) (string, bool) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxReportDetailsRunes {
		return "", false
	}
	for _, r := range s {
		switch {
		case r == '\n':
		case r < 0x20, r == 0x7f:
			return "", false
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return "", false
		}
	}
	return s, true
}

// handleCreateReport (§4.4) lets any visitor flag a page for review.
// Honeypot: a non-empty "website" answers exactly like success without
// writing anything. The endpoint otherwise always answers 202 for a
// structurally valid body, including for an unknown/hidden/draft/
// taken-down/deleted slug (the same uniform-404 doctrine as the GET, just
// folded into an always-202 response instead) and for a duplicate report
// (CreateReport's ON CONFLICT DO NOTHING against the reporter's own open
// report for the event): a caller learns nothing about whether the slug
// exists or whether their report was the first.
func (s *Server) handleCreateReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := clientIPFrom(ctx)
	if !s.allow(w, r, limit{"reports:ip:" + ipRateKey(ip), 5, time.Hour}) {
		return
	}
	// Mirrors ipRateKey's /64 collapse for IPv6: without it, an attacker
	// with an IPv6 allocation can rotate addresses within one /64 to defeat
	// reports_open_dedupe_key's per-reporter duplicate check even though the
	// rate limit above (keyed the same way) still catches them.
	hashIP := ip.Unmap()
	if hashIP.IsValid() && hashIP.Is6() {
		hashIP = netip.PrefixFrom(hashIP, 64).Masked().Addr()
	}

	var body struct {
		Reason  string `json:"reason"`
		Details string `json:"details"`
		Website string `json:"website"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Website != "" {
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
		return
	}
	if !validReportReasons[body.Reason] {
		writeError(w, http.StatusBadRequest, "validation_failed", "Choose a reason.")
		return
	}
	details, ok := normalizeReportDetails(body.Details)
	if !ok {
		writeError(w, http.StatusBadRequest, "validation_failed", "Check the details field.")
		return
	}

	norm, ok := content.NormalizeSlug(chi.URLParam(r, "slug"))
	if !ok {
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
		return
	}
	row, err := s.q.GetPublicEventBySlug(ctx, &norm)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get public event: %w", err))
		return
	}

	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		n, err := q.CreateReport(ctx, store.CreateReportParams{
			ID: uuid.Must(uuid.NewV7()), EventID: row.ID, ReporterIpHash: s.tokens.HashIP(hashIP),
			Reason: body.Reason, Details: details,
		})
		if err != nil {
			return fmt.Errorf("create report: %w", err)
		}
		if n == 0 {
			return nil // this reporter already has an open report for the event
		}
		_, err = s.jobs.InsertTx(ctx, tx, jobs.NotifyReportArgs{EventID: row.ID}, nil)
		return err
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}
