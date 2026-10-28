package zapsmtp

// Regressions found by the Codex review of the Go SDK.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"testing"
	"testing/iotest"
	"time"
)

func TestCodexSdkgoExplicitEmptyTemplateCollections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  any
		field string
		want  string
	}{
		{"clear sample variables", &TemplatesUpdateParams{Variables: map[string]any{}}, "variables", "{}"},
		{"clear declared schema", &TemplatesUpdateParams{VariableSchema: []VariableSpec{}}, "variable_schema", "[]"},
		{"render without stored samples", &TemplatesRenderParams{Variables: map[string]any{}}, "variables", "{}"},
		{"validate explicit empty webhook events", &WebhooksUpdateParams{Events: []WebhookEventType{}}, "events", "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _, err := (&request{body: tc.body}).encode()
			mustNil(t, err)
			var fields map[string]json.RawMessage
			mustNil(t, json.Unmarshal(body, &fields))
			if string(fields[tc.field]) != tc.want {
				t.Fatalf("explicit empty %s must reach the API as %s; request was %s", tc.field, tc.want, body)
			}
		})
	}
}

func TestCodexSdkgoResponseMetadataSurvivesBodyFailure(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"X-Request-Id": {"req_body_failed"}, "Retry-After": {"2"}},
			Body:       io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)),
		}, nil
	})}
	c := NewClient("zap_test_key", WithHTTPClient(hc), WithMaxRetries(0))
	var response Response
	_, err := c.Emails.Get(ctx, "email-1", WithResponse(&response))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected body read failure, got %v", err)
	}
	if response.StatusCode != 503 || response.RequestID != "req_body_failed" || response.Header.Get("Retry-After") != "2" {
		t.Fatalf("WithResponse lost the HTTP answer when its body failed: %+v", response)
	}
}

func TestCodexSdkgoWebhookDoesNotSilentlyDiscardBatch(t *testing.T) {
	// API.md permits signed arrays when a webhook has batch:true. The current
	// single-event return type cannot represent that array. At minimum it must
	// report an error so the documented handler does not acknowledge lost events.
	body := `[{"id":"evt_1","type":"email.delivered","data":{"email_id":"e1"}},{"id":"evt_2","type":"email.bounced","data":{"email_id":"e2"}}]`
	secret := "whsec_batch"
	header := "t=1700000000,v1=" + sign(secret, "1700000000", body)
	event, err := VerifyWebhook([]byte(body), header, secret, WithCurrentTime(time.Unix(1700000000, 0)))
	if event != nil || !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("signed batch became a successful empty single event: event=%+v err=%v", event, err)
	}
}

func TestEmptyIdempotencyKeyRefusedBeforeSending(t *testing.T) {
	f := newFake(t, answer(200, `{"id":"e1"}`))
	c := f.client()
	p := &SendEmailParams{To: []string{"b"}}
	for _, opt := range []RequestOption{
		WithIdempotencyKey(""), WithIdempotencyKey("  \t"),
		WithHeader("Idempotency-Key", ""), WithHeader("idempotency-key", " "), WithHeader("IDEMPOTENCY-KEY", ""),
	} {
		_, err := c.Emails.Send(ctx, p, opt)
		var e *Error
		if !errors.As(err, &e) || !IsValidation(err) || e.Code != "invalid_parameter" || e.Message != "idempotency key must not be empty" || e.StatusCode != 0 {
			t.Fatalf("%v", err)
		}
	}
	// also on an operation that takes no key: a caller's empty header is refused all the same
	_, err := c.Domains.Create(ctx, &DomainsCreateParams{Name: "a.test"}, WithHeader("idempotency-key", ""))
	if !IsValidation(err) {
		t.Fatal(err)
	}
	if len(f.calls()) != 0 {
		t.Fatal("nothing is sent")
	}
	// a caller's header replaces the SDK's own whatever its case
	_, err = c.Emails.Send(ctx, p, WithHeader("idempotency-key", "mine"))
	mustNil(t, err)
	if h := f.calls()[0].header["Idempotency-Key"]; len(h) != 1 || h[0] != "mine" {
		t.Fatal(h)
	}
}

func TestAutoPagingStopsOnCancelBetweenItems(t *testing.T) {
	f := newFake(t, page(true, `{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`))
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var ids []string
	var last error
	for e, err := range f.client().Emails.ListAutoPaging(cctx, nil) {
		if err != nil {
			last = err
			break
		}
		ids = append(ids, e.ID)
		cancel()
	}
	if len(ids) != 1 || !errors.Is(last, context.Canceled) || len(f.calls()) != 1 {
		t.Fatal(ids, last, len(f.calls()))
	}
}

func TestOutOfRangeNumbersDecodeToZero(t *testing.T) {
	data := `{"score":1e999,"threshold":-1e999,"note":"n","engines":[{"engine":"x","score":1e400}]}`
	var s SpamCheck
	mustNil(t, json.Unmarshal([]byte(data), &s))
	if s.Score != 0 || s.Threshold != 0 || s.Note != "n" || len(s.Engines) != 1 || s.Engines[0].Score != 0 || s.Engines[0].Engine != "x" {
		t.Fatalf("%+v", s)
	}
	var u Usage
	mustNil(t, json.Unmarshal([]byte(`{"limit":1e999,"sent":5}`), &u))
	if u.Limit != nil || u.Sent != 5 {
		t.Fatalf("%+v", u)
	}
	var tpl Template
	mustNil(t, json.Unmarshal([]byte(`{"design":{"w":1e999},"name":"t"}`), &tpl))
	if tpl.Design != nil || tpl.Name != "t" {
		t.Fatalf("%+v", tpl)
	}
	for _, f := range []float64{s.Score, s.Threshold, s.Engines[0].Score} {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			t.Fatal(f)
		}
	}
}

func TestRetryAfterNeverOverflows(t *testing.T) {
	for _, v := range []string{"1e300", "9223372037", "99999999999999999999"} {
		d, ok := (&Error{Header: http.Header{"Retry-After": {v}}}).RetryAfter()
		if !ok || d != time.Duration(math.MaxInt64) {
			t.Errorf("%s: %v", v, d)
		}
	}
	if d, _ := (&Error{Header: http.Header{"Retry-After": {"9223372036"}}}).RetryAfter(); d != 9223372036*time.Second {
		t.Fatal(d)
	}
	if b := backoff(0, Ptr(1e300)); b != 60*time.Second {
		t.Fatal(b)
	}
}
