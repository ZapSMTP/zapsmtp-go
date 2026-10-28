package zapsmtp

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Webhook signature verification (docs: Webhook events). Every request ZapSMTP
// sends carries Zap-Signature: t=<unix seconds>,v1=<hex>, where v1 is
// HMAC-SHA256 over "<t>.<raw body>" keyed with the whole secret (its whsec_
// prefix included). Two v1 entries are sent for 24 hours after a rotation.

// ErrWebhookSignature is the kind of every error of VerifyWebhook and
// VerifyWebhookEvents: check it with errors.Is.
var ErrWebhookSignature = errors.New("zapsmtp: webhook rejected")

// ErrWebhookBatch is VerifyWebhook's error for a correctly signed batch of
// events (a webhook with batch delivery). It is an ErrWebhookSignature too.
var ErrWebhookBatch = fmt.Errorf("%w: the body is a batch of events: use VerifyWebhookEvents", ErrWebhookSignature)

// DefaultWebhookTolerance is the oldest signature VerifyWebhook accepts by default.
const DefaultWebhookTolerance = 300 * time.Second

// WebhookEvent is a webhook request body.
type WebhookEvent struct {
	// ID is evt_…; it is empty on email.received, thread.created and
	// thread.replied (see the Zap-Event-Id header).
	ID string `json:"id,omitempty"`
	// Type is the event type, such as "email.delivered" (see WebhookEventType).
	Type string `json:"type"`
	// CreatedAt is when the event happened.
	CreatedAt *time.Time `json:"created_at"`
	// Data is the event's object; decode Raw into your own type for a typed view.
	Data map[string]any `json:"data"`
	// Raw is the body as received.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes leniently and keeps the raw body in Raw.
func (e *WebhookEvent) UnmarshalJSON(data []byte) error {
	type plain WebhookEvent
	decodeLenient(data, (*plain)(e))
	e.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// VerifyOption configures VerifyWebhook.
type VerifyOption func(*verifyConfig)

type verifyConfig struct {
	tolerance time.Duration
	now       time.Time
}

// WithTolerance sets the oldest signature accepted (default 300 s), and how far
// in the future one may be.
func WithTolerance(d time.Duration) VerifyOption {
	return func(c *verifyConfig) { c.tolerance = d }
}

// WithCurrentTime sets the time the signature's timestamp is checked against (default: now).
func WithCurrentTime(t time.Time) VerifyOption {
	return func(c *verifyConfig) { c.now = t }
}

// VerifyWebhook checks a webhook request's signature and returns its event.
//
// Pass the body exactly as received, before any JSON parsing, the
// Zap-Signature header and the webhook's secret (whsec_…). Every error wraps
// ErrWebhookSignature; answer it with 400. A webhook with batch delivery sends
// an array of events: VerifyWebhook refuses it with ErrWebhookBatch, and
// VerifyWebhookEvents reads both kinds.
func VerifyWebhook(payload []byte, signature string, secret string, opts ...VerifyOption) (*WebhookEvent, error) {
	body, err := verified(payload, signature, secret, opts)
	if err != nil {
		return nil, err
	}
	switch body[0] {
	case '[':
		return nil, ErrWebhookBatch
	case '{':
		return decodeEvent(body), nil
	}
	return nil, webhookError("the body is not an event")
}

// VerifyWebhookEvents checks a webhook request's signature and returns its
// events: every event of a batch (a webhook with batch delivery), or the one
// event of any other request.
func VerifyWebhookEvents(payload []byte, signature string, secret string, opts ...VerifyOption) ([]WebhookEvent, error) {
	body, err := verified(payload, signature, secret, opts)
	if err != nil {
		return nil, err
	}
	items := []json.RawMessage{body}
	if body[0] == '[' {
		_ = json.Unmarshal(body, &items) // valid JSON: an array always decodes
	}
	events := make([]WebhookEvent, 0, len(items))
	for _, item := range items {
		if item[0] != '{' {
			return nil, webhookError("the body is not an event")
		}
		events = append(events, *decodeEvent(item))
	}
	return events, nil
}

// Verify checks a webhook request's Zap-Signature and returns its event (see VerifyWebhook).
func (s *WebhooksService) Verify(payload []byte, signature string, secret string, opts ...VerifyOption) (*WebhookEvent, error) {
	return VerifyWebhook(payload, signature, secret, opts...)
}

// VerifyEvents checks a webhook request's Zap-Signature and returns its events (see VerifyWebhookEvents).
func (s *WebhooksService) VerifyEvents(payload []byte, signature string, secret string, opts ...VerifyOption) ([]WebhookEvent, error) {
	return VerifyWebhookEvents(payload, signature, secret, opts...)
}

// verified checks the signature and returns the body with its leading spaces
// trimmed: valid JSON.
func verified(payload []byte, signature string, secret string, opts []VerifyOption) ([]byte, error) {
	cfg := verifyConfig{tolerance: DefaultWebhookTolerance}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.now.IsZero() {
		cfg.now = timeNow()
	}
	if signature == "" {
		return nil, webhookError("missing Zap-Signature header")
	}
	if secret == "" {
		return nil, webhookError("missing webhook secret")
	}
	var t string
	var v1 []string
	for _, part := range strings.Split(signature, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch strings.TrimSpace(k) {
		case "t":
			t = strings.TrimSpace(v)
		case "v1":
			v1 = append(v1, strings.TrimSpace(v))
		}
	}
	// unix seconds: at most 12 digits
	if t == "" || len(t) > 12 || strings.Trim(t, "0123456789") != "" || len(v1) == 0 {
		return nil, webhookError("malformed Zap-Signature header")
	}
	ts, _ := strconv.ParseInt(t, 10, 64) // 1 to 12 digits: always an int64
	if math.Abs(float64(cfg.now.Unix()-ts)) > cfg.tolerance.Seconds() {
		return nil, webhookError("signature is too old (or from the future)")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(payload)
	want := mac.Sum(nil)
	ok := false
	for _, h := range v1 {
		got, err := hex.DecodeString(h)
		if err == nil && len(got) == sha256.Size && hmac.Equal(got, want) {
			ok = true
		}
	}
	if !ok {
		return nil, webhookError("signature does not match")
	}
	if !json.Valid(payload) {
		return nil, webhookError("the body is not JSON")
	}
	return bytes.TrimLeft(payload, " \t\r\n"), nil
}

func decodeEvent(data []byte) *WebhookEvent {
	e := new(WebhookEvent)
	decodeLenient(data, e)
	return e
}

func webhookError(msg string) error {
	return fmt.Errorf("%w: %s", ErrWebhookSignature, msg)
}
