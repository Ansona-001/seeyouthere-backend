package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Details carries structured extra context for a handful of error
	// codes: validation_failed ([]content.Issue-shaped entries),
	// version_conflict ({"current_version": n}), not_ready (missing
	// fields) and CSV import row errors. Omitted for every other error.
	Details any `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: message}})
}

// writeErrorDetails is writeError plus a structured Details payload.
func writeErrorDetails(w http.ResponseWriter, status int, code, message string, details any) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: message, Details: details}})
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "request failed", "err", err, "path", r.URL.Path)
	writeError(w, http.StatusInternalServerError, "internal", "Something went wrong.")
}

const maxBodyBytes = 64 << 10

// decodeJSON reads a JSON body up to the default 64 KiB limit. Larger
// bodies (e.g. a full-document PATCH) call decodeJSONMax directly.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSONMax(w, r, dst, maxBodyBytes)
}

// decodeJSONMax reads a JSON body into dst, writing a 4xx response and
// returning false on failure. Requiring application/json also means plain
// HTML forms can't reach these endpoints (no CSRF token needed on top of
// the existing Origin/Sec-Fetch-Site checks).
func decodeJSONMax(w http.ResponseWriter, r *http.Request, dst any, max int64) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send JSON with Content-Type: application/json.")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large.")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "Request body is not valid JSON.")
		}
		return false
	}
	return true
}
