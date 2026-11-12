package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ansonarose/seeyouthere-backend/internal/store"
)

// premiumEnv drives the full router for the manual, regional premium-template
// flow: two premium templates (A offered in IN+AE+INT, B in IN+AE only), one
// free template (F), a host who owns the events under test, and the admin
// fixture's super_admin / moderator.
type premiumEnv struct {
	a        adminAPI
	pool     *pgxpool.Pool
	tag      string // unique occasion tag scoping GET /v1/templates to these rows
	hostID   uuid.UUID
	hostTok  string
	tplA     premiumTpl
	tplB     premiumTpl
	tplFree  premiumTpl
	superTok string
}

// premiumTpl describes a seeded template. Price is the India price in paise;
// AE and INT are the other regional prices in minor units, 0 = not offered.
type premiumTpl struct {
	ID    uuid.UUID
	Slug  string
	Name  string
	Price int32
	AE    int32
	INT   int32
}

type premiumPrice struct {
	Region      string `json:"region"`
	Currency    string `json:"currency"`
	AmountMinor int32  `json:"amount_minor"`
}

// prices is the list the API must report: IN first, then AE and INT if offered.
func (p premiumTpl) prices() []premiumPrice {
	out := []premiumPrice{{"IN", "INR", p.Price}}
	if p.AE != 0 {
		out = append(out, premiumPrice{"AE", "AED", p.AE})
	}
	if p.INT != 0 {
		out = append(out, premiumPrice{"INT", "USD", p.INT})
	}
	return out
}

func newPremiumEnv(t *testing.T) *premiumEnv {
	t.Helper()
	pool := dbTestPool(t)
	rdb := dbTestRedis(t)
	a := newAdminAPI(t, pool, rdb)
	e := &premiumEnv{a: a, pool: pool, tag: "zz-" + uuid.NewString(), superTok: a.superTok}
	e.tplA = e.newTemplate(t, "prem-a", true, 59900, 1, 2900, 900)
	e.tplB = e.newTemplate(t, "prem-b", true, 79900, 2, 3900, 0)
	e.tplFree = e.newTemplate(t, "free", false, 59900, 3, 0, 0)
	e.hostID, _ = a.newTarget(t, "prem-host")
	e.hostTok = a.login(t, e.hostID, false)
	return e
}

// newTemplate inserts a published template with one published version. A
// published version can never be deleted (template_versions_immutable), so
// cleanup drops payments and prices and flips the template to draft, which
// hides it from every catalog query. ae / intl insert template_prices rows.
func (e *premiumEnv) newTemplate(t *testing.T, label string, premium bool, price int32, sort int, ae, intl int32) premiumTpl {
	t.Helper()
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	slug := label + "-" + id.String()
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO templates (id, slug, name, tags, status, sort_order, is_premium, price_paise)
		 VALUES ($1, $2, $3, $4::jsonb, 'published', $5, $6, $7)`,
		id, slug, label, `{"occasions":["`+e.tag+`"]}`, sort, premium, price); err != nil {
		t.Fatalf("insert template %s: %v", label, err)
	}
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO template_versions (template_id, version, manifest, published_at) VALUES ($1, 1, $2::jsonb, now())`,
		id, testManifestJSON(t)); err != nil {
		t.Fatalf("insert version %s: %v", label, err)
	}
	// INT is inserted before AE on purpose: the API order must not follow insertion order.
	if intl != 0 {
		e.offer(t, id, "INT", "USD", intl)
	}
	if ae != 0 {
		e.offer(t, id, "AE", "AED", ae)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = e.pool.Exec(bg, "DELETE FROM event_payments WHERE template_id = $1", id)
		_, _ = e.pool.Exec(bg, "DELETE FROM template_prices WHERE template_id = $1", id)
		_, _ = e.pool.Exec(bg, "UPDATE templates SET status = 'draft' WHERE id = $1", id)
	})
	return premiumTpl{ID: id, Slug: slug, Name: label, Price: price, AE: ae, INT: intl}
}

func (e *premiumEnv) offer(t *testing.T, id uuid.UUID, region, currency string, amount int32) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO template_prices (template_id, region, currency, amount_minor) VALUES ($1, $2, $3, $4)`,
		id, region, currency, amount); err != nil {
		t.Fatalf("insert template price %s: %v", region, err)
	}
}

type premiumEventView struct {
	ID       uuid.UUID `json:"id"`
	Status   string    `json:"status"`
	Version  int32     `json:"version"`
	Template struct {
		ID   uuid.UUID `json:"id"`
		Slug string    `json:"slug"`
	} `json:"template"`
	Premium *premiumInfo `json:"premium"`
}

type premiumInfo struct {
	Paid   bool           `json:"paid"`
	Prices []premiumPrice `json:"prices"`
}

func decodeEvent(t *testing.T, rec *httptest.ResponseRecorder) premiumEventView {
	t.Helper()
	var resp struct {
		Event premiumEventView `json:"event"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode event: %v; body = %s", err, rec.Body.String())
	}
	return resp.Event
}

// wantPremium fails unless the event's premium block is {paid, tpl.prices()}.
func wantPremium(t *testing.T, got *premiumInfo, paid bool, tpl premiumTpl) {
	t.Helper()
	if got == nil {
		t.Fatalf("premium = null, want paid=%v prices=%v", paid, tpl.prices())
	}
	if got.Paid != paid || !reflect.DeepEqual(got.Prices, tpl.prices()) {
		t.Fatalf("premium = %+v, want paid=%v prices=%v", got, paid, tpl.prices())
	}
}

// createEvent creates a draft host event on tpl and makes it publishable
// (slug set; title and start time come from the setup answers).
func (e *premiumEnv) createEvent(t *testing.T, tpl premiumTpl) premiumEventView {
	t.Helper()
	body := fmt.Sprintf(`{"occasion_slug":"birthday","template_slug":%q,"answers":{"name":"Sam","date":%q,"venue":"The Hall"}}`, tpl.Slug, seedTestDate)
	rec := e.a.do(http.MethodPost, "/v1/events", e.hostTok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create event: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ev := decodeEvent(t, rec)
	if _, err := e.pool.Exec(context.Background(), "UPDATE events SET slug = $2 WHERE id = $1", ev.ID, "prem-"+ev.ID.String()); err != nil {
		t.Fatalf("set slug: %v", err)
	}
	return ev
}

func (e *premiumEnv) getEvent(t *testing.T, id uuid.UUID) premiumEventView {
	t.Helper()
	rec := e.a.do(http.MethodGet, "/v1/events/"+id.String(), e.hostTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get event: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return decodeEvent(t, rec)
}

// dbStatus reads the status column directly (works for taken_down events too).
func (e *premiumEnv) dbStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), "SELECT status FROM events WHERE id = $1", id).Scan(&s); err != nil {
		t.Fatalf("event status: %v", err)
	}
	return s
}

// publish / unpublish / switchTemplate send the call at the event's current
// version and return the recorder so callers can assert on status and body.
func (e *premiumEnv) publish(t *testing.T, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	v := e.getEvent(t, id).Version
	return e.a.do(http.MethodPost, "/v1/events/"+id.String()+"/publish", e.hostTok, fmt.Sprintf(`{"version":%d}`, v))
}

func (e *premiumEnv) unpublish(t *testing.T, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	v := e.getEvent(t, id).Version
	return e.a.do(http.MethodPost, "/v1/events/"+id.String()+"/unpublish", e.hostTok, fmt.Sprintf(`{"version":%d}`, v))
}

func (e *premiumEnv) switchTemplate(t *testing.T, id uuid.UUID, tpl premiumTpl) *httptest.ResponseRecorder {
	t.Helper()
	v := e.getEvent(t, id).Version
	return e.a.do(http.MethodPatch, "/v1/events/"+id.String(), e.hostTok, fmt.Sprintf(`{"version":%d,"template_slug":%q}`, v, tpl.Slug))
}

func (e *premiumEnv) upgrade(t *testing.T, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	v := e.getEvent(t, id).Version
	return e.a.do(http.MethodPost, "/v1/events/"+id.String()+"/template/upgrade", e.hostTok, fmt.Sprintf(`{"version":%d}`, v))
}

func (e *premiumEnv) grant(eventID uuid.UUID, body string) *httptest.ResponseRecorder {
	return e.a.do(http.MethodPost, "/v1/admin/events/"+eventID.String()+"/payments", e.superTok, body)
}

// amount is the live price the admin would confirm for region. For a region
// the template does not offer it returns a plausible placeholder so the body
// still passes amount validation and the request reaches the region check.
func (p premiumTpl) amount(region string) int32 {
	switch region {
	case "AE":
		if p.AE != 0 {
			return p.AE
		}
		return 2900
	case "INT":
		if p.INT != 0 {
			return p.INT
		}
		return 900
	}
	return p.Price
}

func grantJSON(tpl premiumTpl, region string) string {
	return fmt.Sprintf(`{"template_id":%q,"region":%q,"amount_minor":%d}`, tpl.ID, region, tpl.amount(region))
}

// withAmount adds a valid amount_minor to a JSON object body.
func withAmount(body string) string {
	if body == "{}" {
		return `{"amount_minor":59900}`
	}
	return strings.Replace(body, "{", `{"amount_minor":59900,`, 1)
}

// mustGrant records an India payment for tpl on the event and requires 201.
func (e *premiumEnv) mustGrant(t *testing.T, eventID uuid.UUID, tpl premiumTpl) {
	t.Helper()
	wantStatusCode(t, e.grant(eventID, grantJSON(tpl, "IN")), http.StatusCreated, "")
}

func (e *premiumEnv) revoke(eventID, templateID uuid.UUID) *httptest.ResponseRecorder {
	return e.a.do(http.MethodDelete, "/v1/admin/events/"+eventID.String()+"/payments/"+templateID.String(), e.superTok, "")
}

func (e *premiumEnv) takedown(t *testing.T, id uuid.UUID) {
	t.Helper()
	wantStatusCode(t, e.a.do(http.MethodPost, "/v1/admin/events/"+id.String()+"/takedown", e.a.moderatorT, `{}`), http.StatusOK, "")
	if got := e.dbStatus(t, id); got != "taken_down" {
		t.Fatalf("status after takedown = %q", got)
	}
}

func (e *premiumEnv) restore(t *testing.T, id uuid.UUID) {
	t.Helper()
	wantStatusCode(t, e.a.do(http.MethodPost, "/v1/admin/events/"+id.String()+"/restore", e.a.moderatorT, `{}`), http.StatusOK, "")
}

func wantPaymentRequired(t *testing.T, rec *httptest.ResponseRecorder, tpl premiumTpl) {
	t.Helper()
	wantStatusCode(t, rec, http.StatusPaymentRequired, "payment_required")
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				TemplateID uuid.UUID      `json:"template_id"`
				Prices     []premiumPrice `json:"prices"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 402: %v; body = %s", err, rec.Body.String())
	}
	d := resp.Error.Details
	if d.TemplateID != tpl.ID || !reflect.DeepEqual(d.Prices, tpl.prices()) {
		t.Fatalf("402 details = %+v, want template %s prices %v", d, tpl.ID, tpl.prices())
	}
	if strings.Contains(rec.Body.String(), "amount_paise") {
		t.Errorf("402 still carries legacy amount_paise: %s", rec.Body.String())
	}
}

// paymentCount counts every row for the event, revoked ones included.
func (e *premiumEnv) paymentCount(t *testing.T, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM event_payments WHERE event_id = $1", eventID).Scan(&n); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	return n
}

func (e *premiumEnv) livePaymentCount(t *testing.T, eventID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM event_payments WHERE event_id = $1 AND revoked_at IS NULL", eventID).Scan(&n); err != nil {
		t.Fatalf("count live payments: %v", err)
	}
	return n
}

type issueView struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

// wantIssues asserts a 400 validation_failed whose details carry exactly the
// given paths (order-insensitive); codes, when non-nil, maps path -> code.
func wantIssues(t *testing.T, rec *httptest.ResponseRecorder, codes map[string]string, paths ...string) {
	t.Helper()
	wantStatusCode(t, rec, http.StatusBadRequest, "validation_failed")
	var resp struct {
		Error struct {
			Details []issueView `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body.String())
	}
	got := make([]string, 0, len(resp.Error.Details))
	for _, d := range resp.Error.Details {
		got = append(got, d.Path)
		if want, ok := codes[d.Path]; ok && want != d.Code {
			t.Errorf("issue %s code = %q, want %q", d.Path, d.Code, want)
		}
	}
	sort.Strings(got)
	want := append([]string(nil), paths...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("issue paths = %v, want %v; body = %s", got, want, rec.Body.String())
	}
}

// --- catalog ---

func TestPremium_CatalogListsFlagWithoutPrice(t *testing.T) {
	e := newPremiumEnv(t)
	rec := e.a.do(http.MethodGet, "/v1/templates?occasion="+e.tag, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Templates []map[string]json.RawMessage `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Templates) != 3 {
		t.Fatalf("templates = %d, want 3", len(resp.Templates))
	}
	for _, got := range resp.Templates {
		var slug string
		_ = json.Unmarshal(got["slug"], &slug)
		var premium bool
		switch slug {
		case e.tplA.Slug, e.tplB.Slug:
			premium = true
		case e.tplFree.Slug:
		default:
			t.Fatalf("unexpected template %q", slug)
		}
		if string(got["is_premium"]) != fmt.Sprint(premium) {
			t.Errorf("%s: is_premium = %s, want %v", slug, got["is_premium"], premium)
		}
		for _, k := range []string{"price_paise", "price", "prices", "amount_minor", "regional_prices", "premium"} {
			if _, ok := got[k]; ok {
				t.Errorf("%s: public catalog leaks %q", slug, k)
			}
		}
	}
}

// --- create / response shape ---

func TestPremium_CreateEventResponseShape(t *testing.T) {
	e := newPremiumEnv(t)

	ev := e.createEvent(t, e.tplA)
	if ev.Status != "draft" {
		t.Errorf("status = %q, want draft", ev.Status)
	}
	wantPremium(t, ev.Premium, false, e.tplA)

	free := e.createEvent(t, e.tplFree)
	if free.Premium != nil {
		t.Errorf("free template premium = %+v, want null", free.Premium)
	}

	// The raw JSON must carry an explicit null, not omit the key.
	rec := e.a.do(http.MethodGet, "/v1/events/"+free.ID.String(), e.hostTok, "")
	var raw struct {
		Event map[string]json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := raw.Event["premium"]; !ok || string(got) != "null" {
		t.Errorf("free event premium key = %q (present=%v), want null", got, ok)
	}

	// GET reflects the same state as create for the premium event.
	wantPremium(t, e.getEvent(t, ev.ID).Premium, false, e.tplA)
}

// Prices are IN first, then AE, then INT, and only regions the template offers.
func TestPremium_EventPricesOrderingAndOfferedRegions(t *testing.T) {
	e := newPremiumEnv(t)
	cases := []struct {
		name string
		tpl  premiumTpl
	}{
		{"IN AE INT", e.tplA},
		{"IN AE only", e.tplB},
		{"IN INT only", e.newTemplate(t, "prem-int", true, 10000, 4, 0, 1500)},
		{"IN only", e.newTemplate(t, "prem-in", true, 12300, 5, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := e.createEvent(t, c.tpl)
			wantPremium(t, ev.Premium, false, c.tpl)
			wantPremium(t, e.getEvent(t, ev.ID).Premium, false, c.tpl)
			// Prices are also reported identically in the 402.
			wantPaymentRequired(t, e.publish(t, ev.ID), c.tpl)
		})
	}
	// The raw order for the full template, key by key.
	ev := e.createEvent(t, e.tplA)
	rec := e.a.do(http.MethodGet, "/v1/events/"+ev.ID.String(), e.hostTok, "")
	if !strings.Contains(rec.Body.String(), `"prices":[{"region":"IN","currency":"INR","amount_minor":59900},{"region":"AE","currency":"AED","amount_minor":2900},{"region":"INT","currency":"USD","amount_minor":900}]`) {
		t.Errorf("raw prices not ordered IN, AE, INT: %s", rec.Body.String())
	}
}

// --- publish gate ---

func TestPremium_PublishGate(t *testing.T) {
	e := newPremiumEnv(t)

	ev := e.createEvent(t, e.tplA)
	wantPaymentRequired(t, e.publish(t, ev.ID), e.tplA)
	if got := e.getEvent(t, ev.ID); got.Status != "draft" {
		t.Fatalf("status after refused publish = %q, want draft", got.Status)
	}

	wantStatusCode(t, e.grant(ev.ID, fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":%d,"note":"bank transfer ref 123"}`, e.tplA.ID, e.tplA.Price)), http.StatusCreated, "")
	wantPremium(t, e.getEvent(t, ev.ID).Premium, true, e.tplA)

	rec := e.publish(t, ev.ID)
	wantStatusCode(t, rec, http.StatusOK, "")
	if got := decodeEvent(t, rec); got.Status != "published" {
		t.Fatalf("published event = %+v", got)
	} else {
		wantPremium(t, got.Premium, true, e.tplA)
	}

	wantStatusCode(t, e.unpublish(t, ev.ID), http.StatusOK, "")
	rec = e.publish(t, ev.ID)
	wantStatusCode(t, rec, http.StatusOK, "") // grant persists, republish is free
	if got := decodeEvent(t, rec); got.Status != "published" {
		t.Fatalf("republished status = %q", got.Status)
	}

	// A free template is never gated.
	free := e.createEvent(t, e.tplFree)
	rec = e.publish(t, free.ID)
	wantStatusCode(t, rec, http.StatusOK, "")
	if got := decodeEvent(t, rec); got.Premium != nil {
		t.Errorf("free published premium = %+v, want null", got.Premium)
	}

	// Stale version still reports a conflict rather than being masked by the gate for a PAID event.
	stale := e.a.do(http.MethodPost, "/v1/events/"+ev.ID.String()+"/unpublish", e.hostTok, `{"version":999}`)
	if stale.Code != http.StatusConflict {
		t.Errorf("stale unpublish status = %d, want 409", stale.Code)
	}

	// Only the owner can publish: a stranger gets 404, not a payment hint.
	stranger, _ := e.a.newTarget(t, "prem-stranger")
	tok := e.a.login(t, stranger, false)
	other := e.createEvent(t, e.tplA)
	rec = e.a.do(http.MethodPost, "/v1/events/"+other.ID.String()+"/publish", tok, fmt.Sprintf(`{"version":%d}`, other.Version))
	if rec.Code != http.StatusNotFound {
		t.Errorf("stranger publish status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// An unpaid premium publish is refused before the publish rate limit is
// consumed, so a host waiting on a manual payment never burns the allowance.
func TestPremium_UnpaidPublishDoesNotConsumeRateLimit(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)

	const dailyPublishLimit = 20 // events:publish:<user> in handlePublishEvent
	for i := 0; i < dailyPublishLimit+5; i++ {
		rec := e.publish(t, ev.ID)
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("unpaid publish #%d status = %d, want 402; body = %s", i+1, rec.Code, rec.Body.String())
		}
	}
	e.mustGrant(t, ev.ID, e.tplA)
	rec := e.publish(t, ev.ID)
	wantStatusCode(t, rec, http.StatusOK, "")
	if got := decodeEvent(t, rec); got.Status != "published" {
		t.Fatalf("status = %q, want published", got.Status)
	}
}

// --- switching templates ---

func TestPremium_TemplateSwitching(t *testing.T) {
	e := newPremiumEnv(t)

	ev := e.createEvent(t, e.tplA)
	e.mustGrant(t, ev.ID, e.tplA)
	wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")

	// Published and paid for A: moving to unpaid premium B is refused and the pin is unchanged.
	wantPaymentRequired(t, e.switchTemplate(t, ev.ID, e.tplB), e.tplB)
	if got := e.getEvent(t, ev.ID); got.Template.Slug != e.tplA.Slug || got.Status != "published" {
		t.Fatalf("after refused switch: template=%q status=%q", got.Template.Slug, got.Status)
	}

	// Free is always allowed; back to the paid A is allowed too.
	rec := e.switchTemplate(t, ev.ID, e.tplFree)
	wantStatusCode(t, rec, http.StatusOK, "")
	if got := decodeEvent(t, rec); got.Template.Slug != e.tplFree.Slug || got.Premium != nil {
		t.Fatalf("after switch to free: %+v", got)
	}
	rec = e.switchTemplate(t, ev.ID, e.tplA)
	wantStatusCode(t, rec, http.StatusOK, "")
	got := decodeEvent(t, rec)
	if got.Template.Slug != e.tplA.Slug || got.Status != "published" {
		t.Fatalf("after switch back to A: %+v", got)
	}
	wantPremium(t, got.Premium, true, e.tplA)

	// Switching an unknown template is a plain 400, not a payment error.
	v := e.getEvent(t, ev.ID).Version
	rec = e.a.do(http.MethodPatch, "/v1/events/"+ev.ID.String(), e.hostTok, fmt.Sprintf(`{"version":%d,"template_slug":"nope-%s"}`, v, e.tag))
	wantStatusCode(t, rec, http.StatusBadRequest, "unknown_template")

	// Hidden (unpublished) events behave like drafts: unpaid premium is fine.
	wantStatusCode(t, e.unpublish(t, ev.ID), http.StatusOK, "")
	rec = e.switchTemplate(t, ev.ID, e.tplB)
	wantStatusCode(t, rec, http.StatusOK, "")
	got = decodeEvent(t, rec)
	if got.Status != "hidden" {
		t.Fatalf("hidden event on unpaid B: %+v", got)
	}
	wantPremium(t, got.Premium, false, e.tplB)
	// ...but it cannot be republished until B is paid.
	wantPaymentRequired(t, e.publish(t, ev.ID), e.tplB)

	// A draft may switch to an unpaid premium template freely.
	draft := e.createEvent(t, e.tplFree)
	rec = e.switchTemplate(t, draft.ID, e.tplB)
	wantStatusCode(t, rec, http.StatusOK, "")
	got = decodeEvent(t, rec)
	if got.Template.Slug != e.tplB.Slug || got.Status != "draft" {
		t.Fatalf("draft on unpaid B: %+v", got)
	}
	wantPremium(t, got.Premium, false, e.tplB)
}

// A grant is per event: paying for template A on one event must not unlock A
// on another event, nor on the same owner's later events.
func TestPremium_GrantIsPerEvent(t *testing.T) {
	e := newPremiumEnv(t)

	paid := e.createEvent(t, e.tplA)
	other := e.createEvent(t, e.tplA)
	e.mustGrant(t, paid.ID, e.tplA)

	wantStatusCode(t, e.publish(t, paid.ID), http.StatusOK, "")
	wantPaymentRequired(t, e.publish(t, other.ID), e.tplA)
	got := e.getEvent(t, other.ID)
	if got.Status != "draft" {
		t.Fatalf("other event = %+v, want draft", got)
	}
	wantPremium(t, got.Premium, false, e.tplA)

	// Switching the other (draft) event away and back never inherits the grant either.
	wantStatusCode(t, e.switchTemplate(t, other.ID, e.tplFree), http.StatusOK, "")
	wantStatusCode(t, e.switchTemplate(t, other.ID, e.tplA), http.StatusOK, "")
	wantPaymentRequired(t, e.publish(t, other.ID), e.tplA)

	// A published free event cannot be moved onto A without its own grant.
	free := e.createEvent(t, e.tplFree)
	wantStatusCode(t, e.publish(t, free.ID), http.StatusOK, "")
	wantPaymentRequired(t, e.switchTemplate(t, free.ID, e.tplA), e.tplA)
}

// --- admin grant ---

func TestPremium_AdminGrantAuth(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)
	path := "/v1/admin/events/" + ev.ID.String() + "/payments"
	plainTok := e.a.login(t, e.a.f.plainUserID, false)
	good := grantJSON(e.tplA, "IN")

	cases := []struct {
		name   string
		method string
		path   string
		tok    string
		want   int
	}{
		{"grant anonymous", http.MethodPost, path, "", http.StatusUnauthorized},
		{"grant plain user", http.MethodPost, path, plainTok, http.StatusForbidden},
		{"grant host owner", http.MethodPost, path, e.hostTok, http.StatusForbidden},
		{"grant moderator", http.MethodPost, path, e.a.moderatorT, http.StatusForbidden},
		{"revoke anonymous", http.MethodDelete, path + "/" + e.tplA.ID.String(), "", http.StatusUnauthorized},
		{"revoke plain user", http.MethodDelete, path + "/" + e.tplA.ID.String(), plainTok, http.StatusForbidden},
		{"revoke host owner", http.MethodDelete, path + "/" + e.tplA.ID.String(), e.hostTok, http.StatusForbidden},
		{"revoke moderator", http.MethodDelete, path + "/" + e.tplA.ID.String(), e.a.moderatorT, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := ""
			if c.method == http.MethodPost {
				body = good
			}
			rec := e.a.do(c.method, c.path, c.tok, body)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
	if n := e.paymentCount(t, ev.ID); n != 0 {
		t.Fatalf("payments = %d after rejected calls, want 0", n)
	}
}

func TestPremium_AdminGrantRejections(t *testing.T) {
	e := newPremiumEnv(t)
	prem := e.createEvent(t, e.tplA)
	free := e.createEvent(t, e.tplFree)

	t.Run("unknown event", func(t *testing.T) {
		wantStatusCode(t, e.grant(uuid.Must(uuid.NewV7()), grantJSON(e.tplA, "IN")), http.StatusNotFound, "not_found")
	})
	t.Run("malformed event id", func(t *testing.T) {
		rec := e.a.do(http.MethodPost, "/v1/admin/events/not-a-uuid/payments", e.superTok, grantJSON(e.tplA, "IN"))
		wantStatusCode(t, rec, http.StatusNotFound, "not_found")
	})
	t.Run("free template", func(t *testing.T) {
		wantStatusCode(t, e.grant(free.ID, grantJSON(e.tplFree, "IN")), http.StatusConflict, "not_premium")
		if n := e.paymentCount(t, free.ID); n != 0 {
			t.Fatalf("payments on free event = %d", n)
		}
	})
	t.Run("template of another premium", func(t *testing.T) {
		wantStatusCode(t, e.grant(prem.ID, grantJSON(e.tplB, "IN")), http.StatusConflict, "template_changed")
		wantStatusCode(t, e.grant(prem.ID, grantJSON(e.tplFree, "IN")), http.StatusConflict, "template_changed")
	})
	t.Run("unknown template id", func(t *testing.T) {
		wantStatusCode(t, e.grant(prem.ID, fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":59900}`, uuid.Must(uuid.NewV7()))), http.StatusConflict, "template_changed")
	})

	bad := []struct{ name, body string }{
		{"note too long", `{"note":"` + strings.Repeat("x", 201) + `"`},
		{"note multibyte too long", `{"note":"` + strings.Repeat("é", 201) + `"`},
		{"note newline", `{"note":"a\nb"`},
		{"note nul", `{"note":"a\u0000b"`},
		{"note tab", `{"note":"a\tb"`},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			body := c.body + fmt.Sprintf(`,"template_id":%q,"region":"IN","amount_minor":%d}`, e.tplA.ID, e.tplA.Price)
			wantIssues(t, e.grant(prem.ID, body), map[string]string{"note": "invalid"}, "note")
		})
	}
	for _, c := range []struct{ name, body string }{
		{"note not a string", fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":59900,"note":5}`, e.tplA.ID)},
		{"unknown field", fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":59900,"bogus":1}`, e.tplA.ID)},
		{"legacy amount field", fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":59900,"amount_paise":59900}`, e.tplA.ID)},
		{"template_id not a string", `{"template_id":5,"region":"IN","amount_minor":59900}`},
		{"region not a string", fmt.Sprintf(`{"template_id":%q,"region":1,"amount_minor":59900}`, e.tplA.ID)},
		{"malformed json", `{"note":`},
		{"empty body", ``},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := e.grant(prem.ID, c.body)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 400/415; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	if n := e.paymentCount(t, prem.ID); n != 0 {
		t.Fatalf("payment recorded despite rejected requests: %d", n)
	}

	t.Run("oversized body", func(t *testing.T) {
		rec := e.grant(prem.ID, `{"note":"`+strings.Repeat("x", 70<<10)+`"}`)
		if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 413/400", rec.Code)
		}
	})

	t.Run("note exactly 200 runes and null note", func(t *testing.T) {
		rec := e.grant(prem.ID, fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":%d,"note":"%s"}`, e.tplA.ID, e.tplA.Price, strings.Repeat("é", 200)))
		wantStatusCode(t, rec, http.StatusCreated, "")
		var resp struct {
			Payment adminPaymentResp `json:"payment"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		p := resp.Payment
		if p.TemplateID != e.tplA.ID || p.TemplateName != "prem-a" || p.Region != "IN" || p.Currency != "INR" ||
			p.AmountMinor != e.tplA.Price || len([]rune(p.Note)) != 200 || p.CreatedAt.IsZero() {
			t.Fatalf("payment = %+v", p)
		}
		// Second grant for the same pair is refused, whatever the region.
		wantStatusCode(t, e.grant(prem.ID, grantJSON(e.tplA, "IN")), http.StatusConflict, "already_paid")
		wantStatusCode(t, e.grant(prem.ID, grantJSON(e.tplA, "AE")), http.StatusConflict, "already_paid")
		if n := e.paymentCount(t, prem.ID); n != 1 {
			t.Fatalf("payments = %d, want 1", n)
		}

		// null note on a fresh event is accepted as an empty note.
		fresh := e.createEvent(t, e.tplA)
		rec = e.grant(fresh.ID, fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":%d,"note":null}`, e.tplA.ID, e.tplA.Price))
		wantStatusCode(t, rec, http.StatusCreated, "")
		if !strings.Contains(rec.Body.String(), `"note":""`) {
			t.Errorf("null note body = %s, want empty note", rec.Body.String())
		}
	})
}

func TestPremium_AdminGrantBodyValidationPaths(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)
	tid := e.tplA.ID.String()
	long := strings.Repeat("x", 201)

	cases := []struct {
		name  string
		body  string
		paths []string
	}{
		{"empty object", `{}`, []string{"template_id", "region"}},
		{"missing template_id", `{"region":"IN"}`, []string{"template_id"}},
		{"missing region", fmt.Sprintf(`{"template_id":%q}`, tid), []string{"region"}},
		{"null template_id", `{"template_id":null,"region":"IN"}`, []string{"template_id"}},
		{"null region", fmt.Sprintf(`{"template_id":%q,"region":null}`, tid), []string{"region"}},
		{"empty template_id", `{"template_id":"","region":"IN"}`, []string{"template_id"}},
		{"empty region", fmt.Sprintf(`{"template_id":%q,"region":""}`, tid), []string{"region"}},
		{"template_id not a uuid", `{"template_id":"not-a-uuid","region":"IN"}`, []string{"template_id"}},
		{"lower-case region", fmt.Sprintf(`{"template_id":%q,"region":"ae"}`, tid), []string{"region"}},
		{"unknown region", fmt.Sprintf(`{"template_id":%q,"region":"US"}`, tid), []string{"region"}},
		{"region with space", fmt.Sprintf(`{"template_id":%q,"region":"IN "}`, tid), []string{"region"}},
		{"note only bad", fmt.Sprintf(`{"template_id":%q,"region":"IN","note":%q}`, tid, long), []string{"note"}},
		{"all three bad", fmt.Sprintf(`{"template_id":"zzz","region":"XX","note":%q}`, long), []string{"template_id", "region", "note"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantIssues(t, e.grant(ev.ID, withAmount(c.body)), nil, c.paths...)
		})
	}
	// Required paths use the "required" code, malformed ones "invalid".
	wantIssues(t, e.grant(ev.ID, withAmount(`{}`)), map[string]string{"template_id": "required", "region": "required"}, "template_id", "region")
	wantIssues(t, e.grant(ev.ID, withAmount(`{"template_id":"x","region":"x"}`)), map[string]string{"template_id": "invalid", "region": "invalid"}, "template_id", "region")

	if n := e.paymentCount(t, ev.ID); n != 0 {
		t.Fatalf("payments = %d after validation failures, want 0", n)
	}
	if n := len(e.a.auditRows(t, "event", ev.ID)); n != 0 {
		t.Fatalf("audit rows = %d after validation failures, want 0", n)
	}
}

func TestPremium_AdminGrantAmountValidation(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)
	base := fmt.Sprintf(`"template_id":%q,"region":"IN"`, e.tplA.ID)

	for _, c := range []struct {
		name string
		body string
		code string
	}{
		{"missing", `{` + base + `}`, "required"},
		{"null", `{` + base + `,"amount_minor":null}`, "required"},
		{"string", `{` + base + `,"amount_minor":"59900"}`, "invalid"},
		{"bool", `{` + base + `,"amount_minor":true}`, "invalid"},
		{"fraction", `{` + base + `,"amount_minor":599.5}`, "invalid"},
		{"array", `{` + base + `,"amount_minor":[]}`, "invalid"},
		{"too low", `{` + base + `,"amount_minor":99}`, "out_of_range"},
		{"zero", `{` + base + `,"amount_minor":0}`, "out_of_range"},
		{"negative", `{` + base + `,"amount_minor":-59900}`, "out_of_range"},
		{"too high", `{` + base + `,"amount_minor":1000001}`, "out_of_range"},
		{"int32 overflow", `{` + base + `,"amount_minor":2147483648}`, "out_of_range"},
		{"int64 overflow", `{` + base + `,"amount_minor":99999999999999999999}`, "out_of_range"},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantIssues(t, e.grant(ev.ID, c.body), map[string]string{"amount_minor": c.code}, "amount_minor")
		})
	}
	// Reported together with the other bad fields.
	wantIssues(t, e.grant(ev.ID, `{"template_id":"x","region":"x","amount_minor":"x","note":"a\nb"}`), nil,
		"template_id", "region", "amount_minor", "note")
	if n := e.paymentCount(t, ev.ID); n != 0 {
		t.Fatalf("payments = %d after validation failures, want 0", n)
	}
}

func TestPremium_AdminGrantPriceChanged(t *testing.T) {
	e := newPremiumEnv(t)

	for _, c := range []struct {
		region string
		live   int32
	}{{"IN", e.tplA.Price}, {"AE", e.tplA.AE}, {"INT", e.tplA.INT}} {
		t.Run("wrong amount "+c.region, func(t *testing.T) {
			ev := e.createEvent(t, e.tplA)
			for _, amt := range []int32{c.live - 1, c.live + 1, 100, 1000000} {
				body := fmt.Sprintf(`{"template_id":%q,"region":%q,"amount_minor":%d}`, e.tplA.ID, c.region, amt)
				wantStatusCode(t, e.grant(ev.ID, body), http.StatusConflict, "price_changed")
			}
			if n := e.paymentCount(t, ev.ID); n != 0 {
				t.Fatalf("payments = %d after price_changed, want 0", n)
			}
			if n := len(e.a.auditRows(t, "event", ev.ID)); n != 0 {
				t.Fatalf("audit rows = %d after price_changed, want 0", n)
			}
			wantPaymentRequired(t, e.publish(t, ev.ID), e.tplA)
			// The exact live amount works.
			body := fmt.Sprintf(`{"template_id":%q,"region":%q,"amount_minor":%d}`, e.tplA.ID, c.region, c.live)
			wantStatusCode(t, e.grant(ev.ID, body), http.StatusCreated, "")
		})
	}

	t.Run("price edited after the admin loaded the page", func(t *testing.T) {
		tpl := e.newTemplate(t, "prem-edit", true, 30000, 9, 2000, 700)
		ev := e.createEvent(t, tpl)
		stale := grantJSON(tpl, "IN")
		staleAE := grantJSON(tpl, "AE")
		wantStatusCode(t, e.a.do(http.MethodPatch, "/v1/admin/templates/"+tpl.ID.String(), e.superTok, `{"price_paise":35000,"regional_prices":{"AE":2500}}`), http.StatusOK, "")
		wantStatusCode(t, e.grant(ev.ID, stale), http.StatusConflict, "price_changed")
		wantStatusCode(t, e.grant(ev.ID, staleAE), http.StatusConflict, "price_changed")
		if n := e.paymentCount(t, ev.ID); n != 0 {
			t.Fatalf("payments = %d, want 0", n)
		}
		fresh := tpl
		fresh.Price, fresh.AE = 35000, 2500
		wantStatusCode(t, e.grant(ev.ID, grantJSON(fresh, "AE")), http.StatusCreated, "")
		var amount int32
		if err := e.pool.QueryRow(context.Background(), "SELECT amount_minor FROM event_payments WHERE event_id = $1", ev.ID).Scan(&amount); err != nil || amount != 2500 {
			t.Fatalf("recorded amount = %d, %v; want 2500", amount, err)
		}
	})

	t.Run("unoffered region wins over a wrong amount", func(t *testing.T) {
		ev := e.createEvent(t, e.tplB) // no INT price
		body := fmt.Sprintf(`{"template_id":%q,"region":"INT","amount_minor":12345}`, e.tplB.ID)
		wantStatusCode(t, e.grant(ev.ID, body), http.StatusConflict, "region_unavailable")
	})
	t.Run("template_changed and not_premium win over a wrong amount", func(t *testing.T) {
		ev := e.createEvent(t, e.tplA)
		body := fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":12345}`, e.tplB.ID)
		wantStatusCode(t, e.grant(ev.ID, body), http.StatusConflict, "template_changed")
		free := e.createEvent(t, e.tplFree)
		body = fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":12345}`, e.tplFree.ID)
		wantStatusCode(t, e.grant(free.ID, body), http.StatusConflict, "not_premium")
	})
}

// Grants are refused for events that are gone or never owned.
func TestPremium_AdminGrantUnavailableEvents(t *testing.T) {
	e := newPremiumEnv(t)
	ctx := context.Background()

	t.Run("soft-deleted event", func(t *testing.T) {
		ev := e.createEvent(t, e.tplA)
		if _, err := e.pool.Exec(ctx, "UPDATE events SET deleted_at = now() WHERE id = $1", ev.ID); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplA, "IN")), http.StatusNotFound, "not_found")
		if n := e.paymentCount(t, ev.ID); n != 0 {
			t.Fatalf("payments = %d on soft-deleted event", n)
		}
		if n := len(e.a.auditRows(t, "event", ev.ID)); n != 0 {
			t.Fatalf("audit rows = %d on soft-deleted event", n)
		}
	})

	t.Run("anonymous draft", func(t *testing.T) {
		rec := e.a.do(http.MethodPost, "/v1/events", "", fmt.Sprintf(`{"occasion_slug":"birthday","template_slug":%q,"answers":{"name":"Sam","date":%q,"venue":"The Hall"}}`, e.tplA.Slug, seedTestDate))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create anonymous draft: status = %d, body = %s", rec.Code, rec.Body.String())
		}
		ev := decodeEvent(t, rec)
		var owner *uuid.UUID
		if err := e.pool.QueryRow(ctx, "SELECT owner_id FROM events WHERE id = $1", ev.ID).Scan(&owner); err != nil || owner != nil {
			t.Fatalf("owner_id = %v, %v; want NULL for an anonymous draft", owner, err)
		}
		wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplA, "IN")), http.StatusNotFound, "not_found")
		if n := e.paymentCount(t, ev.ID); n != 0 {
			t.Fatalf("payments = %d on anonymous draft", n)
		}
	})
}

func TestPremium_AdminGrantRegions(t *testing.T) {
	e := newPremiumEnv(t)

	happy := []struct {
		region   string
		currency string
		amount   int32
	}{
		{"IN", "INR", e.tplA.Price},
		{"AE", "AED", e.tplA.AE},
		{"INT", "USD", e.tplA.INT},
	}
	for _, c := range happy {
		t.Run("grant "+c.region, func(t *testing.T) {
			ev := e.createEvent(t, e.tplA)
			rec := e.grant(ev.ID, fmt.Sprintf(`{"template_id":%q,"region":%q,"amount_minor":%d,"note":"ref %s"}`, e.tplA.ID, c.region, c.amount, c.region))
			wantStatusCode(t, rec, http.StatusCreated, "")
			var resp struct {
				Payment adminPaymentResp `json:"payment"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			p := resp.Payment
			if p.TemplateID != e.tplA.ID || p.TemplateName != "prem-a" || p.Region != c.region || p.Currency != c.currency ||
				p.AmountMinor != c.amount || p.Note != "ref "+c.region || p.CreatedAt.IsZero() {
				t.Fatalf("payment = %+v, want %s %s %d", p, c.region, c.currency, c.amount)
			}
			if strings.Contains(rec.Body.String(), "amount_paise") {
				t.Errorf("response carries legacy amount_paise: %s", rec.Body.String())
			}
			// Whatever region was sold, the event is paid and publishable.
			wantPremium(t, e.getEvent(t, ev.ID).Premium, true, e.tplA)
			wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")

			var region, currency string
			var amount int32
			if err := e.pool.QueryRow(context.Background(),
				"SELECT region, currency, amount_minor FROM event_payments WHERE event_id = $1", ev.ID).Scan(&region, &currency, &amount); err != nil {
				t.Fatalf("query payment: %v", err)
			}
			if region != c.region || currency != c.currency || amount != c.amount {
				t.Errorf("stored = %s %s %d, want %s %s %d", region, currency, amount, c.region, c.currency, c.amount)
			}
		})
	}

	t.Run("region not offered", func(t *testing.T) {
		ev := e.createEvent(t, e.tplB) // B has no INT price
		wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplB, "INT")), http.StatusConflict, "region_unavailable")
		if n := e.paymentCount(t, ev.ID); n != 0 {
			t.Fatalf("payments = %d after region_unavailable", n)
		}
		if n := len(e.a.auditRows(t, "event", ev.ID)); n != 0 {
			t.Fatalf("audit rows = %d after region_unavailable", n)
		}
		wantPaymentRequired(t, e.publish(t, ev.ID), e.tplB)
		// The offered regions still work.
		wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplB, "AE")), http.StatusCreated, "")
	})

	t.Run("India always available", func(t *testing.T) {
		only := e.newTemplate(t, "prem-in-only", true, 20000, 6, 0, 0)
		ev := e.createEvent(t, only)
		wantStatusCode(t, e.grant(ev.ID, grantJSON(only, "AE")), http.StatusConflict, "region_unavailable")
		wantStatusCode(t, e.grant(ev.ID, grantJSON(only, "INT")), http.StatusConflict, "region_unavailable")
		wantStatusCode(t, e.grant(ev.ID, grantJSON(only, "IN")), http.StatusCreated, "")
	})

	t.Run("region withdrawn by admin", func(t *testing.T) {
		tpl := e.newTemplate(t, "prem-withdrawn", true, 20000, 7, 2000, 700)
		ev := e.createEvent(t, tpl)
		rec := e.a.do(http.MethodPatch, "/v1/admin/templates/"+tpl.ID.String(), e.superTok, `{"regional_prices":{"AE":null}}`)
		wantStatusCode(t, rec, http.StatusOK, "")
		wantStatusCode(t, e.grant(ev.ID, grantJSON(tpl, "AE")), http.StatusConflict, "region_unavailable")
		wantStatusCode(t, e.grant(ev.ID, grantJSON(tpl, "INT")), http.StatusCreated, "")
	})

	t.Run("one live grant per template across regions", func(t *testing.T) {
		ev := e.createEvent(t, e.tplA)
		wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplA, "AE")), http.StatusCreated, "")
		for _, region := range []string{"AE", "IN", "INT"} {
			wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplA, region)), http.StatusConflict, "already_paid")
		}
		if n := e.paymentCount(t, ev.ID); n != 1 {
			t.Fatalf("payments = %d, want 1", n)
		}
	})
}

// A grant naming a template the draft has since moved off must not land on
// the new template, nor be recorded against the old one.
func TestPremium_AdminGrantTemplateChanged(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)
	staleBody := grantJSON(e.tplA, "IN") // the admin UI loaded the event while it was on A

	wantStatusCode(t, e.switchTemplate(t, ev.ID, e.tplB), http.StatusOK, "") // draft: allowed

	wantStatusCode(t, e.grant(ev.ID, staleBody), http.StatusConflict, "template_changed")
	if n := e.paymentCount(t, ev.ID); n != 0 {
		t.Fatalf("payments = %d after template_changed, want 0", n)
	}
	if n := len(e.a.auditRows(t, "event", ev.ID)); n != 0 {
		t.Fatalf("audit rows = %d after template_changed, want 0", n)
	}
	wantPaymentRequired(t, e.publish(t, ev.ID), e.tplB)

	// Naming the current template works.
	wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplB, "IN")), http.StatusCreated, "")
	wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
}

func TestPremium_AdminGrantAuditHasNoNoteText(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)
	const secretNote = "called +91 98765 43210 about transfer"

	wantStatusCode(t, e.grant(ev.ID, fmt.Sprintf(`{"template_id":%q,"region":"AE","amount_minor":%d,"note":%q}`, e.tplA.ID, e.tplA.AE, secretNote)), http.StatusCreated, "")
	rows := e.a.auditRows(t, "event", ev.ID)
	if len(rows) != 1 || rows[0].Action != "event.payment.grant" {
		t.Fatalf("audit rows = %+v", rows)
	}
	if rows[0].ActorID == nil || *rows[0].ActorID != e.a.f.superAdminID {
		t.Errorf("actor = %v, want super admin", rows[0].ActorID)
	}
	after := string(rows[0].After)
	if strings.Contains(after, "98765") || strings.Contains(after, "transfer") {
		t.Fatalf("audit leaks note text: %s", after)
	}
	var m map[string]any
	if err := json.Unmarshal(rows[0].After, &m); err != nil {
		t.Fatalf("decode audit after: %v", err)
	}
	if m["has_note"] != true || m["template_id"] != e.tplA.ID.String() || m["region"] != "AE" ||
		m["currency"] != "AED" || m["amount_minor"] != float64(e.tplA.AE) {
		t.Errorf("audit after = %v", m)
	}
	if _, ok := m["amount_paise"]; ok {
		t.Errorf("audit still carries amount_paise: %v", m)
	}
	if _, ok := m["note"]; ok {
		t.Errorf("audit carries the note itself: %v", m)
	}

	// A refused grant writes no audit row.
	wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplA, "AE")), http.StatusConflict, "already_paid")
	if n := len(e.a.auditRows(t, "event", ev.ID)); n != 1 {
		t.Errorf("audit rows after refused grant = %d, want 1", n)
	}

	// No note: has_note is false.
	ev2 := e.createEvent(t, e.tplA)
	e.mustGrant(t, ev2.ID, e.tplA)
	rows = e.a.auditRows(t, "event", ev2.ID)
	if len(rows) != 1 || !strings.Contains(string(rows[0].After), `"has_note": false`) && !strings.Contains(string(rows[0].After), `"has_note":false`) {
		t.Errorf("audit without note = %+v", rows)
	}
}

func TestPremium_AdminGrantSnapshotsPrice(t *testing.T) {
	e := newPremiumEnv(t)
	inEv := e.createEvent(t, e.tplA)
	aeEv := e.createEvent(t, e.tplA)
	e.mustGrant(t, inEv.ID, e.tplA)
	wantStatusCode(t, e.grant(aeEv.ID, grantJSON(e.tplA, "AE")), http.StatusCreated, "")

	rec := e.a.do(http.MethodPatch, "/v1/admin/templates/"+e.tplA.ID.String(), e.superTok, `{"price_paise":99900,"regional_prices":{"AE":5000}}`)
	wantStatusCode(t, rec, http.StatusOK, "")

	amount := func(id uuid.UUID) int32 {
		var a int32
		if err := e.pool.QueryRow(context.Background(), "SELECT amount_minor FROM event_payments WHERE event_id = $1", id).Scan(&a); err != nil {
			t.Fatalf("query payment: %v", err)
		}
		return a
	}
	if got := amount(inEv.ID); got != e.tplA.Price {
		t.Errorf("IN amount = %d after price change, want snapshot %d", got, e.tplA.Price)
	}
	if got := amount(aeEv.ID); got != e.tplA.AE {
		t.Errorf("AE amount = %d after price change, want snapshot %d", got, e.tplA.AE)
	}
	// The host sees the new list prices on a still-unpaid sibling event.
	sib := e.createEvent(t, e.tplA)
	updated := e.tplA
	updated.Price, updated.AE = 99900, 5000
	wantPremium(t, e.getEvent(t, sib.ID).Premium, false, updated)
	// A new grant is taken at the new price.
	wantStatusCode(t, e.grant(sib.ID, grantJSON(e.tplA, "AE")), http.StatusConflict, "price_changed") // stale list price
	wantStatusCode(t, e.grant(sib.ID, grantJSON(updated, "AE")), http.StatusCreated, "")
	if got := amount(sib.ID); got != 5000 {
		t.Errorf("new AE grant amount = %d, want 5000", got)
	}
}

func TestPremium_AdminGrantConcurrent(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)

	const n = 6
	regions := []string{"IN", "AE", "INT"}
	codes := make([]int, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := e.grant(ev.ID, grantJSON(e.tplA, regions[i%len(regions)]))
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}()
	}
	wg.Wait()
	created, conflict := 0, 0
	for i, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
			if !strings.Contains(bodies[i], "already_paid") {
				t.Errorf("conflict body = %s, want already_paid", bodies[i])
			}
		}
	}
	if created != 1 || conflict != n-1 {
		t.Fatalf("codes = %v, want exactly one 201 and %d 409", codes, n-1)
	}
	if got := e.paymentCount(t, ev.ID); got != 1 {
		t.Fatalf("payments = %d, want 1", got)
	}
	if rows := e.a.auditRows(t, "event", ev.ID); len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
}

// --- admin revoke (soft) ---

func TestPremium_AdminRevoke(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)

	t.Run("no grant", func(t *testing.T) {
		wantStatusCode(t, e.revoke(ev.ID, e.tplA.ID), http.StatusNotFound, "not_found")
	})
	t.Run("malformed ids", func(t *testing.T) {
		rec := e.a.do(http.MethodDelete, "/v1/admin/events/not-a-uuid/payments/"+e.tplA.ID.String(), e.superTok, "")
		wantStatusCode(t, rec, http.StatusNotFound, "not_found")
		rec = e.a.do(http.MethodDelete, "/v1/admin/events/"+ev.ID.String()+"/payments/not-a-uuid", e.superTok, "")
		wantStatusCode(t, rec, http.StatusNotFound, "not_found")
	})

	wantStatusCode(t, e.grant(ev.ID, fmt.Sprintf(`{"template_id":%q,"region":"AE","amount_minor":%d,"note":"first"}`, e.tplA.ID, e.tplA.AE)), http.StatusCreated, "")
	wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")

	t.Run("wrong template leaves grant", func(t *testing.T) {
		wantStatusCode(t, e.revoke(ev.ID, e.tplB.ID), http.StatusNotFound, "not_found")
		if n := e.livePaymentCount(t, ev.ID); n != 1 {
			t.Fatalf("live payments = %d, want 1", n)
		}
	})

	t.Run("revoke is soft, keeps event published, republish is gated", func(t *testing.T) {
		rec := e.revoke(ev.ID, e.tplA.ID)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Errorf("204 body = %q, want empty", rec.Body.String())
		}
		got := e.getEvent(t, ev.ID)
		if got.Status != "published" {
			t.Fatalf("status after revoke = %q, want published (revoke does not unpublish)", got.Status)
		}
		wantPremium(t, got.Premium, false, e.tplA)

		// The row stays, stamped with who revoked it and when.
		if n := e.paymentCount(t, ev.ID); n != 1 {
			t.Fatalf("payment rows = %d after revoke, want 1 (soft revoke)", n)
		}
		var revokedAt *time.Time
		var revokedBy *uuid.UUID
		if err := e.pool.QueryRow(context.Background(),
			"SELECT revoked_at, revoked_by FROM event_payments WHERE event_id = $1", ev.ID).Scan(&revokedAt, &revokedBy); err != nil {
			t.Fatalf("query revoked row: %v", err)
		}
		if revokedAt == nil || revokedBy == nil || *revokedBy != e.a.f.superAdminID {
			t.Fatalf("revoked_at=%v revoked_by=%v, want set by super admin", revokedAt, revokedBy)
		}

		// Live list excludes it, history includes it.
		q := store.New(e.pool)
		live, err := q.ListEventPayments(context.Background(), ev.ID)
		if err != nil || len(live) != 0 {
			t.Fatalf("live payments = %+v, %v; want none", live, err)
		}
		hist, err := q.ListEventPaymentHistory(context.Background(), ev.ID)
		if err != nil || len(hist) != 1 || hist[0].RevokedAt == nil {
			t.Fatalf("history = %+v, %v; want the revoked row", hist, err)
		}

		// A revoked unlock does not count: republishing is gated again.
		wantStatusCode(t, e.unpublish(t, ev.ID), http.StatusOK, "")
		wantPaymentRequired(t, e.publish(t, ev.ID), e.tplA)

		// Second revoke: no live grant left.
		wantStatusCode(t, e.revoke(ev.ID, e.tplA.ID), http.StatusNotFound, "not_found")

		// Re-granting after a revoke works again, in any region, and a second live grant is still refused.
		wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplA, "INT")), http.StatusCreated, "")
		wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplA, "IN")), http.StatusConflict, "already_paid")
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
		if n, live := e.paymentCount(t, ev.ID), e.livePaymentCount(t, ev.ID); n != 2 || live != 1 {
			t.Fatalf("rows=%d live=%d, want 2 and 1", n, live)
		}
		hist, err = q.ListEventPaymentHistory(context.Background(), ev.ID)
		if err != nil || len(hist) != 2 {
			t.Fatalf("history = %+v, %v; want 2 rows", hist, err)
		}
		live, err = q.ListEventPayments(context.Background(), ev.ID)
		if err != nil || len(live) != 1 || live[0].Region != "INT" || live[0].RevokedAt != nil {
			t.Fatalf("live = %+v, %v; want the INT grant", live, err)
		}
	})

	t.Run("audit", func(t *testing.T) {
		var actions []string
		var revokeBefore []byte
		for _, r := range e.a.auditRows(t, "event", ev.ID) {
			actions = append(actions, r.Action)
			if r.Action == "event.payment.revoke" {
				revokeBefore = r.Before
			}
		}
		// newest first: grant, revoke, grant
		want := []string{"event.payment.grant", "event.payment.revoke", "event.payment.grant"}
		if strings.Join(actions, ",") != strings.Join(want, ",") {
			t.Fatalf("audit actions = %v, want %v", actions, want)
		}
		var before map[string]any
		if err := json.Unmarshal(revokeBefore, &before); err != nil {
			t.Fatalf("decode revoke before: %v; raw = %s", err, revokeBefore)
		}
		for _, k := range []string{"payment_id", "granted_by", "granted_at", "has_note", "region", "currency", "amount_minor", "template_id"} {
			if _, ok := before[k]; !ok {
				t.Errorf("revoke audit before lacks %q: %v", k, before)
			}
		}
		if before["template_id"] != e.tplA.ID.String() || before["region"] != "AE" || before["currency"] != "AED" ||
			before["amount_minor"] != float64(e.tplA.AE) || before["has_note"] != true || before["granted_by"] != e.a.f.superAdminID.String() {
			t.Errorf("revoke audit before = %v", before)
		}
		if strings.Contains(string(revokeBefore), "first") {
			t.Errorf("revoke audit leaks note text: %s", revokeBefore)
		}
	})
}

// A revoked premium event cannot be republished, switched or upgraded until
// paid again; the refusal is a 402, never a version conflict.
func TestPremium_RevokedPublishedEventIsGated(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)
	e.mustGrant(t, ev.ID, e.tplA)
	wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
	wantStatusCode(t, e.revoke(ev.ID, e.tplA.ID), http.StatusNoContent, "")

	if got := e.getEvent(t, ev.ID); got.Status != "published" {
		t.Fatalf("status = %q, want published", got.Status)
	}
	wantPaymentRequired(t, e.upgrade(t, ev.ID), e.tplA)
	wantPaymentRequired(t, e.switchTemplate(t, ev.ID, e.tplA), e.tplA) // same template, no longer paid
	wantPaymentRequired(t, e.switchTemplate(t, ev.ID, e.tplB), e.tplB)
	if got := e.getEvent(t, ev.ID); got.Template.Slug != e.tplA.Slug {
		t.Fatalf("template after refused switch = %q", got.Template.Slug)
	}
	// Moving to a free template is still allowed.
	wantStatusCode(t, e.switchTemplate(t, ev.ID, e.tplFree), http.StatusOK, "")

	// And after unpublishing a revoked event, republish is refused.
	ev2 := e.createEvent(t, e.tplA)
	e.mustGrant(t, ev2.ID, e.tplA)
	wantStatusCode(t, e.publish(t, ev2.ID), http.StatusOK, "")
	wantStatusCode(t, e.revoke(ev2.ID, e.tplA.ID), http.StatusNoContent, "")
	wantStatusCode(t, e.unpublish(t, ev2.ID), http.StatusOK, "")
	wantPaymentRequired(t, e.publish(t, ev2.ID), e.tplA)
	if got := e.getEvent(t, ev2.ID); got.Status != "hidden" {
		t.Fatalf("status = %q, want hidden", got.Status)
	}
}

// A grant for an event that is purged later must outlive it (financial record),
// revoked or not.
func TestPremium_PaymentSurvivesEventPurge(t *testing.T) {
	e := newPremiumEnv(t)
	live := e.createEvent(t, e.tplA)
	revoked := e.createEvent(t, e.tplA)
	wantStatusCode(t, e.grant(live.ID, grantJSON(e.tplA, "AE")), http.StatusCreated, "")
	e.mustGrant(t, revoked.ID, e.tplA)
	wantStatusCode(t, e.revoke(revoked.ID, e.tplA.ID), http.StatusNoContent, "")

	for _, id := range []uuid.UUID{live.ID, revoked.ID} {
		if _, err := e.pool.Exec(context.Background(), "DELETE FROM events WHERE id = $1", id); err != nil {
			t.Fatalf("purge event: %v", err)
		}
	}
	var n, nRevoked int
	if err := e.pool.QueryRow(context.Background(),
		"SELECT count(*), count(revoked_at) FROM event_payments WHERE template_id = $1 AND event_id IS NULL", e.tplA.ID).Scan(&n, &nRevoked); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 || nRevoked != 1 {
		t.Fatalf("orphaned payments = %d (revoked %d), want 2 (1)", n, nRevoked)
	}
}

// --- admin event detail ---

func TestPremium_AdminEventDetail(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)
	path := "/v1/admin/events/" + ev.ID.String()

	type detail struct {
		Premium  *premiumInfo       `json:"premium"`
		Payments []adminPaymentResp `json:"payments"`
		Event    premiumEventView   `json:"event"`
	}
	get := func(tok string) (detail, *httptest.ResponseRecorder) {
		rec := e.a.do(http.MethodGet, path, tok, "")
		var d detail
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
				t.Fatalf("decode: %v; body = %s", err, rec.Body.String())
			}
		}
		return d, rec
	}

	d, _ := get(e.superTok)
	wantPremium(t, d.Premium, false, e.tplA)
	if d.Payments == nil || len(d.Payments) != 0 {
		t.Fatalf("payments = %#v, want empty non-nil slice", d.Payments)
	}
	wantPremium(t, d.Event.Premium, false, e.tplA)

	wantStatusCode(t, e.grant(ev.ID, fmt.Sprintf(`{"template_id":%q,"region":"AE","amount_minor":%d,"note":"upi 42"}`, e.tplA.ID, e.tplA.AE)), http.StatusCreated, "")
	d, _ = get(e.superTok)
	wantPremium(t, d.Premium, true, e.tplA)
	if len(d.Payments) != 1 {
		t.Fatalf("payments = %+v", d.Payments)
	}
	p := d.Payments[0]
	if p.TemplateID != e.tplA.ID || p.TemplateName != "prem-a" || p.Region != "AE" || p.Currency != "AED" ||
		p.AmountMinor != e.tplA.AE || p.Note != "upi 42" || p.CreatedAt.IsZero() {
		t.Fatalf("payment = %+v", p)
	}

	// The event can also be reached by moderators (read access), and free events show null premium.
	if _, rec := get(e.a.moderatorT); rec.Code != http.StatusOK {
		t.Errorf("moderator detail status = %d, want 200", rec.Code)
	}
	free := e.createEvent(t, e.tplFree)
	rec := e.a.do(http.MethodGet, "/v1/admin/events/"+free.ID.String(), e.superTok, "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["premium"]) != "null" || string(raw["payments"]) != "[]" {
		t.Errorf("free event premium=%s payments=%s, want null and []", raw["premium"], raw["payments"])
	}

	// A revoked payment drops out of the detail and the event is unpaid again.
	wantStatusCode(t, e.revoke(ev.ID, e.tplA.ID), http.StatusNoContent, "")
	d, _ = get(e.superTok)
	wantPremium(t, d.Premium, false, e.tplA)
	if len(d.Payments) != 0 {
		t.Fatalf("payments after revoke = %+v, want none", d.Payments)
	}
}

// --- rate limits ---

func TestPremium_AdminPaymentRateLimits(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)
	id := e.a.f.superAdminID.String()

	e.a.exhaust(t, "admin:payment:grant:"+id, 60, time.Hour)
	rec := e.grant(ev.ID, grantJSON(e.tplA, "IN"))
	wantStatusCode(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("missing Retry-After")
	}
	if n := e.paymentCount(t, ev.ID); n != 0 {
		t.Fatalf("payment recorded despite rate limit")
	}
	// Validation runs before the limiter, so a malformed body is still a 400 here.
	wantStatusCode(t, e.grant(ev.ID, `{}`), http.StatusBadRequest, "validation_failed")

	// Grant limit does not block revoke (separate key).
	wantStatusCode(t, e.revoke(ev.ID, e.tplA.ID), http.StatusNotFound, "not_found")
	e.a.exhaust(t, "admin:payment:revoke:"+id, 60, time.Hour)
	wantStatusCode(t, e.revoke(ev.ID, e.tplA.ID), http.StatusTooManyRequests, "rate_limited")
}

// --- admin templates: prices ---

type tplPricesView struct {
	ID             uuid.UUID `json:"id"`
	IsPremium      bool      `json:"is_premium"`
	PricePaise     int32     `json:"price_paise"`
	RegionalPrices struct {
		AE  *int32 `json:"AE"`
		INT *int32 `json:"INT"`
	} `json:"regional_prices"`
}

func decodeTplPrices(t *testing.T, rec *httptest.ResponseRecorder) tplPricesView {
	t.Helper()
	var v tplPricesView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode template: %v; body = %s", err, rec.Body.String())
	}
	return v
}

func wantRegional(t *testing.T, v tplPricesView, ae, intl *int32) {
	t.Helper()
	eq := func(a, b *int32) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	if !eq(v.RegionalPrices.AE, ae) || !eq(v.RegionalPrices.INT, intl) {
		show := func(p *int32) string {
			if p == nil {
				return "null"
			}
			return fmt.Sprint(*p)
		}
		t.Fatalf("regional_prices = {AE:%s INT:%s}, want {AE:%s INT:%s}", show(v.RegionalPrices.AE), show(v.RegionalPrices.INT), show(ae), show(intl))
	}
}

func i32(v int32) *int32 { return &v }

func (e *premiumEnv) createAdminTemplate(t *testing.T, body string) (*httptest.ResponseRecorder, uuid.UUID) {
	t.Helper()
	rec := e.a.do(http.MethodPost, "/v1/admin/templates", e.superTok, body)
	var resp struct {
		ID uuid.UUID `json:"id"`
	}
	if rec.Code == http.StatusCreated {
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		id := resp.ID
		t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), "DELETE FROM templates WHERE id = $1", id) })
	}
	return rec, resp.ID
}

func ptestSlug() string { return "ptest-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16] }

func wantPriceIssue(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	wantIssues(t, rec, nil, "price_paise")
}

func TestPremium_AdminTemplateCreatePrice(t *testing.T) {
	e := newPremiumEnv(t)

	t.Run("default price, no regional prices", func(t *testing.T) {
		rec, id := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Default price","is_premium":true}`, ptestSlug()))
		wantStatusCode(t, rec, http.StatusCreated, "")
		v := decodeTplPrices(t, rec)
		if v.PricePaise != 59900 || !v.IsPremium {
			t.Fatalf("resp = %+v, want 59900 premium", v)
		}
		wantRegional(t, v, nil, nil) // a new premium template is India-only until regional prices are given
		if !strings.Contains(rec.Body.String(), `"regional_prices":{"AE":null,"INT":null}`) {
			t.Errorf("body = %s, want explicit null regions", rec.Body.String())
		}
		var n int
		if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM template_prices WHERE template_id = $1", id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("template_prices rows = %d, %v; want 0", n, err)
		}
	})

	t.Run("with regional prices", func(t *testing.T) {
		rec, id := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Regional","is_premium":true,"price_paise":70000,"regional_prices":{"AE":3500,"INT":1100}}`, ptestSlug()))
		wantStatusCode(t, rec, http.StatusCreated, "")
		wantRegional(t, decodeTplPrices(t, rec), i32(3500), i32(1100))

		var ae, intl int32
		if err := e.pool.QueryRow(context.Background(),
			"SELECT (SELECT amount_minor FROM template_prices WHERE template_id = $1 AND region = 'AE'), (SELECT amount_minor FROM template_prices WHERE template_id = $1 AND region = 'INT')", id).Scan(&ae, &intl); err != nil {
			t.Fatalf("query prices: %v", err)
		}
		if ae != 3500 || intl != 1100 {
			t.Errorf("stored AE=%d INT=%d", ae, intl)
		}
		// Audit after carries the regional prices.
		rows := e.a.auditRows(t, "template", id)
		if len(rows) != 1 || rows[0].Action != "template.create" {
			t.Fatalf("audit rows = %+v", rows)
		}
		var after struct {
			Regional map[string]*int32 `json:"regional_prices"`
		}
		if err := json.Unmarshal(rows[0].After, &after); err != nil || after.Regional["AE"] == nil || *after.Regional["AE"] != 3500 ||
			after.Regional["INT"] == nil || *after.Regional["INT"] != 1100 {
			t.Errorf("create audit after = %s (%v)", rows[0].After, err)
		}
		// The list endpoint reports them too.
		listRec := e.a.do(http.MethodGet, "/v1/admin/templates", e.superTok, "")
		wantStatusCode(t, listRec, http.StatusOK, "")
		var list struct {
			Templates []tplPricesView `json:"templates"`
		}
		if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		found := false
		for _, tv := range list.Templates {
			if tv.ID == id {
				found = true
				wantRegional(t, tv, i32(3500), i32(1100))
			}
			if tv.ID == e.tplA.ID {
				wantRegional(t, tv, i32(e.tplA.AE), i32(e.tplA.INT))
			}
			if tv.ID == e.tplFree.ID {
				wantRegional(t, tv, nil, nil)
			}
		}
		if !found {
			t.Fatalf("created template missing from list")
		}
	})

	t.Run("only one region and null", func(t *testing.T) {
		rec, _ := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"One","is_premium":true,"regional_prices":{"INT":900,"AE":null}}`, ptestSlug()))
		wantStatusCode(t, rec, http.StatusCreated, "")
		wantRegional(t, decodeTplPrices(t, rec), nil, i32(900))
		rec, _ = e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Null obj","is_premium":true,"regional_prices":null}`, ptestSlug()))
		wantStatusCode(t, rec, http.StatusCreated, "")
		wantRegional(t, decodeTplPrices(t, rec), nil, nil)
	})

	for _, p := range []string{"100", "1000000", "59900"} {
		t.Run("accepts "+p, func(t *testing.T) {
			rec, _ := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Bound","is_premium":true,"price_paise":%s,"regional_prices":{"AE":%s,"INT":%s}}`, ptestSlug(), p, p, p))
			wantStatusCode(t, rec, http.StatusCreated, "")
			if !strings.Contains(rec.Body.String(), `"price_paise":`+p) {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}

	// Out of range, wrong type, overflow: all a field-level validation issue.
	for _, p := range []string{"99", "1000001", "0", "-1", "2147483648", "9999999999", "99999999999999999999999",
		"599.5", `"599"`, "true", "false", "[]", "{}", "1e3"} {
		t.Run("rejects price_paise "+p, func(t *testing.T) {
			rec, _ := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Bad","price_paise":%s}`, ptestSlug(), p))
			wantPriceIssue(t, rec)
		})
		for _, region := range []string{"AE", "INT"} {
			t.Run("rejects regional "+region+" "+p, func(t *testing.T) {
				rec, _ := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Bad","is_premium":true,"regional_prices":{%q:%s}}`, ptestSlug(), region, p))
				wantIssues(t, rec, nil, "regional_prices."+region)
			})
		}
	}

	t.Run("all bad fields reported together", func(t *testing.T) {
		rec, _ := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Bad","is_premium":true,"price_paise":"x","regional_prices":{"AE":5,"INT":true}}`, ptestSlug()))
		wantIssues(t, rec, map[string]string{"price_paise": "invalid", "regional_prices.AE": "out_of_range", "regional_prices.INT": "invalid"},
			"price_paise", "regional_prices.AE", "regional_prices.INT")
	})

	t.Run("regional price on a free template", func(t *testing.T) {
		rec, _ := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Free","regional_prices":{"AE":3000}}`, ptestSlug()))
		wantIssues(t, rec, map[string]string{"regional_prices.AE": "requires_premium"}, "regional_prices.AE")
		rec, _ = e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Free","is_premium":false,"regional_prices":{"AE":3000,"INT":900}}`, ptestSlug()))
		wantIssues(t, rec, map[string]string{"regional_prices.AE": "requires_premium", "regional_prices.INT": "requires_premium"}, "regional_prices.AE", "regional_prices.INT")
		// Null regions on a free template are harmless.
		rec, _ = e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Free","regional_prices":{"AE":null}}`, ptestSlug()))
		wantStatusCode(t, rec, http.StatusCreated, "")
	})

	t.Run("invalid json shapes", func(t *testing.T) {
		for _, c := range []struct{ name, extra string }{
			{"unknown top-level field", `,"amount":5`},
			{"unknown region key", `,"regional_prices":{"EU":500}`},
			{"regional_prices not an object", `,"regional_prices":5`},
			{"regional_prices array", `,"regional_prices":[]`},
		} {
			t.Run(c.name, func(t *testing.T) {
				rec, _ := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"X","is_premium":true%s}`, ptestSlug(), c.extra))
				wantStatusCode(t, rec, http.StatusBadRequest, "invalid_json")
			})
		}
	})

	t.Run("auth", func(t *testing.T) {
		body := fmt.Sprintf(`{"slug":%q,"name":"X","is_premium":true}`, ptestSlug())
		wantStatusCode(t, e.a.do(http.MethodPost, "/v1/admin/templates", e.a.moderatorT, body), http.StatusForbidden, "")
		wantStatusCode(t, e.a.do(http.MethodPost, "/v1/admin/templates", "", body), http.StatusUnauthorized, "")
	})
}

func TestPremium_AdminTemplatePatchPrice(t *testing.T) {
	e := newPremiumEnv(t)
	rec, id := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Patch me","is_premium":true,"price_paise":50000}`, ptestSlug()))
	wantStatusCode(t, rec, http.StatusCreated, "")
	path := "/v1/admin/templates/" + id.String()

	// create audit carries the price
	creates := e.a.auditRows(t, "template", id)
	if len(creates) != 1 || creates[0].Action != "template.create" || !strings.Contains(string(creates[0].After), `"price_paise": 50000`) && !strings.Contains(string(creates[0].After), `"price_paise":50000`) {
		t.Fatalf("create audit = %+v", creates)
	}

	dbPrice := func() int32 {
		var cur int32
		if err := e.pool.QueryRow(context.Background(), "SELECT price_paise FROM templates WHERE id = $1", id).Scan(&cur); err != nil {
			t.Fatalf("query price: %v", err)
		}
		return cur
	}

	for _, p := range []string{"99", "1000001", "0", "-5", "1.5", `"7"`, "true", "99999999999999999999"} {
		t.Run("rejects "+p, func(t *testing.T) {
			wantPriceIssue(t, e.a.do(http.MethodPatch, path, e.superTok, `{"price_paise":`+p+`}`))
		})
	}
	if cur := dbPrice(); cur != 50000 {
		t.Fatalf("price after rejected patches = %d; want 50000", cur)
	}

	rec = e.a.do(http.MethodPatch, path, e.superTok, `{"price_paise":75000}`)
	wantStatusCode(t, rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), `"price_paise":75000`) {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// Omitting price, or sending null, leaves it untouched.
	wantStatusCode(t, e.a.do(http.MethodPatch, path, e.superTok, `{"name":"Renamed"}`), http.StatusOK, "")
	if cur := dbPrice(); cur != 75000 {
		t.Fatalf("price after rename = %d; want 75000", cur)
	}
	wantStatusCode(t, e.a.do(http.MethodPatch, path, e.superTok, `{"price_paise":null}`), http.StatusOK, "")
	if cur := dbPrice(); cur != 75000 {
		t.Fatalf("price after null = %d; want 75000", cur)
	}

	// Boundaries.
	wantStatusCode(t, e.a.do(http.MethodPatch, path, e.superTok, `{"price_paise":100}`), http.StatusOK, "")
	wantStatusCode(t, e.a.do(http.MethodPatch, path, e.superTok, `{"price_paise":1000000}`), http.StatusOK, "")

	// Audit: the first successful patch recorded old and new price.
	rows := e.a.auditRows(t, "template", id)
	var first *store.AuditLog
	for i := range rows {
		if rows[i].Action == "template.update" {
			first = &rows[i] // keep overwriting: ends on the oldest update (newest first)
		}
	}
	if first == nil {
		t.Fatalf("no template.update audit row in %+v", rows)
	}
	var before, after map[string]any
	if err := json.Unmarshal(first.Before, &before); err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := json.Unmarshal(first.After, &after); err != nil {
		t.Fatalf("after: %v", err)
	}
	if before["price_paise"] != float64(50000) || after["price_paise"] != float64(75000) {
		t.Errorf("audit price before=%v after=%v, want 50000 -> 75000", before["price_paise"], after["price_paise"])
	}

	// Auth and not-found.
	wantStatusCode(t, e.a.do(http.MethodPatch, path, e.a.moderatorT, `{"price_paise":1000}`), http.StatusForbidden, "")
	wantStatusCode(t, e.a.do(http.MethodPatch, "/v1/admin/templates/"+uuid.NewString(), e.superTok, `{"price_paise":1000}`), http.StatusNotFound, "not_found")
	wantStatusCode(t, e.a.do(http.MethodPatch, "/v1/admin/templates/"+uuid.NewString(), e.superTok, `{"regional_prices":{"AE":1000}}`), http.StatusNotFound, "not_found")
}

func TestPremium_AdminTemplatePatchRegionalPrices(t *testing.T) {
	e := newPremiumEnv(t)
	rec, id := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Regional patch","is_premium":true,"price_paise":50000}`, ptestSlug()))
	wantStatusCode(t, rec, http.StatusCreated, "")
	path := "/v1/admin/templates/" + id.String()

	patch := func(body string) *httptest.ResponseRecorder { return e.a.do(http.MethodPatch, path, e.superTok, body) }
	type auditPrices struct {
		Regional map[string]*int32 `json:"regional_prices"`
	}
	latestAudit := func() (before, after auditPrices) {
		t.Helper()
		rows := e.a.auditRows(t, "template", id)
		if len(rows) == 0 || rows[0].Action != "template.update" {
			t.Fatalf("latest audit = %+v", rows)
		}
		if err := json.Unmarshal(rows[0].Before, &before); err != nil {
			t.Fatalf("before: %v", err)
		}
		if err := json.Unmarshal(rows[0].After, &after); err != nil {
			t.Fatalf("after: %v", err)
		}
		return before, after
	}
	val := func(m map[string]*int32, k string) string {
		v, ok := m[k]
		if !ok {
			return "<absent>"
		}
		if v == nil {
			return "null"
		}
		return fmt.Sprint(*v)
	}

	// Set AE only; INT stays unoffered.
	r := patch(`{"regional_prices":{"AE":2900}}`)
	wantStatusCode(t, r, http.StatusOK, "")
	wantRegional(t, decodeTplPrices(t, r), i32(2900), nil)
	b, a := latestAudit()
	if val(b.Regional, "AE") != "null" || val(b.Regional, "INT") != "null" || val(a.Regional, "AE") != "2900" || val(a.Regional, "INT") != "null" {
		t.Errorf("audit regional before=%v after=%v", b.Regional, a.Regional)
	}

	// Omitted key unchanged: set INT, AE stays.
	r = patch(`{"regional_prices":{"INT":900}}`)
	wantStatusCode(t, r, http.StatusOK, "")
	wantRegional(t, decodeTplPrices(t, r), i32(2900), i32(900))
	b, a = latestAudit()
	if val(b.Regional, "AE") != "2900" || val(b.Regional, "INT") != "null" || val(a.Regional, "AE") != "2900" || val(a.Regional, "INT") != "900" {
		t.Errorf("audit regional before=%v after=%v", b.Regional, a.Regional)
	}

	// No regional_prices / empty object / unrelated edit: unchanged.
	for _, body := range []string{`{"name":"Renamed"}`, `{"regional_prices":{}}`, `{"regional_prices":null}`} {
		r = patch(body)
		wantStatusCode(t, r, http.StatusOK, "")
		wantRegional(t, decodeTplPrices(t, r), i32(2900), i32(900))
	}

	// Update an existing region in place (upsert).
	r = patch(`{"regional_prices":{"AE":3100}}`)
	wantStatusCode(t, r, http.StatusOK, "")
	wantRegional(t, decodeTplPrices(t, r), i32(3100), i32(900))

	// null deletes a region; the other stays.
	r = patch(`{"regional_prices":{"AE":null}}`)
	wantStatusCode(t, r, http.StatusOK, "")
	wantRegional(t, decodeTplPrices(t, r), nil, i32(900))
	b, a = latestAudit()
	if val(b.Regional, "AE") != "3100" || val(a.Regional, "AE") != "null" || val(a.Regional, "INT") != "900" {
		t.Errorf("audit regional before=%v after=%v", b.Regional, a.Regional)
	}
	// Deleting a region that is not offered is a no-op, not an error.
	r = patch(`{"regional_prices":{"AE":null}}`)
	wantStatusCode(t, r, http.StatusOK, "")
	wantRegional(t, decodeTplPrices(t, r), nil, i32(900))
	// Both at once, with a price change alongside.
	r = patch(`{"price_paise":60000,"regional_prices":{"AE":4000,"INT":null}}`)
	wantStatusCode(t, r, http.StatusOK, "")
	v := decodeTplPrices(t, r)
	wantRegional(t, v, i32(4000), nil)
	if v.PricePaise != 60000 {
		t.Errorf("price_paise = %d, want 60000", v.PricePaise)
	}
	// GET detail agrees.
	g := e.a.do(http.MethodGet, path, e.superTok, "")
	wantStatusCode(t, g, http.StatusOK, "")
	wantRegional(t, decodeTplPrices(t, g), i32(4000), nil)

	snapshot := func() string {
		var s string
		if err := e.pool.QueryRow(context.Background(),
			`SELECT t.name || '|' || t.price_paise || '|' || coalesce((SELECT string_agg(region || amount_minor, ',' ORDER BY region) FROM template_prices WHERE template_id = t.id), '')
			 FROM templates t WHERE t.id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		return s
	}
	baseline := snapshot()

	t.Run("field level errors", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			body  string
			paths []string
		}{
			{"AE out of range", `{"regional_prices":{"AE":99}}`, []string{"regional_prices.AE"}},
			{"INT too high", `{"regional_prices":{"INT":1000001}}`, []string{"regional_prices.INT"}},
			{"AE string", `{"regional_prices":{"AE":"29"}}`, []string{"regional_prices.AE"}},
			{"INT bool", `{"regional_prices":{"INT":false}}`, []string{"regional_prices.INT"}},
			{"AE fraction", `{"regional_prices":{"AE":29.5}}`, []string{"regional_prices.AE"}},
			{"AE overflow", `{"regional_prices":{"AE":99999999999999999999}}`, []string{"regional_prices.AE"}},
			{"AE int32 overflow", `{"regional_prices":{"AE":2147483648}}`, []string{"regional_prices.AE"}},
			{"AE zero", `{"regional_prices":{"AE":0}}`, []string{"regional_prices.AE"}},
			{"AE negative", `{"regional_prices":{"AE":-100}}`, []string{"regional_prices.AE"}},
			{"both regions", `{"regional_prices":{"AE":1,"INT":"x"}}`, []string{"regional_prices.AE", "regional_prices.INT"}},
			{"all three fields", `{"price_paise":true,"regional_prices":{"AE":1,"INT":2000000}}`, []string{"price_paise", "regional_prices.AE", "regional_prices.INT"}},
			{"bad price with good region", `{"price_paise":5,"regional_prices":{"AE":2900}}`, []string{"price_paise"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				wantIssues(t, patch(c.body), nil, c.paths...)
				if got := snapshot(); got != baseline {
					t.Fatalf("template changed by rejected patch: %q -> %q", baseline, got)
				}
			})
		}
		// The rejected requests wrote no audit rows.
		if rows := e.a.auditRows(t, "template", id); rows[0].Action != "template.update" {
			t.Fatalf("unexpected audit head %+v", rows[0])
		}
	})

	t.Run("invalid json shapes", func(t *testing.T) {
		for _, body := range []string{
			`{"amount":5}`, `{"regional_prices":{"EU":500}}`, `{"regional_prices":5}`, `{"regional_prices":[]}`, `{"regional_prices":"x"}`,
		} {
			t.Run(body, func(t *testing.T) {
				wantStatusCode(t, patch(body), http.StatusBadRequest, "invalid_json")
			})
		}
		if got := snapshot(); got != baseline {
			t.Fatalf("template changed by rejected patches: %q -> %q", baseline, got)
		}
	})

	t.Run("requires premium", func(t *testing.T) {
		frec, fid := e.createAdminTemplate(t, fmt.Sprintf(`{"slug":%q,"name":"Free one"}`, ptestSlug()))
		wantStatusCode(t, frec, http.StatusCreated, "")
		fpath := "/v1/admin/templates/" + fid.String()
		freeSnapshot := func() string {
			var s string
			if err := e.pool.QueryRow(context.Background(),
				`SELECT t.name || '|' || t.is_premium || '|' || (SELECT count(*) FROM template_prices WHERE template_id = t.id) FROM templates t WHERE t.id = $1`, fid).Scan(&s); err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			return s
		}
		base := freeSnapshot()

		// Not premium (known from the request).
		wantIssues(t, e.a.do(http.MethodPatch, fpath, e.superTok, `{"is_premium":false,"regional_prices":{"AE":2900}}`),
			map[string]string{"regional_prices.AE": "requires_premium"}, "regional_prices.AE")
		// Not premium (known only in the transaction): the other fields of the request must roll back with it.
		wantIssues(t, e.a.do(http.MethodPatch, fpath, e.superTok, `{"name":"Should not stick","regional_prices":{"AE":2900,"INT":900}}`),
			map[string]string{"regional_prices.AE": "requires_premium"}, "regional_prices.AE")
		if got := freeSnapshot(); got != base {
			t.Fatalf("rejected patch left changes: %q -> %q", base, got)
		}
		if rows := e.a.auditRows(t, "template", fid); len(rows) != 1 {
			t.Fatalf("audit rows = %d after rejected patches, want only the create row", len(rows))
		}
		// Nulls on a free template are fine.
		wantStatusCode(t, e.a.do(http.MethodPatch, fpath, e.superTok, `{"regional_prices":{"AE":null,"INT":null}}`), http.StatusOK, "")
		// Making it premium in the same request unlocks regional prices.
		r := e.a.do(http.MethodPatch, fpath, e.superTok, `{"is_premium":true,"regional_prices":{"AE":2900}}`)
		wantStatusCode(t, r, http.StatusOK, "")
		v := decodeTplPrices(t, r)
		wantRegional(t, v, i32(2900), nil)
		if !v.IsPremium {
			t.Errorf("template not premium after patch")
		}
	})

	t.Run("auth", func(t *testing.T) {
		wantStatusCode(t, e.a.do(http.MethodPatch, path, e.a.moderatorT, `{"regional_prices":{"AE":2900}}`), http.StatusForbidden, "")
		wantStatusCode(t, e.a.do(http.MethodPatch, path, "", `{"regional_prices":{"AE":2900}}`), http.StatusUnauthorized, "")
		if got := snapshot(); got != baseline {
			t.Fatalf("template changed by unauthorised patches")
		}
	})
}

func TestPremium_DBConstraints(t *testing.T) {
	e := newPremiumEnv(t)
	ctx := context.Background()

	t.Run("templates price range", func(t *testing.T) {
		for _, p := range []int{99, 1000001} {
			if _, err := e.pool.Exec(ctx, "UPDATE templates SET price_paise = $2 WHERE id = $1", e.tplA.ID, p); err == nil {
				t.Errorf("price_paise %d accepted by DB, want CHECK violation", p)
			}
		}
	})

	t.Run("template_prices", func(t *testing.T) {
		ins := func(region, currency string, amount int) error {
			_, err := e.pool.Exec(ctx, `INSERT INTO template_prices (template_id, region, currency, amount_minor) VALUES ($1, $2, $3, $4)`,
				e.tplB.ID, region, currency, amount)
			return err
		}
		for _, c := range []struct {
			name     string
			region   string
			currency string
			amount   int
		}{
			{"IN region not allowed", "IN", "INR", 1000},
			{"unknown region", "XX", "USD", 1000},
			{"AE with USD", "INT", "AED", 1000},
			{"INT with AED", "AE", "USD", 1000},
			{"amount too low", "INT", "USD", 99},
			{"amount too high", "INT", "USD", 1000001},
		} {
			if err := ins(c.region, c.currency, c.amount); err == nil {
				t.Errorf("%s: insert accepted, want CHECK violation", c.name)
			}
		}
		if err := ins("INT", "USD", 100); err != nil {
			t.Fatalf("valid boundary insert: %v", err)
		}
		err := ins("INT", "USD", 500)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Errorf("duplicate (template, region) err = %v, want unique violation", err)
		}
	})

	t.Run("template_prices cascade with template", func(t *testing.T) {
		tpl := e.newTemplate(t, "prem-cascade", true, 20000, 8, 2000, 700)
		// Draft-only templates can be deleted; this one has a published version, so check the FK action instead.
		var action string
		if err := e.pool.QueryRow(ctx, `SELECT confdeltype::text FROM pg_constraint WHERE conrelid = 'template_prices'::regclass AND contype = 'f'`).Scan(&action); err != nil || action != "c" {
			t.Fatalf("template_prices FK delete action = %q, %v; want c (cascade) for %s", action, err, tpl.Slug)
		}
	})

	t.Run("event_payments", func(t *testing.T) {
		ev := e.createEvent(t, e.tplA)
		ins := func(eventID uuid.UUID, region, currency string, amount int, revoked bool) error {
			q := `INSERT INTO event_payments (id, event_id, template_id, region, currency, amount_minor) VALUES ($1, $2, $3, $4, $5, $6)`
			if revoked {
				q = `INSERT INTO event_payments (id, event_id, template_id, region, currency, amount_minor, revoked_at) VALUES ($1, $2, $3, $4, $5, $6, now())`
			}
			_, err := e.pool.Exec(ctx, q, uuid.Must(uuid.NewV7()), eventID, e.tplA.ID, region, currency, amount)
			return err
		}
		for _, c := range []struct {
			name     string
			region   string
			currency string
			amount   int
		}{
			{"IN with USD", "IN", "USD", 1000},
			{"AE with INR", "AE", "INR", 1000},
			{"INT with AED", "INT", "AED", 1000},
			{"unknown region", "XX", "USD", 1000},
			{"amount too low", "IN", "INR", 99},
			{"amount too high", "IN", "INR", 1000001},
		} {
			if err := ins(ev.ID, c.region, c.currency, c.amount, false); err == nil {
				t.Errorf("%s: insert accepted, want CHECK violation", c.name)
			}
		}
		if err := e.pool.QueryRow(ctx, "SELECT 1").Scan(new(int)); err != nil {
			t.Fatal(err)
		}

		// A revoked row never blocks anything: several can coexist with one live row.
		if err := ins(ev.ID, "AE", "AED", 2900, true); err != nil {
			t.Fatalf("revoked insert: %v", err)
		}
		if err := ins(ev.ID, "IN", "INR", 59900, true); err != nil {
			t.Fatalf("second revoked insert: %v", err)
		}
		if err := ins(ev.ID, "INT", "USD", 900, false); err != nil {
			t.Fatalf("live insert next to revoked rows: %v", err)
		}
		err := ins(ev.ID, "AE", "AED", 2900, false)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Errorf("second live grant err = %v, want unique violation", err)
		}
		// revoked_by requires revoked_at.
		_, err = e.pool.Exec(ctx, `UPDATE event_payments SET revoked_by = $2 WHERE event_id = $1 AND revoked_at IS NULL`, ev.ID, e.hostID)
		if err == nil {
			t.Errorf("revoked_by without revoked_at accepted, want CHECK violation")
		}
	})

	t.Run("template with payments is not deletable", func(t *testing.T) {
		ev := e.createEvent(t, e.tplA)
		e.mustGrant(t, ev.ID, e.tplA)
		if _, err := e.pool.Exec(ctx, "DELETE FROM templates WHERE id = $1", e.tplA.ID); err == nil {
			t.Errorf("template with payments was deletable, want RESTRICT")
		}
		// Revoked payments still protect the template.
		wantStatusCode(t, e.revoke(ev.ID, e.tplA.ID), http.StatusNoContent, "")
		if _, err := e.pool.Exec(ctx, "DELETE FROM templates WHERE id = $1", e.tplA.ID); err == nil {
			t.Errorf("template with only revoked payments was deletable, want RESTRICT")
		}
	})
}

// --- moderator restore ---

func TestPremium_RestoreEvent(t *testing.T) {
	e := newPremiumEnv(t)

	t.Run("free published goes back to published", func(t *testing.T) {
		ev := e.createEvent(t, e.tplFree)
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
		e.takedown(t, ev.ID)
		e.restore(t, ev.ID)
		if got := e.dbStatus(t, ev.ID); got != "published" {
			t.Fatalf("status = %q, want published", got)
		}
	})

	t.Run("never published goes back to draft", func(t *testing.T) {
		ev := e.createEvent(t, e.tplFree)
		e.takedown(t, ev.ID)
		e.restore(t, ev.ID)
		if got := e.dbStatus(t, ev.ID); got != "draft" {
			t.Fatalf("status = %q, want draft", got)
		}
		// A never-published premium draft likewise returns to draft, paid or not.
		prem := e.createEvent(t, e.tplA)
		e.takedown(t, prem.ID)
		e.restore(t, prem.ID)
		if got := e.dbStatus(t, prem.ID); got != "draft" {
			t.Fatalf("premium never-published status = %q, want draft", got)
		}
	})

	t.Run("switched to unpaid premium while hidden restores to hidden", func(t *testing.T) {
		ev := e.createEvent(t, e.tplFree)
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
		wantStatusCode(t, e.unpublish(t, ev.ID), http.StatusOK, "")
		wantStatusCode(t, e.switchTemplate(t, ev.ID, e.tplA), http.StatusOK, "") // allowed while hidden
		e.takedown(t, ev.ID)
		e.restore(t, ev.ID)
		if got := e.dbStatus(t, ev.ID); got != "hidden" {
			t.Fatalf("status = %q, want hidden (restore must not bypass the payment gate)", got)
		}
		// The host still has to pay before the event goes public.
		wantPaymentRequired(t, e.publish(t, ev.ID), e.tplA)
		e.mustGrant(t, ev.ID, e.tplA)
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
	})

	t.Run("paid premium restores to published", func(t *testing.T) {
		ev := e.createEvent(t, e.tplA)
		e.mustGrant(t, ev.ID, e.tplA)
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
		e.takedown(t, ev.ID)
		e.restore(t, ev.ID)
		if got := e.dbStatus(t, ev.ID); got != "published" {
			t.Fatalf("status = %q, want published", got)
		}
	})

	t.Run("payment revoked while taken down restores to hidden", func(t *testing.T) {
		ev := e.createEvent(t, e.tplA)
		e.mustGrant(t, ev.ID, e.tplA)
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
		e.takedown(t, ev.ID)
		wantStatusCode(t, e.revoke(ev.ID, e.tplA.ID), http.StatusNoContent, "")
		e.restore(t, ev.ID)
		if got := e.dbStatus(t, ev.ID); got != "hidden" {
			t.Fatalf("status = %q, want hidden", got)
		}
		// Re-granting lets the host publish again.
		wantStatusCode(t, e.grant(ev.ID, grantJSON(e.tplA, "AE")), http.StatusCreated, "")
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
	})

	t.Run("audit records the resulting status", func(t *testing.T) {
		ev := e.createEvent(t, e.tplFree)
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
		e.takedown(t, ev.ID)
		e.restore(t, ev.ID)
		rows := e.a.auditRows(t, "event", ev.ID)
		if len(rows) < 1 || rows[0].Action != "event.restore" || !strings.Contains(string(rows[0].After), "published") || !strings.Contains(string(rows[0].Before), "taken_down") {
			t.Fatalf("restore audit = %+v", rows)
		}
	})

	t.Run("restoring an event that is not taken down is 404", func(t *testing.T) {
		ev := e.createEvent(t, e.tplFree)
		rec := e.a.do(http.MethodPost, "/v1/admin/events/"+ev.ID.String()+"/restore", e.a.moderatorT, `{}`)
		wantStatusCode(t, rec, http.StatusNotFound, "not_found")
	})
}

// An admin may pin a premium template on a published event without payment;
// the flags audit row records whether that left the event unpaid.
func TestPremium_AdminFlagsAuditPremiumUnpaid(t *testing.T) {
	e := newPremiumEnv(t)
	flags := func(id uuid.UUID, tpl premiumTpl) map[string]any {
		t.Helper()
		rec := e.a.do(http.MethodPatch, "/v1/admin/events/"+id.String()+"/flags", e.superTok, fmt.Sprintf(`{"template_slug":%q}`, tpl.Slug))
		wantStatusCode(t, rec, http.StatusOK, "")
		rows := e.a.auditRows(t, "event", id)
		if len(rows) == 0 || rows[0].Action != "event.flags" {
			t.Fatalf("audit head = %+v", rows)
		}
		var after map[string]any
		if err := json.Unmarshal(rows[0].After, &after); err != nil {
			t.Fatalf("decode audit after: %v", err)
		}
		return after
	}

	t.Run("published and unpaid", func(t *testing.T) {
		ev := e.createEvent(t, e.tplFree)
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
		after := flags(ev.ID, e.tplA)
		if after["premium_unpaid"] != true {
			t.Fatalf("audit after = %v, want premium_unpaid=true", after)
		}
		if got := e.getEvent(t, ev.ID); got.Template.Slug != e.tplA.Slug || got.Status != "published" {
			t.Fatalf("event after flags = %+v", got)
		}
	})
	t.Run("published and paid", func(t *testing.T) {
		ev := e.createEvent(t, e.tplA)
		e.mustGrant(t, ev.ID, e.tplA)
		wantStatusCode(t, e.publish(t, ev.ID), http.StatusOK, "")
		if after := flags(ev.ID, e.tplA); after["premium_unpaid"] != false {
			t.Fatalf("audit after = %v, want premium_unpaid=false", after)
		}
	})
	t.Run("draft and free target carry no premium_unpaid", func(t *testing.T) {
		draft := e.createEvent(t, e.tplFree)
		if after := flags(draft.ID, e.tplA); after["premium_unpaid"] != nil {
			t.Fatalf("draft audit after = %v, want no premium_unpaid", after)
		}
		pub := e.createEvent(t, e.tplFree)
		wantStatusCode(t, e.publish(t, pub.ID), http.StatusOK, "")
		if after := flags(pub.ID, e.tplFree); after["premium_unpaid"] != nil {
			t.Fatalf("free-target audit after = %v, want no premium_unpaid", after)
		}
	})
}

// The India amount must not be accepted for another region's price.
func TestPremium_AdminGrantWrongRegionAmount(t *testing.T) {
	e := newPremiumEnv(t)
	ev := e.createEvent(t, e.tplA)
	for _, region := range []string{"AE", "INT"} {
		body := fmt.Sprintf(`{"template_id":%q,"region":%q,"amount_minor":%d}`, e.tplA.ID, region, e.tplA.Price)
		wantStatusCode(t, e.grant(ev.ID, body), http.StatusConflict, "price_changed")
	}
	// ...nor an AE amount for India.
	body := fmt.Sprintf(`{"template_id":%q,"region":"IN","amount_minor":%d}`, e.tplA.ID, e.tplA.AE)
	wantStatusCode(t, e.grant(ev.ID, body), http.StatusConflict, "price_changed")
	if n := e.paymentCount(t, ev.ID); n != 0 {
		t.Fatalf("payments = %d, want 0", n)
	}
}
