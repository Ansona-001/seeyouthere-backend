package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
)

// occasionResp is the catalog shape for one occasion (§4.2). name comes from
// the store row directly since content.Occasion doesn't carry it.
type occasionResp struct {
	Slug           string                  `json:"slug"`
	Name           string                  `json:"name"`
	Copy           content.Copy            `json:"copy"`
	SetupQuestions []content.SetupQuestion `json:"setup_questions"`
	OptionalBlocks []string                `json:"optional_blocks"`
	RSVPFields     []content.Field         `json:"rsvp_fields"`
}

// GET /v1/occasions
func (s *Server) handleListOccasions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListActiveOccasions(r.Context())
	if err != nil {
		serverError(w, r, fmt.Errorf("list occasions: %w", err))
		return
	}

	occasions := make([]occasionResp, 0, len(rows))
	for _, row := range rows {
		occ, err := content.ParseOccasion(row)
		if err != nil {
			// Seed data is trusted; a parse failure here means the catalog
			// itself is broken, not a bad request.
			serverError(w, r, fmt.Errorf("parse occasion %s: %w", row.Slug, err))
			return
		}
		occasions = append(occasions, occasionResp{
			Slug:           occ.Slug,
			Name:           row.Name,
			Copy:           occ.Copy,
			SetupQuestions: occ.SetupQuestions,
			OptionalBlocks: occ.OptionalBlocks,
			RSVPFields:     occ.RSVPFields,
		})
	}

	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{"occasions": occasions})
}

// templateCatalogResp is the host-picker shape for one template (§4.2). tags
// passes the stored jsonb through as-is ({"occasions": [...]}).
type templateCatalogResp struct {
	ID         uuid.UUID                `json:"id"`
	Slug       string                   `json:"slug"`
	Name       string                   `json:"name"`
	Tags       json.RawMessage          `json:"tags"`
	Version    int32                    `json:"version"`
	Defaults   content.ManifestDefaults `json:"defaults"`
	Palettes   []content.Palette        `json:"palettes"`
	Fonts      []content.FontPair       `json:"fonts"`
	Layout     string                   `json:"layout"`
	HeroStyle  string                   `json:"hero_style"`
	Decoration string                   `json:"decoration"`
}

// GET /v1/templates?occasion=
func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	var occasion *string
	if v := strings.TrimSpace(r.URL.Query().Get("occasion")); v != "" {
		occasion = &v
	}

	rows, err := s.q.ListPublishedTemplates(r.Context(), occasion)
	if err != nil {
		serverError(w, r, fmt.Errorf("list templates: %w", err))
		return
	}

	templates := make([]templateCatalogResp, 0, len(rows))
	for _, row := range rows {
		m, err := content.ValidateStoredManifest(row.Manifest)
		if err != nil {
			// Published manifests are validated at publish time; a failure
			// here means stored data has drifted from the validator. Skip just
			// this template so one bad version can't take the whole picker
			// down; the error carries the first issue only, never the body.
			slog.ErrorContext(r.Context(), "skipping template with invalid manifest",
				"request_id", middleware.GetReqID(r.Context()),
				"template_slug", row.Slug, "version", row.Version, "err", err)
			continue
		}
		tags := row.Tags
		if len(tags) == 0 {
			tags = json.RawMessage("{}")
		}
		templates = append(templates, templateCatalogResp{
			ID:         row.ID,
			Slug:       row.Slug,
			Name:       row.Name,
			Tags:       tags,
			Version:    row.Version,
			Defaults:   m.Defaults,
			Palettes:   m.Palettes,
			Fonts:      m.Fonts,
			Layout:     m.Layout,
			HeroStyle:  m.HeroStyle,
			Decoration: m.Decoration,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"templates": templates})
}
