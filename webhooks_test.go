package zapsmtp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type vector struct {
	Name      string `json:"name"`
	Secret    string `json:"secret"`
	Payload   string `json:"payload"`
	Header    string `json:"header"`
	Now       int64  `json:"now"`
	Tolerance int64  `json:"tolerance"`
	Valid     bool   `json:"valid"`
	Batch     bool   `json:"batch"`
}

func vectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile("../spec/webhook-vectors.json")
	if os.IsNotExist(err) {
		t.Skip("no sdks/spec next to this module")
	}
	mustNil(t, err)
	var v struct{ Vectors []vector }
	mustNil(t, json.Unmarshal(raw, &v))
	if len(v.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	return v.Vectors
}

func TestWebhookVectors(t *testing.T) {
	for _, v := range vectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			opts := []VerifyOption{WithCurrentTime(time.Unix(v.Now, 0)), WithTolerance(time.Duration(v.Tolerance) * time.Second)}
			e, err := VerifyWebhook([]byte(v.Payload), v.Header, v.Secret, opts...)
			events, errs := VerifyWebhookEvents([]byte(v.Payload), v.Header, v.Secret, opts...)
			if !v.Valid {
				if e != nil || events != nil || !errors.Is(err, ErrWebhookSignature) || !errors.Is(errs, ErrWebhookSignature) || errors.Is(err, ErrWebhookBatch) {
					t.Fatalf("%v / %v", err, errs)
				}
				return
			}
			mustNil(t, errs)
			var want []map[string]any
			if v.Batch {
				if e != nil || !errors.Is(err, ErrWebhookBatch) || !errors.Is(err, ErrWebhookSignature) ||
					!strings.HasSuffix(err.Error(), "the body is a batch of events: use VerifyWebhookEvents") {
					t.Fatalf("a batch is refused by VerifyWebhook: %v", err)
				}
				mustNil(t, json.Unmarshal([]byte(v.Payload), &want))
			} else {
				mustNil(t, err)
				var one map[string]any
				mustNil(t, json.Unmarshal([]byte(v.Payload), &one))
				want = []map[string]any{one}
				checkEvent(t, *e, one)
			}
			if len(events) != len(want) {
				t.Fatalf("%d events, want %d", len(events), len(want))
			}
			for i := range want {
				checkEvent(t, events[i], want[i])
			}
		})
	}
}

func checkEvent(t *testing.T, e WebhookEvent, want map[string]any) {
	t.Helper()
	var got map[string]any
	mustNil(t, json.Unmarshal(e.Raw, &got))
	if !reflect.DeepEqual(got, want) || e.ID != want["id"] || e.Type != want["type"] || !reflect.DeepEqual(e.Data, want["data"]) {
		t.Fatalf("%+v", e)
	}
	if at, _ := time.Parse(time.RFC3339, want["created_at"].(string)); !e.CreatedAt.Equal(at) {
		t.Fatalf("created_at %v", e.CreatedAt)
	}
}

func TestWebhookClientMethodAndClock(t *testing.T) {
	v := vectors(t)[0]
	e, err := NewClient("k").Webhooks.Verify([]byte(v.Payload), v.Header, v.Secret, WithCurrentTime(time.Unix(v.Now, 0)))
	mustNil(t, err)
	if e.ID != "evt_1" {
		t.Fatal(e.ID)
	}
	defer func(f func() time.Time) { timeNow = f }(timeNow)
	timeNow = func() time.Time { return time.Unix(v.Now+10, 0) }
	_, err = VerifyWebhook([]byte(v.Payload), v.Header, v.Secret)
	mustNil(t, err)
	timeNow = func() time.Time { return time.Unix(v.Now+301, 0) }
	_, err = VerifyWebhook([]byte(v.Payload), v.Header, v.Secret)
	if !errors.Is(err, ErrWebhookSignature) || !strings.Contains(err.Error(), "too old") {
		t.Fatal(err)
	}
}

func sign(secret, t string, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(t + "." + body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestWebhookRefusals(t *testing.T) {
	now := WithCurrentTime(time.Unix(1700000000, 0))
	for _, tc := range []struct{ body, header, secret, want string }{
		{"{}", "", "s", "missing Zap-Signature header"},
		{"{}", "t=1,v1=00", "", "missing webhook secret"},
		{"{}", "t=,v1=00", "s", "malformed"},
		{"{}", "t=1x,v1=00", "s", "malformed"},
		{"{}", "t,v1", "s", "malformed"},
		{"{}", "t=99999999999999999999999999999999999999,v1=00", "s", "malformed"},
		{"{}", "t=1234567890123,v1=00", "s", "malformed"},
		{"{}", "t=999999999999,v1=00", "s", "too old"},
		{"not json", "t=1700000000,v1=" + sign("whsec_x", "1700000000", "not json"), "whsec_x", "not JSON"},
		{"{}", "t=1700000000,v1=" + sign("whsec_x", "1700000000", "{}")[:62], "whsec_x", "does not match"},
	} {
		_, err := VerifyWebhook([]byte(tc.body), tc.header, tc.secret, now)
		if !errors.Is(err, ErrWebhookSignature) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.header, err)
		}
	}
	// signed bodies that are not events
	for _, body := range []string{`"x"`, `null`, `[{"type":"a"},1]`, `[[]]`} {
		header := "t=1700000000,v1=" + sign("whsec_x", "1700000000", body)
		_, err := VerifyWebhookEvents([]byte(body), header, "whsec_x", now)
		if !errors.Is(err, ErrWebhookSignature) || !strings.Contains(err.Error(), "not an event") {
			t.Errorf("%s: %v", body, err)
		}
	}
	for _, body := range []string{`"x"`, ` 7`} {
		_, err := VerifyWebhook([]byte(body), "t=1700000000,v1="+sign("whsec_x", "1700000000", body), "whsec_x", now)
		if !errors.Is(err, ErrWebhookSignature) || !strings.Contains(err.Error(), "not an event") {
			t.Errorf("%s: %v", body, err)
		}
	}
	// leading spaces before a batch, and the client method
	batch := ` [{"type":"a"},{"type":"b"}]`
	events, err := NewClient("k").Webhooks.VerifyEvents([]byte(batch), "t=1700000000,v1="+sign("whsec_x", "1700000000", batch), "whsec_x", now)
	mustNil(t, err)
	if len(events) != 2 || events[1].Type != "b" {
		t.Fatalf("%+v", events)
	}
	// upper-case hex is the same signature
	sig := strings.ToUpper(sign("whsec_x", "1700000000", `{"type":"x"}`))
	e, err := VerifyWebhook([]byte(`{"type":"x"}`), "t=1700000000,v1="+sig, "whsec_x", now)
	mustNil(t, err)
	if e.Type != "x" {
		t.Fatal(e.Type)
	}
}
