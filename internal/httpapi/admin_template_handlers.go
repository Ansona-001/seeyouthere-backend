package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ansonarose/seeyouthere-backend/internal/content"
	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

var (
	errTemplateNotFound        = errors.New("admin: template not found")
	errTemplateVersionPublish  = errors.New("admin: template version already published")
	errTemplateVersionInvalid  = errors.New("admin: template version manifest invalid")
	errTemplateVersionNotReady = errors.New("admin: template version missing its asset")
)

// templateSlugRe mirrors templates.slug's shape: lower-case, hyphenated.
var templateSlugRe = regexp.MustCompile(`^[a-z0-9-]{1,50}$`)

// --- response shapes ---

type adminTemplateResp struct {
	ID            uuid.UUID       `json:"id"`
	Slug          string          `json:"slug"`
	Name          string          `json:"name"`
	Tags          json.RawMessage `json:"tags"`
	IsPremium     bool            `json:"is_premium"`
	Status        string          `json:"status"`
	LatestVersion int32           `json:"latest_version"`
}

func tagsOrEmptyObject(tags []byte) json.RawMessage {
	if len(tags) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(tags)
}

type adminTemplateVersionResp struct {
	Version     int32           `json:"version"`
	Manifest    json.RawMessage `json:"manifest"`
	Status      string          `json:"status"`
	PublishedAt *time.Time      `json:"published_at"`
	CreatedBy   *uuid.UUID      `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
}

func versionStatus(publishedAt *time.Time) string {
	if publishedAt != nil {
		return "published"
	}
	return "draft"
}

// --- GET, POST /v1/admin/templates ---

func (s *Server) handleListAdminTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListTemplatesAdmin(r.Context())
	if err != nil {
		serverError(w, r, fmt.Errorf("list templates admin: %w", err))
		return
	}
	items := make([]adminTemplateResp, len(rows))
	for i, row := range rows {
		items[i] = adminTemplateResp{
			ID: row.ID, Slug: row.Slug, Name: row.Name, Tags: tagsOrEmptyObject(row.Tags),
			IsPremium: row.IsPremium, Status: row.Status, LatestVersion: row.LatestVersion,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": items})
}

func (s *Server) handleCreateAdminTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug      string          `json:"slug"`
		Name      string          `json:"name"`
		Tags      json.RawMessage `json:"tags"`
		IsPremium bool            `json:"is_premium"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !templateSlugRe.MatchString(body.Slug) {
		writeError(w, http.StatusBadRequest, "validation_failed", "Slug must be lower-case letters, digits and hyphens.")
		return
	}
	name := body.Name
	if n := utf8.RuneCountInString(name); n == 0 || n > 120 {
		writeError(w, http.StatusBadRequest, "validation_failed", "Name must be 1-120 characters.")
		return
	}
	tags, ok := validateTemplateTags(body.Tags)
	if !ok {
		writeError(w, http.StatusBadRequest, "validation_failed", "Invalid tags.")
		return
	}

	callerID, _ := userIDFrom(r.Context())
	ctx := r.Context()
	templateID := uuid.Must(uuid.NewV7())
	var created store.Template
	err := s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		var err error
		created, err = q.CreateTemplate(ctx, store.CreateTemplateParams{
			ID: templateID, Slug: body.Slug, Name: name, Tags: tags, IsPremium: body.IsPremium,
		})
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "template.create", TargetType: "template", TargetID: templateID.String(),
			After: map[string]any{"slug": body.Slug, "name": name, "is_premium": body.IsPremium},
		})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "slug_taken", "That template slug is already in use.")
			return
		}
		serverError(w, r, fmt.Errorf("create template: %w", err))
		return
	}
	writeJSON(w, http.StatusCreated, adminTemplateResp{
		ID: created.ID, Slug: created.Slug, Name: created.Name, Tags: tagsOrEmptyObject(created.Tags),
		IsPremium: created.IsPremium, Status: created.Status, LatestVersion: 0,
	})
}

// validateTemplateTags requires an object shaped {"occasions": ["slug", ...]}
// (or an empty object/omitted field), matching how the catalog filters by
// tags @> jsonb_build_object('occasions', ...). Canonicalises missing tags
// to "{}".
func validateTemplateTags(raw json.RawMessage) ([]byte, bool) {
	if len(raw) == 0 {
		return []byte("{}"), true
	}
	var v struct {
		Occasions []string `json:"occasions"`
	}
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if len(v.Occasions) > 20 {
		return nil, false
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return canon, true
}

// --- GET, PATCH /v1/admin/templates/{id} ---

func (s *Server) handleGetAdminTemplate(w http.ResponseWriter, r *http.Request) {
	templateID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such template.")
		return
	}
	ctx := r.Context()
	tpl, err := s.q.GetTemplateAdmin(ctx, templateID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such template.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get template admin: %w", err))
		return
	}
	resp, err := s.buildAdminTemplateDetail(ctx, tpl)
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type adminTemplateDetailResp struct {
	adminTemplateResp
	Versions []adminTemplateVersionResp `json:"versions"`
}

// buildAdminTemplateDetail loads every version's manifest individually
// (ListTemplateVersions deliberately omits manifest, "without manifests
// (wide); fetch one with GetTemplateVersion"). Templates have at most a
// handful of versions and this is an admin-only, low-traffic page, so the
// per-version round trip is fine.
func (s *Server) buildAdminTemplateDetail(ctx context.Context, tpl store.Template) (adminTemplateDetailResp, error) {
	versions, err := s.q.ListTemplateVersions(ctx, tpl.ID)
	if err != nil {
		return adminTemplateDetailResp{}, fmt.Errorf("list template versions: %w", err)
	}
	var latest int32
	items := make([]adminTemplateVersionResp, len(versions))
	for i, v := range versions {
		if v.Version > latest {
			latest = v.Version
		}
		full, err := s.q.GetTemplateVersion(ctx, store.GetTemplateVersionParams{TemplateID: tpl.ID, Version: v.Version})
		if err != nil {
			return adminTemplateDetailResp{}, fmt.Errorf("get template version %d: %w", v.Version, err)
		}
		items[i] = adminTemplateVersionResp{
			Version: v.Version, Manifest: json.RawMessage(full.Manifest), Status: versionStatus(v.PublishedAt),
			PublishedAt: v.PublishedAt, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt,
		}
	}
	return adminTemplateDetailResp{
		adminTemplateResp: adminTemplateResp{
			ID: tpl.ID, Slug: tpl.Slug, Name: tpl.Name, Tags: tagsOrEmptyObject(tpl.Tags),
			IsPremium: tpl.IsPremium, Status: tpl.Status, LatestVersion: latest,
		},
		Versions: items,
	}, nil
}

func (s *Server) handleUpdateAdminTemplate(w http.ResponseWriter, r *http.Request) {
	templateID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such template.")
		return
	}
	var body struct {
		Name      *string         `json:"name"`
		Tags      json.RawMessage `json:"tags"`
		IsPremium *bool           `json:"is_premium"`
		Status    *string         `json:"status"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name != nil {
		if n := utf8.RuneCountInString(*body.Name); n == 0 || n > 120 {
			writeError(w, http.StatusBadRequest, "validation_failed", "Name must be 1-120 characters.")
			return
		}
	}
	var tags []byte
	if len(body.Tags) > 0 {
		canon, ok := validateTemplateTags(body.Tags)
		if !ok {
			writeError(w, http.StatusBadRequest, "validation_failed", "Invalid tags.")
			return
		}
		tags = canon
	}
	if body.Status != nil && *body.Status != "draft" && *body.Status != "published" {
		writeError(w, http.StatusBadRequest, "validation_failed", "Status must be draft or published.")
		return
	}

	ctx := r.Context()
	if body.Status != nil && *body.Status == "published" {
		count, err := s.q.CountPublishedVersions(ctx, templateID)
		if err != nil {
			serverError(w, r, fmt.Errorf("count published versions: %w", err))
			return
		}
		if count == 0 {
			writeError(w, http.StatusConflict, "needs_published_version", "Publish at least one version first.")
			return
		}
	}

	callerID, _ := userIDFrom(r.Context())
	var updated store.UpdateTemplateRow
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		var err error
		updated, err = q.UpdateTemplate(ctx, store.UpdateTemplateParams{
			Name: body.Name, Tags: tags, IsPremium: body.IsPremium, Status: body.Status, TemplateID: templateID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTemplateNotFound
		}
		if err != nil {
			return fmt.Errorf("update template: %w", err)
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "template.update", TargetType: "template", TargetID: templateID.String(),
			Before: map[string]any{"name": updated.OldName, "is_premium": updated.OldIsPremium, "status": updated.OldStatus},
			After:  map[string]any{"name": updated.Name, "is_premium": updated.IsPremium, "status": updated.Status},
		})
	})
	if err != nil {
		if errors.Is(err, errTemplateNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such template.")
			return
		}
		serverError(w, r, err)
		return
	}
	detail, err := s.buildAdminTemplateDetail(ctx, store.Template{
		ID: updated.ID, Slug: updated.Slug, Name: updated.Name, Tags: updated.Tags,
		Status: updated.Status, IsPremium: updated.IsPremium, CreatedAt: updated.CreatedAt, UpdatedAt: updated.UpdatedAt,
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// --- POST /v1/admin/templates/{id}/versions ---

// handleCreateAdminTemplateVersion supports either an explicit manifest or
// copy_from_version (the admin UI's "New draft version" button sends only
// copy_from_version, no manifest): exactly one source is required.
func (s *Server) handleCreateAdminTemplateVersion(w http.ResponseWriter, r *http.Request) {
	templateID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such template.")
		return
	}
	var body struct {
		Manifest        json.RawMessage `json:"manifest"`
		CopyFromVersion *int32          `json:"copy_from_version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Manifest) == 0 && body.CopyFromVersion == nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "Provide a manifest or copy_from_version.")
		return
	}

	ctx := r.Context()
	manifestRaw := []byte(body.Manifest)
	if len(manifestRaw) == 0 {
		src, err := s.q.GetTemplateVersion(ctx, store.GetTemplateVersionParams{TemplateID: templateID, Version: *body.CopyFromVersion})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "validation_failed", "Unknown copy_from_version.")
			return
		}
		if err != nil {
			serverError(w, r, fmt.Errorf("get template version: %w", err))
			return
		}
		manifestRaw = src.Manifest
	}
	manifest, err := content.ValidateManifest(manifestRaw)
	if err != nil {
		writeValidationIssues(w, r, err, "Check the manifest.")
		return
	}
	canon, err := json.Marshal(manifest)
	if err != nil {
		serverError(w, r, fmt.Errorf("marshal manifest: %w", err))
		return
	}

	callerID, _ := userIDFrom(r.Context())
	var created store.CreateTemplateVersionRow
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		var err error
		created, err = q.CreateTemplateVersion(ctx, store.CreateTemplateVersionParams{
			TemplateID: templateID, Manifest: canon, CreatedBy: &callerID,
		})
		if err != nil {
			return fmt.Errorf("create template version: %w", err)
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "template_version.create", TargetType: "template_version",
			TargetID: fmt.Sprintf("%s/%d", templateID, created.Version),
			After:    map[string]any{"version": created.Version},
		})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "retry", "Try again.")
			return
		}
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, adminTemplateVersionResp{
		Version: created.Version, Manifest: json.RawMessage(canon), Status: "draft",
		PublishedAt: created.PublishedAt, CreatedBy: created.CreatedBy, CreatedAt: created.CreatedAt,
	})
}

// --- PUT /v1/admin/templates/{id}/versions/{v} ---

func (s *Server) handleUpdateAdminTemplateVersion(w http.ResponseWriter, r *http.Request) {
	templateID, version, ok := parseTemplateVersionParams(w, r)
	if !ok {
		return
	}
	var body struct {
		Manifest json.RawMessage `json:"manifest"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	manifest, err := content.ValidateManifest(body.Manifest)
	if err != nil {
		writeValidationIssues(w, r, err, "Check the manifest.")
		return
	}
	canon, err := json.Marshal(manifest)
	if err != nil {
		serverError(w, r, fmt.Errorf("marshal manifest: %w", err))
		return
	}

	callerID, _ := userIDFrom(r.Context())
	ctx := r.Context()
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		n, err := q.UpdateTemplateVersionManifest(ctx, store.UpdateTemplateVersionManifestParams{
			Manifest: canon, TemplateID: templateID, Version: version,
		})
		if err != nil {
			return fmt.Errorf("update template version manifest: %w", err)
		}
		if n == 0 {
			return errTemplateVersionPublish
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "template_version.update", TargetType: "template_version",
			TargetID: fmt.Sprintf("%s/%d", templateID, version),
		})
	})
	if err != nil {
		if errors.Is(err, errTemplateVersionPublish) {
			writeError(w, http.StatusConflict, "version_published", "This version is published and can't be edited.")
			return
		}
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, adminTemplateVersionResp{Version: version, Manifest: json.RawMessage(canon), Status: "draft"})
}

// --- POST /v1/admin/templates/{id}/versions/{v}/background ---

func (s *Server) handleUploadAdminTemplateBackground(w http.ResponseWriter, r *http.Request) {
	templateID, version, ok := parseTemplateVersionParams(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// Confirm the version exists and is still a draft before doing any
	// filesystem work: without this, a bad id/version only fails later at
	// SetTemplateVersionAssets, after CommitTemplateAsset has already moved
	// the upload into public/templates/<id>/<v>/, leaving an orphaned
	// directory with no row pointing at it.
	verRow, err := s.q.GetTemplateVersion(ctx, store.GetTemplateVersionParams{TemplateID: templateID, Version: version})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such template version.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get template version: %w", err))
		return
	}
	if verRow.PublishedAt != nil {
		writeError(w, http.StatusConflict, "version_published", "This version is published and can't be edited.")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxHostUploadBytes)
	outDir, _, ok := s.processUpload(w, r, body)
	if !ok {
		return
	}
	assetsPath, err := s.media.CommitTemplateAsset(ctx, outDir, templateID, version)
	if err != nil {
		writeStorageError(w, r, fmt.Errorf("commit template asset: %w", err))
		return
	}

	callerID, _ := userIDFrom(r.Context())
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		n, err := q.SetTemplateVersionAssets(ctx, store.SetTemplateVersionAssetsParams{
			AssetsPath: assetsPath, TemplateID: templateID, Version: version,
		})
		if err != nil {
			return fmt.Errorf("set template version assets: %w", err)
		}
		if n == 0 {
			return errTemplateVersionPublish
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "template_version.asset", TargetType: "template_version",
			TargetID: fmt.Sprintf("%s/%d", templateID, version), After: map[string]string{"assets_path": assetsPath},
		})
	})
	if err != nil {
		if errors.Is(err, errTemplateVersionPublish) {
			writeError(w, http.StatusConflict, "version_published", "This version is published and can't be edited.")
			return
		}
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"assets_path": assetsPath})
}

// --- POST /v1/admin/templates/{id}/versions/{v}/publish ---

// handleAdminPublishTemplateVersion re-validates the manifest (a manifest
// requiring a background asset must have one committed) before publishing.
// The row is locked (FOR UPDATE) for the whole transaction so a concurrent
// manifest or asset edit cannot land between validation and publish: it
// waits, then fails with version_published.
func (s *Server) handleAdminPublishTemplateVersion(w http.ResponseWriter, r *http.Request) {
	templateID, version, ok := parseTemplateVersionParams(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	callerID, _ := userIDFrom(ctx)
	var (
		publishedAt time.Time
		manifestRaw []byte
		invalid     error
	)
	err := s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		row, err := q.GetTemplateVersionForUpdate(ctx, store.GetTemplateVersionForUpdateParams{TemplateID: templateID, Version: version})
		if err != nil {
			return err
		}
		if row.PublishedAt != nil {
			return errTemplateVersionPublish
		}
		manifest, err := content.ValidateStoredManifest(row.Manifest)
		if err != nil {
			invalid = err
			return errTemplateVersionInvalid
		}
		if manifest.UsesBackgroundAsset() && row.AssetsPath == "" {
			return errTemplateVersionNotReady
		}
		manifestRaw = row.Manifest
		publishedAt, err = q.PublishTemplateVersion(ctx, store.PublishTemplateVersionParams{TemplateID: templateID, Version: version})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTemplateVersionPublish
		}
		if err != nil {
			return fmt.Errorf("publish template version: %w", err)
		}
		return s.writeAudit(ctx, q, r, auditEntry{
			ActorID: &callerID, Action: "template_version.publish", TargetType: "template_version",
			TargetID: fmt.Sprintf("%s/%d", templateID, version),
		})
	})
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "No such template version.")
		return
	case errors.Is(err, errTemplateVersionInvalid):
		writeValidationIssues(w, r, invalid, "This version's manifest is invalid.")
		return
	case errors.Is(err, errTemplateVersionNotReady):
		writeError(w, http.StatusConflict, "not_ready", "Upload a background image before publishing.")
		return
	case errors.Is(err, errTemplateVersionPublish):
		writeError(w, http.StatusConflict, "version_published", "This version is already published.")
		return
	default:
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, adminTemplateVersionResp{
		Version: version, Manifest: json.RawMessage(manifestRaw), Status: "published", PublishedAt: &publishedAt,
	})
}

// --- GET /v1/admin/templates/{id}/versions/{v}/preview ---

// handleAdminPreviewTemplateVersion resolves the theme for every palette x
// font pair combination in the manifest, so the admin UI can render sample
// swatches without parsing the manifest itself.
func (s *Server) handleAdminPreviewTemplateVersion(w http.ResponseWriter, r *http.Request) {
	templateID, version, ok := parseTemplateVersionParams(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	row, err := s.q.GetTemplateVersion(ctx, store.GetTemplateVersionParams{TemplateID: templateID, Version: version})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "No such template version.")
		return
	}
	if err != nil {
		serverError(w, r, fmt.Errorf("get template version: %w", err))
		return
	}
	manifest, err := content.ValidateStoredManifest(row.Manifest)
	if err != nil {
		writeValidationIssues(w, r, err, "This version's manifest is invalid.")
		return
	}
	backgroundSrc := ""
	if manifest.UsesBackgroundAsset() && row.AssetsPath != "" {
		backgroundSrc = "/media/" + row.AssetsPath + "/background"
	}

	type preview struct {
		PaletteID string        `json:"palette_id"`
		FontID    string        `json:"font_id"`
		Theme     content.Theme `json:"theme"`
	}
	previews := make([]preview, 0, len(manifest.Palettes)*len(manifest.Fonts))
	for _, p := range manifest.Palettes {
		for _, f := range manifest.Fonts {
			overrides, _ := json.Marshal(content.Overrides{Palette: p.ID, Font: f.ID})
			previews = append(previews, preview{
				PaletteID: p.ID, FontID: f.ID, Theme: content.ResolveTheme(manifest, overrides, backgroundSrc),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"previews": previews})
}

// parseTemplateVersionParams parses {id} and {v} together, since every
// version-scoped admin route needs both.
func parseTemplateVersionParams(w http.ResponseWriter, r *http.Request) (uuid.UUID, int32, bool) {
	templateID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such template.")
		return uuid.Nil, 0, false
	}
	v, err := parseInt32Param(r, "v")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such template version.")
		return uuid.Nil, 0, false
	}
	return templateID, v, true
}
