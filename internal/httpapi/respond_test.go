package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWriteErrorDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	details := map[string]int{"current_version": 3}

	writeErrorDetails(rec, 409, "version_conflict", "Someone else changed this.", details)

	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body struct {
		Error apiError `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "version_conflict" || body.Error.Message == "" {
		t.Errorf("unexpected error body: %+v", body.Error)
	}
	m, ok := body.Error.Details.(map[string]any)
	if !ok || m["current_version"] != float64(3) {
		t.Errorf("details = %+v", body.Error.Details)
	}
}

func TestWriteError_OmitsDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, 404, "not_found", "No such thing.")

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var errObj map[string]json.RawMessage
	if err := json.Unmarshal(raw["error"], &errObj); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if _, present := errObj["details"]; present {
		t.Error("plain writeError should omit the details field entirely")
	}
}
