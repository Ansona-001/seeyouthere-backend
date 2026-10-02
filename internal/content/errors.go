// Package content validates and normalises everything that ends up inside
// events.content, events.overrides and template_versions.manifest. It is
// pure Go: no database access, no network, no filesystem. Callers decode
// caller-controlled JSON with ValidateContent/ValidateManifest/ValidateOverrides,
// or re-parse trusted, previously-validated JSON with ParseStored.
//
// Validation is strict: unknown fields are rejected, every string is measured
// in runes, and the accepted output is always the canonical (re-marshalled)
// form so two equivalent payloads are stored identically.
package content

import "fmt"

// Issue describes one validation failure. Path identifies the offending
// value (e.g. "content[2].title", "answers.dietary") so a client can
// highlight the right field.
type Issue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// maxIssues bounds the response size for a hostile payload with many errors.
const maxIssues = 50

// ValidationError reports every issue found in one validation pass.
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	if len(e.Issues) == 0 {
		return "validation failed"
	}
	first := e.Issues[0]
	return fmt.Sprintf("validation failed: %s: %s", first.Path, first.Message)
}

// issues accumulates Issue values during a validation pass and stops
// recording once maxIssues is reached, so the caller can keep validating
// (for accurate downstream state) without the response growing unbounded.
type issues struct {
	list []Issue
}

func (c *issues) add(path, code, message string) {
	if len(c.list) >= maxIssues {
		return
	}
	c.list = append(c.list, Issue{Path: path, Code: code, Message: message})
}

func (c *issues) err() error {
	if len(c.list) == 0 {
		return nil
	}
	return &ValidationError{Issues: c.list}
}

// single is a convenience for validators that only ever fail with one issue.
func single(path, code, message string) error {
	return &ValidationError{Issues: []Issue{{Path: path, Code: code, Message: message}}}
}
