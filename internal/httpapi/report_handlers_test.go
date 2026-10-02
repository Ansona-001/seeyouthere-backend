package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ansonarose/seeyouthere-backend/internal/jobs"
	"github.com/ansonarose/seeyouthere-backend/internal/media"
)

// withJobs attaches a real (but never-started) River client to f.s, the way
// TestHandleDeleteEvent_EnqueuesMediaVisibilityJob does: handleCreateReport
// enqueues notify_report inside the same tx via s.jobs.InsertTx, which needs
// a client that at least recognises the job kind.
func (f *publicEventFixture) withJobs(t *testing.T) {
	t.Helper()
	mediaStore, err := media.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("media.NewStore: %v", err)
	}
	jobClient, err := jobs.NewClient(f.s.pool, noopSender{}, noopSender{}, f.s.q, mediaStore, f.s.tokens, f.s.limiter, "", f.s.cfg.SiteURL)
	if err != nil {
		t.Fatalf("jobs.NewClient: %v", err)
	}
	f.s.jobs = jobClient
}

func countReportRows(t *testing.T, f *publicEventFixture) int {
	t.Helper()
	var n int
	if err := f.s.pool.QueryRow(context.Background(), "SELECT count(*) FROM reports WHERE event_id = $1", f.eventID).Scan(&n); err != nil {
		t.Fatalf("count reports: %v", err)
	}
	return n
}

func countNotifyReportJobs(t *testing.T, f *publicEventFixture) int {
	t.Helper()
	var n int
	if err := f.s.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM river_job WHERE kind = 'notify_report' AND args->>'event_id' = $1", f.eventID.String(),
	).Scan(&n); err != nil {
		t.Fatalf("count notify_report jobs: %v", err)
	}
	return n
}

func TestHandleCreateReport(t *testing.T) {
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)

	post := func(f *publicEventFixture, ip, body string) *httptest.ResponseRecorder {
		req := requestWithIP(http.MethodPost, "/", ip, []byte(body))
		req.Header.Set("Content-Type", "application/json")
		req = withSlugParam(req, f.slug)
		rec := httptest.NewRecorder()
		f.s.handleCreateReport(rec, req)
		return rec
	}

	t.Run("valid report is accepted and enqueues one notify_report job", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		f.withJobs(t)
		rec := post(f, nextTestIP(), `{"reason":"spam","details":"Looks like spam.","website":""}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
		}
		if n := countReportRows(t, f); n != 1 {
			t.Errorf("report rows = %d, want 1", n)
		}
		if n := countNotifyReportJobs(t, f); n != 1 {
			t.Errorf("notify_report jobs = %d, want 1", n)
		}
	})

	t.Run("honeypot: a filled website field answers exactly like success and writes nothing", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		f.withJobs(t)
		rec := post(f, nextTestIP(), `{"reason":"spam","details":"","website":"https://spambot.example"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
		}
		if n := countReportRows(t, f); n != 0 {
			t.Errorf("report rows = %d, want 0 (honeypot must not write)", n)
		}
	})

	t.Run("invalid reason is 400", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		rec := post(f, nextTestIP(), `{"reason":"not-a-real-reason","details":"","website":""}`)
		if rec.Code != http.StatusBadRequest || !containsAll(rec.Body.String(), `"validation_failed"`) {
			t.Fatalf("status = %d, body = %s, want 400 validation_failed", rec.Code, rec.Body.String())
		}
	})

	t.Run("details over the rune limit is 400", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		long := make([]byte, maxReportDetailsRunes+1)
		for i := range long {
			long[i] = 'a'
		}
		rec := post(f, nextTestIP(), `{"reason":"other","details":"`+string(long)+`","website":""}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("details with a control character is 400", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		rec := post(f, nextTestIP(), "{\"reason\":\"other\",\"details\":\"line one\\u0007bell\",\"website\":\"\"}")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown slug is a uniform 202, revealing nothing", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		req := requestWithIP(http.MethodPost, "/", nextTestIP(), []byte(`{"reason":"spam","details":"","website":""}`))
		req.Header.Set("Content-Type", "application/json")
		req = withSlugParam(req, "no-such-event-whatsoever")
		rec := httptest.NewRecorder()
		f.s.handleCreateReport(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a duplicate report from the same reporter is silently ignored (still 202, no second job)", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		f.withJobs(t)
		ip := nextTestIP()
		rec1 := post(f, ip, `{"reason":"spam","details":"first","website":""}`)
		rec2 := post(f, ip, `{"reason":"harassment","details":"second","website":""}`)
		if rec1.Code != http.StatusAccepted || rec2.Code != http.StatusAccepted {
			t.Fatalf("status = %d, %d, want 202, 202", rec1.Code, rec2.Code)
		}
		if n := countReportRows(t, f); n != 1 {
			t.Errorf("report rows = %d, want 1 (dedupe on open report per reporter)", n)
		}
		if n := countNotifyReportJobs(t, f); n != 1 {
			t.Errorf("notify_report jobs = %d, want 1 (no job for the ignored duplicate)", n)
		}
	})

	t.Run("rate limited after 5 reports from the same IP within the window", func(t *testing.T) {
		f := newPublicEventFixture(t, pool, rdb, publicEventOpts{})
		f.withJobs(t)
		ip := nextTestIP()
		var last *httptest.ResponseRecorder
		for i := 0; i < 6; i++ {
			last = post(f, ip, `{"reason":"spam","details":"","website":""}`)
		}
		if last.Code != http.StatusTooManyRequests {
			t.Fatalf("6th request status = %d, want 429", last.Code)
		}
	})
}
