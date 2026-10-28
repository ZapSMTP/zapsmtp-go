package zapsmtp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- a fake API ------------------------------------------------------------------

type recorded struct {
	method string
	path   string // escaped, as sent
	query  url.Values
	header http.Header
	body   []byte
}

// fakeAPI answers with its handlers in turn (the last one repeats) and records every request.
type fakeAPI struct {
	srv      *httptest.Server
	mu       sync.Mutex
	reqs     []recorded
	handlers []http.HandlerFunc
	sleeps   []time.Duration
}

func newFake(t *testing.T, handlers ...http.HandlerFunc) *fakeAPI {
	t.Helper()
	f := &fakeAPI{handlers: handlers}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.EscapedPath(), r.URL.Query(), r.Header.Clone(), body})
		h := f.handlers[0]
		if len(f.handlers) > 1 {
			f.handlers = f.handlers[1:]
		}
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) client(opts ...ClientOption) *Client {
	c := NewClient("zap_test_key", append([]ClientOption{WithBaseURL(f.srv.URL + "/v1/")}, opts...)...)
	c.c.sleep = func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		f.sleeps = append(f.sleeps, d)
		f.mu.Unlock()
		return ctx.Err()
	}
	return c
}

func (f *fakeAPI) calls() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func answer(status int, body string, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_1")
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func apiError(status int, code string, header ...string) http.HandlerFunc {
	return answer(status, fmt.Sprintf(`{"statusCode":%d,"name":%q,"message":"%s happened","error":{"code":%q,"message":"%s happened","type":"invalid_request_error"}}`,
		status, code, code, code, code), header...)
}

func page(more bool, items ...string) http.HandlerFunc {
	return answer(200, fmt.Sprintf(`{"object":"list","has_more":%v,"data":[%s]}`, more, strings.Join(items, ",")))
}

var ctx = context.Background()

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- client ----------------------------------------------------------------------

func TestEnvironment(t *testing.T) {
	f := newFake(t, answer(200, `{"id":"e1"}`))
	t.Setenv("ZAP_API_KEY", "zap_env")
	t.Setenv("ZAP_BASE_URL", f.srv.URL+"/v1")
	_, err := NewClient("").Emails.Get(ctx, "e1")
	mustNil(t, err)
	r := f.calls()[0]
	if r.path != "/v1/emails/e1" || r.header.Get("Authorization") != "Bearer zap_env" {
		t.Fatalf("%s %s", r.path, r.header.Get("Authorization"))
	}
}

func TestDefaults(t *testing.T) {
	t.Setenv("ZAP_BASE_URL", "")
	t.Setenv("ZAP_API_KEY", "")
	c := NewClient("k")
	if c.c.baseURL != DefaultBaseURL || c.c.timeout != 60*time.Second || c.c.maxRetries != 2 {
		t.Fatalf("%+v", c.c)
	}
	_, err := NewClient("").Usage.Get(ctx)
	if !errors.Is(err, errMissingAPIKey) || !strings.Contains(err.Error(), "ZAP_API_KEY") {
		t.Fatalf("%v", err)
	}
	if c := NewClient("k", WithMaxRetries(-1)); c.c.maxRetries != 0 {
		t.Fatal(c.c.maxRetries)
	}
}

func TestHeaders(t *testing.T) {
	f := newFake(t, answer(200, `{"id":"x"}`))
	_, err := f.client(WithDefaultHeader("X-Team", "mail")).Emails.Get(ctx, "a/b c", WithHeader("X-Extra", "1"))
	mustNil(t, err)
	r := f.calls()[0]
	if r.path != "/v1/emails/a%2Fb%20c" {
		t.Fatal(r.path)
	}
	ua := regexp.MustCompile(`^zapsmtp-go/` + regexp.QuoteMeta(Version) + ` \(go/` + regexp.QuoteMeta(strings.TrimPrefix(runtime.Version(), "go")) + `\)$`)
	if !ua.MatchString(r.header.Get("User-Agent")) {
		t.Fatal(r.header.Get("User-Agent"))
	}
	if r.header.Get("X-Team") != "mail" || r.header.Get("X-Extra") != "1" || r.header.Get("Accept") != "application/json" {
		t.Fatal(r.header)
	}
	if r.header.Get("Content-Type") != "" || len(r.body) != 0 {
		t.Fatal("a GET has no body")
	}
}

func TestQuery(t *testing.T) {
	f := newFake(t, page(false))
	_, err := f.client().Emails.List(ctx, &EmailsListParams{Limit: Int(5), Metadata: map[string]string{"order_id": "A-1", "a b": "c"}})
	mustNil(t, err)
	r := f.calls()[0]
	want := url.Values{"limit": {"5"}, "metadata[order_id]": {"A-1"}, "metadata[a b]": {"c"}}
	if r.query.Encode() != want.Encode() {
		t.Fatal(r.query)
	}
	_, err = f.client().Emails.List(ctx, nil)
	mustNil(t, err)
	if q := f.calls()[1].query; len(q) != 0 {
		t.Fatal(q)
	}

	type custom struct {
		Tags   []string          `url:"tag"`
		None   *string           `url:"none"`
		Rate   *float64          `url:"rate"`
		On     bool              `url:"on"`
		At     time.Duration     `url:"at"`
		Status *EmailStatus      `url:"status"`
		Meta   map[string]string `url:"m"`
		Skip   string
	}
	q := encodeQuery(&custom{Tags: []string{"a", "b"}, Rate: Float(0.25), On: true, At: time.Second, Status: Ptr(EmailStatusBounced), Meta: map[string]string{"z": "1", "a": "2"}})
	if got := q.Encode(); got != "at=1000000000&m%5Ba%5D=2&m%5Bz%5D=1&on=true&rate=0.25&status=bounced&tag=a&tag=b" {
		t.Fatal(got)
	}
	if v := queryValue(reflect.ValueOf(struct{}{})); v != "{}" {
		t.Fatal(v)
	}
	if len(encodeQuery(nil)) != 0 || len(encodeQuery(custom{})) != 0 {
		t.Fatal("not a pointer")
	}
}

func TestSendBodyAndIdempotencyKey(t *testing.T) {
	f := newFake(t, answer(200, `{"id":"e1"}`))
	c := f.client()
	res, err := c.Emails.Send(ctx, &SendEmailParams{From: String("a@x.test"), To: []string{"b@y.test"}, Subject: String("Hi"), Text: String("Hello")})
	mustNil(t, err)
	if res.ID != "e1" || string(res.Raw) != `{"id":"e1"}` {
		t.Fatalf("%+v", res)
	}
	r := f.calls()[0]
	if r.header.Get("Content-Type") != "application/json" {
		t.Fatal(r.header)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(r.header.Get("Idempotency-Key")) {
		t.Fatal(r.header.Get("Idempotency-Key"))
	}
	if string(r.body) != `{"from":"a@x.test","to":["b@y.test"],"subject":"Hi","text":"Hello"}` {
		t.Fatal(string(r.body))
	}
	_, err = c.Emails.Send(ctx, &SendEmailParams{To: []string{"b"}}, WithIdempotencyKey("order-1"))
	mustNil(t, err)
	if k := f.calls()[1].header.Get("Idempotency-Key"); k != "order-1" {
		t.Fatal(k)
	}
	// no Idempotency-Key where the API takes none
	_, err = c.Domains.Create(ctx, &DomainsCreateParams{Name: "a.test"})
	mustNil(t, err)
	if k := f.calls()[2].header.Get("Idempotency-Key"); k != "" {
		t.Fatal(k)
	}
}

func TestEmptyBodies(t *testing.T) {
	f := newFake(t, answer(200, `{}`))
	c := f.client()
	_, err := c.Emails.SpamCheck(ctx, nil)
	mustNil(t, err)
	_, err = c.Emails.SendBatch(ctx, nil)
	mustNil(t, err)
	_, err = c.Emails.Cancel(ctx, "e1")
	mustNil(t, err)
	calls := f.calls()
	if string(calls[0].body) != "{}" || string(calls[1].body) != "[]" || len(calls[2].body) != 0 {
		t.Fatalf("%s %s %s", calls[0].body, calls[1].body, calls[2].body)
	}
	_, err = c.Templates.Create(ctx, &TemplatesCreateParams{Name: "x", Design: make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "encoding the request body") {
		t.Fatal(err)
	}
}

func TestBatchValidation(t *testing.T) {
	f := newFake(t, answer(200, `{"data":[],"errors":[]}`))
	c := f.client()
	_, err := c.Emails.SendBatch(ctx, []SendEmailParams{{To: []string{"b"}}})
	mustNil(t, err)
	_, err = c.Emails.SendBatch(ctx, []SendEmailParams{{To: []string{"b"}}}, WithBatchValidation(BatchValidationPermissive))
	mustNil(t, err)
	calls := f.calls()
	if calls[0].header.Get("X-Batch-Validation") != "" || calls[1].header.Get("X-Batch-Validation") != "permissive" {
		t.Fatal("x-batch-validation")
	}
	if string(calls[0].body) != `[{"to":["b"]}]` {
		t.Fatal(string(calls[0].body))
	}
}

func TestNoContentAndText(t *testing.T) {
	f := newFake(t, answer(204, ""), answer(200, "plain"), answer(204, ""))
	c := f.client()
	mustNil(t, c.Domains.Delete(ctx, "d1"))
	u, err := c.Usage.Get(ctx)
	mustNil(t, err)
	if string(u.Raw) != `"plain"` || u.Plan != nil {
		t.Fatalf("%s", u.Raw)
	}
	d, err := c.Templates.Delete(ctx, "t1")
	mustNil(t, err)
	if d.Deleted || d.Raw != nil {
		t.Fatalf("%+v", d)
	}
}

func TestWithResponse(t *testing.T) {
	f := newFake(t, answer(200, `{"id":"e1"}`, "RateLimit-Remaining", "9"), apiError(404, "not_found"))
	c := f.client()
	var resp Response
	_, err := c.Emails.Get(ctx, "e1", WithResponse(&resp))
	mustNil(t, err)
	if resp.StatusCode != 200 || resp.RequestID != "req_1" || resp.Header.Get("RateLimit-Remaining") != "9" {
		t.Fatalf("%+v", resp)
	}
	_, err = c.Emails.Get(ctx, "e2", WithResponse(&resp))
	if !IsNotFound(err) || resp.StatusCode != 404 {
		t.Fatalf("%v %+v", err, resp)
	}
}

func TestUploadFile(t *testing.T) {
	f := newFake(t, answer(201, `{"id":"img"}`))
	c := f.client()
	a, err := c.TemplateAssets.UploadFile(ctx, bytes.NewReader([]byte{1, 2, 3}), "logo.png", "")
	mustNil(t, err)
	if a.ID != "img" {
		t.Fatal(a.ID)
	}
	_, err = c.TemplateAssets.UploadFile(ctx, strings.NewReader("x"), "", "a.gif")
	mustNil(t, err)
	_, err = c.TemplateAssets.UploadFile(ctx, strings.NewReader("x"), "", "")
	mustNil(t, err)
	type part struct{ file, filename, name string }
	read := func(r recorded) part {
		_, params, err := mime.ParseMediaType(r.header.Get("Content-Type"))
		mustNil(t, err)
		mr := multipart.NewReader(bytes.NewReader(r.body), params["boundary"])
		var p part
		for {
			pt, err := mr.NextPart()
			if err == io.EOF {
				return p
			}
			mustNil(t, err)
			b, _ := io.ReadAll(pt)
			if pt.FormName() == "file" {
				p.file, p.filename = string(b), pt.FileName()
			} else {
				p.name = string(b)
			}
		}
	}
	calls := f.calls()
	if p := read(calls[0]); p != (part{"\x01\x02\x03", "logo.png", "logo.png"}) {
		t.Fatalf("%+v", p)
	}
	if p := read(calls[1]); p != (part{"x", "a.gif", ""}) {
		t.Fatalf("%+v", p)
	}
	if p := read(calls[2]); p.filename != "image" {
		t.Fatalf("%+v", p)
	}
	if calls[0].method != http.MethodPost || calls[0].path != "/v1/template-assets" {
		t.Fatal(calls[0].path)
	}
	_, err = c.TemplateAssets.UploadFile(ctx, errReader{}, "x", "")
	if err == nil || !strings.Contains(err.Error(), "reading the file") {
		t.Fatal(err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestInvalidBaseURL(t *testing.T) {
	_, err := NewClient("k", WithBaseURL("http://a b")).Usage.Get(ctx)
	if err == nil || !strings.HasPrefix(err.Error(), "zapsmtp: ") {
		t.Fatal(err)
	}
}

// ---- errors ----------------------------------------------------------------------

func TestErrorKinds(t *testing.T) {
	kinds := []error{ErrAuthentication, ErrPermission, ErrNotFound, ErrConflict, ErrValidation, ErrRateLimit, ErrServer, ErrConnection}
	helpers := []func(error) bool{IsAuthentication, IsPermission, IsNotFound, IsConflict, IsValidation, IsRateLimit, IsServer, IsConnection}
	for _, tc := range []struct {
		status int
		code   string
		kind   error
	}{
		{400, "validation_error", ErrValidation}, {401, "invalid_api_key", ErrAuthentication}, {403, "restricted_api_key", ErrPermission},
		{404, "not_found", ErrNotFound}, {409, "conflict", ErrConflict}, {413, "payload_too_large", ErrValidation},
		{422, "invalid_parameter", ErrValidation}, {429, "monthly_quota_exceeded", ErrRateLimit}, {500, "internal_error", ErrServer},
		{410, "link_expired", nil},
	} {
		f := newFake(t, apiError(tc.status, tc.code))
		_, err := f.client(WithMaxRetries(0)).Emails.Get(ctx, "x")
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("%d: %v", tc.status, err)
		}
		if e.StatusCode != tc.status || e.Code != tc.code || e.Message != tc.code+" happened" || e.Type != "invalid_request_error" || e.RequestID != "req_1" ||
			e.Header.Get("X-Request-Id") != "req_1" || !strings.Contains(string(e.Body), tc.code) {
			t.Fatalf("%+v", e)
		}
		want := fmt.Sprintf("zapsmtp: %d %s: %s happened (request req_1)", tc.status, tc.code, tc.code)
		if e.Error() != want {
			t.Fatal(e.Error())
		}
		for i, k := range kinds {
			if errors.Is(err, k) != (k == tc.kind) || helpers[i](err) != (k == tc.kind) {
				t.Fatalf("%d is %v: %v", tc.status, k, errors.Is(err, k))
			}
		}
	}
}

func TestErrorShapes(t *testing.T) {
	f := newFake(t, answer(422, `{"statusCode":422,"name":"missing_required_field","message":"subject is required"}`))
	_, err := f.client().Emails.Get(ctx, "x")
	var e *Error
	if !errors.As(err, &e) || e.Code != "missing_required_field" || e.Message != "subject is required" || e.Type != "" {
		t.Fatalf("%+v", e)
	}
	f = newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(418)
		_, _ = io.WriteString(w, "bad gateway")
	})
	_, err = f.client().Emails.Get(ctx, "x")
	if !errors.As(err, &e) || e.StatusCode != 418 || e.Code != "" || e.Message != "HTTP 418" || e.RequestID != "" || e.Error() != "zapsmtp: 418 HTTP 418" {
		t.Fatalf("%+v %s", e, e)
	}
}

func TestRetryAfterOnError(t *testing.T) {
	f := newFake(t, apiError(429, "monthly_quota_exceeded", "Retry-After", "7"))
	_, err := f.client().Emails.Get(ctx, "x")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatal(err)
	}
	if d, ok := e.RetryAfter(); !ok || d != 7*time.Second {
		t.Fatal(d, ok)
	}
	if d, ok := (&Error{}).RetryAfter(); ok || d != 0 {
		t.Fatal(d)
	}
	if len(f.calls()) != 1 {
		t.Fatal("a spent quota is not retried")
	}
}

func TestConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	c := NewClient("k", WithBaseURL(base), WithMaxRetries(0))
	_, err := c.Emails.Get(ctx, "x")
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != 0 || !IsConnection(err) || !strings.HasPrefix(e.Error(), "zapsmtp: connection error: ") {
		t.Fatalf("%v", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) || e.Unwrap() == nil {
		t.Fatal("the cause is wrapped")
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	_, err := f.client(WithMaxRetries(0), WithTimeout(20*time.Millisecond)).Emails.Get(ctx, "x")
	if !IsConnection(err) || !strings.Contains(err.Error(), "timed out after 20ms") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	_, err = f.client(WithMaxRetries(0)).Emails.Get(ctx, "x", WithRequestTimeout(10*time.Millisecond))
	if !strings.Contains(err.Error(), "timed out after 10ms") {
		t.Fatalf("%v", err)
	}
}

func TestTimeoutCoversTheBody(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "{")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	_, err := f.client(WithMaxRetries(0), WithTimeout(50*time.Millisecond)).Emails.Get(ctx, "x")
	if !IsConnection(err) || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("%v", err)
	}
}

func TestNoTimeout(t *testing.T) {
	f := newFake(t, answer(200, `{"id":"e1"}`))
	e, err := f.client(WithTimeout(0)).Emails.Get(ctx, "e1")
	mustNil(t, err)
	if e.ID != "e1" {
		t.Fatal(e.ID)
	}
}

// brokenBody promises more bytes than it sends, so reading the body fails.
func brokenBody(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", "100")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, `{"id":`)
}

func TestBrokenBody(t *testing.T) {
	f := newFake(t, brokenBody)
	_, err := f.client(WithMaxRetries(0)).Emails.Get(ctx, "e1")
	if !IsConnection(err) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("%v", err)
	}
	f = newFake(t, brokenBody, answer(200, `{"id":"e1"}`))
	e, err := f.client().Emails.Get(ctx, "e1")
	mustNil(t, err)
	if e.ID != "e1" || len(f.calls()) != 2 {
		t.Fatal(len(f.calls()))
	}
}

func TestCancelledBeforeSending(t *testing.T) {
	f := newFake(t, answer(200, `{}`))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := f.client().Emails.Get(cctx, "e1")
	if !errors.Is(err, context.Canceled) || len(f.calls()) != 0 {
		t.Fatalf("%v, %d calls", err, len(f.calls()))
	}
	if _, isAPI := err.(*Error); isAPI {
		t.Fatal("the caller's cancellation is returned as is")
	}
}

func TestCancelledInFlight(t *testing.T) {
	cctx, cancel := context.WithCancel(ctx)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})
	_, err := f.client().Emails.Get(cctx, "e1")
	if !errors.Is(err, context.Canceled) || len(f.calls()) != 1 {
		t.Fatalf("%v, %d calls", err, len(f.calls()))
	}
}

func TestCancelledWhileWaiting(t *testing.T) {
	cctx, cancel := context.WithCancel(ctx)
	f := newFake(t, apiError(503, "service_unavailable", "Retry-After", "30"))
	c := NewClient("k", WithBaseURL(f.srv.URL))
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.Emails.Get(cctx, "e1")
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("%v", err)
	}
	// and a connection error's wait
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	cctx, cancel = context.WithCancel(ctx)
	time.AfterFunc(20*time.Millisecond, cancel)
	c = NewClient("k", WithBaseURL(srv.URL), WithMaxRetries(10))
	_, err = c.Emails.Get(cctx, "e1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	// sleepContext runs to its end when nothing cancels it
	mustNil(t, sleepContext(ctx, time.Millisecond))
}

// ---- retries ---------------------------------------------------------------------

func TestRetriesSafeRequests(t *testing.T) {
	f := newFake(t, apiError(503, "service_unavailable", "Retry-After", "2"), brokenBody, answer(200, `{"id":"e1"}`))
	e, err := f.client().Emails.Get(ctx, "e1")
	mustNil(t, err)
	if e.ID != "e1" || len(f.calls()) != 3 {
		t.Fatal(len(f.calls()))
	}
	if f.sleeps[0] != 2*time.Second || f.sleeps[1] < 500*time.Millisecond || f.sleeps[1] > time.Second {
		t.Fatal(f.sleeps)
	}
}

func TestRetriesASendWithTheSameKey(t *testing.T) {
	f := newFake(t, apiError(502, "bad_gateway"), answer(200, `{"id":"e1"}`))
	_, err := f.client().Emails.Send(ctx, &SendEmailParams{To: []string{"b"}})
	mustNil(t, err)
	calls := f.calls()
	if len(calls) != 2 || calls[0].header.Get("Idempotency-Key") != calls[1].header.Get("Idempotency-Key") || string(calls[1].body) != `{"to":["b"]}` {
		t.Fatal("same key, same body")
	}
}

func TestDoesNotRetryUnsafeRequests(t *testing.T) {
	f := newFake(t, apiError(500, "internal_error"))
	_, err := f.client().Domains.Create(ctx, &DomainsCreateParams{Name: "a.test"})
	if !IsServer(err) || len(f.calls()) != 1 {
		t.Fatal(err)
	}
	f = newFake(t, brokenBody)
	_, err = f.client().Domains.Create(ctx, &DomainsCreateParams{Name: "a.test"})
	if !IsConnection(err) || len(f.calls()) != 1 {
		t.Fatal(err)
	}
	f = newFake(t, apiError(429, "rate_limit_exceeded", "Retry-After", "1"), answer(201, `{"id":"d1"}`))
	d, err := f.client().Domains.Create(ctx, &DomainsCreateParams{Name: "a.test"})
	mustNil(t, err)
	if d.ID != "d1" || f.sleeps[0] != time.Second {
		t.Fatal(f.sleeps)
	}
}

func TestGivesUp(t *testing.T) {
	f := newFake(t, apiError(503, "service_unavailable", "Retry-After", "0"))
	_, err := f.client(WithMaxRetries(3)).Emails.Get(ctx, "x")
	if !IsServer(err) || len(f.calls()) != 4 {
		t.Fatal(len(f.calls()))
	}
	f = newFake(t, apiError(503, "service_unavailable"))
	_, err = f.client().Emails.Get(ctx, "x", WithRequestMaxRetries(0))
	if !IsServer(err) || len(f.calls()) != 1 {
		t.Fatal(len(f.calls()))
	}
	if c := f.client(); func() int { rc := requestConfig{}; WithRequestMaxRetries(-3)(&rc); return rc.maxRetries }() != 0 || c == nil {
		t.Fatal("negative retries")
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		safe   bool
		want   bool
	}{
		{429, "overloaded", false, true}, {429, "daily_quota_exceeded", true, false}, {429, "thread_reply_limit", true, false},
		{409, "idempotency_request_in_progress", false, true}, {409, "idempotency_key_reused", true, false},
		{503, "service_unavailable", true, true}, {503, "service_unavailable", false, false}, {408, "", true, true},
		{422, "invalid_parameter", true, false},
	} {
		if retryable(tc.status, tc.code, tc.safe) != tc.want {
			t.Errorf("%+v", tc)
		}
	}
}

func TestBackoff(t *testing.T) {
	for a := range 8 {
		limit := time.Duration(min(500<<a, 8000)) * time.Millisecond
		for range 20 {
			if d := backoff(a, nil); d < limit/2 || d > limit {
				t.Fatalf("attempt %d: %s", a, d)
			}
		}
	}
	three, long := 3.0, 600.0
	if backoff(0, &three) != 3*time.Second || backoff(0, &long) != 60*time.Second {
		t.Fatal("Retry-After")
	}
}

func TestRetryAfterHeader(t *testing.T) {
	defer func(f func() time.Time) { timeNow = f }(timeNow)
	timeNow = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	h := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	for v, want := range map[string]float64{"7": 7, " 1.5 ": 1.5, "-3": 0, "Thu, 01 Jan 2026 00:00:03 GMT": 3, "Wed, 01 Jan 2025 00:00:00 GMT": 0} {
		if got := retryAfterSeconds(h(v)); got == nil || *got != want {
			t.Errorf("%q: %v", v, got)
		}
	}
	for _, v := range []string{"", "soon", "Inf", "NaN"} {
		if got := retryAfterSeconds(h(v)); got != nil {
			t.Errorf("%q: %v", v, *got)
		}
	}
	if retryAfterSeconds(nil) != nil {
		t.Fatal("nil header")
	}
}

// ---- decoding --------------------------------------------------------------------

func TestLenientDecoding(t *testing.T) {
	data := `{"id":"e1","object":"email","to":"not-a-list","created_at":"yesterday","events":[{"status":"sent","attempts":"two"}],"subject":"Hi","new_field":{"x":1}}`
	var e Email
	mustNil(t, json.Unmarshal([]byte(data), &e))
	if e.ID != "e1" || e.Subject != "Hi" || e.To != nil || e.CreatedAt != nil || len(e.Events) != 1 || e.Events[0].Status != "sent" || e.Events[0].Attempts != 0 {
		t.Fatalf("%+v", e)
	}
	if string(e.Raw) != data {
		t.Fatal(string(e.Raw))
	}
	var notObject Email
	mustNil(t, json.Unmarshal([]byte(`[1,2]`), &notObject))
	if notObject.ID != "" || string(notObject.Raw) != `[1,2]` {
		t.Fatalf("%+v", notObject)
	}
	// a page with a bad item and a bad field
	f := newFake(t, answer(200, `{"object":"list","has_more":"yes","data":[{"id":7},{"id":"b"}]}`))
	p, err := f.client().Emails.List(ctx, nil)
	mustNil(t, err)
	if p.HasMore || len(p.Data) != 2 || p.Data[0].ID != "" || p.Data[1].ID != "b" {
		t.Fatalf("%+v", p)
	}
}

func TestNullable(t *testing.T) {
	b, err := json.Marshal(InboundRoutesUpdateParams{WebhookID: Null[string](), ForwardID: NewNullable("f1")})
	mustNil(t, err)
	if string(b) != `{"webhook_id":null,"forward_id":"f1"}` {
		t.Fatal(string(b))
	}
	if v, ok := NewNullable(3).Get(); !ok || v != 3 {
		t.Fatal(v)
	}
	if _, ok := Null[int]().Get(); ok {
		t.Fatal("null")
	}
	if *String("s") != "s" || *Int(1) != 1 || !*Bool(true) || *Float(1.5) != 1.5 || *Ptr(StreamBroadcast) != StreamBroadcast {
		t.Fatal("helpers")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWithHTTPClient(t *testing.T) {
	var seen string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.URL.String()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"e1"}`))}, nil
	})}
	e, err := NewClient("k", WithHTTPClient(hc), WithBaseURL("https://api.test/v1")).Emails.Get(ctx, "e1")
	mustNil(t, err)
	if e.ID != "e1" || seen != "https://api.test/v1/emails/e1" {
		t.Fatal(seen)
	}
}

func TestPathArgumentsRefusedBeforeSending(t *testing.T) {
	f := newFake(t, answer(200, `{}`))
	c := f.client()
	for _, tc := range []struct {
		call func() error
		msg  string
	}{
		{func() error { _, err := c.Emails.Get(ctx, ""); return err }, "id must not be empty"},
		{func() error { _, err := c.Emails.Get(ctx, "."); return err }, `id must not be "."`},
		{func() error { _, err := c.Emails.Receiving.Attachments.Get(ctx, "m1", ".."); return err }, `aid must not be ".."`},
		{func() error { return c.Suppressions.Delete(ctx, "") }, "email must not be empty"},
	} {
		err := tc.call()
		var e *Error
		if !errors.As(err, &e) || !IsValidation(err) || IsConnection(err) || e.Code != "invalid_parameter" || e.Message != tc.msg || e.StatusCode != 0 ||
			e.Error() != "zapsmtp: invalid_parameter: "+tc.msg {
			t.Fatalf("%v", err)
		}
	}
	if len(f.calls()) != 0 {
		t.Fatal("nothing is sent")
	}
	_, err := c.Emails.Receiving.Attachments.Get(ctx, "m 1", "a/b")
	mustNil(t, err)
	if p := f.calls()[0].path; p != "/v1/emails/receiving/m%201/attachments/a%2Fb" {
		t.Fatal(p)
	}
}

func TestEmptyCollectionsAreSent(t *testing.T) {
	f := newFake(t, answer(200, `{"id":"t1"}`))
	c := f.client()
	_, err := c.Templates.Update(ctx, "t1", &TemplatesUpdateParams{Variables: map[string]any{}, VariableSchema: []VariableSpec{}})
	mustNil(t, err)
	_, err = c.Templates.Update(ctx, "t1", &TemplatesUpdateParams{Name: String("n")})
	mustNil(t, err)
	_, err = c.Webhooks.Update(ctx, "w1", &WebhooksUpdateParams{Events: []WebhookEventType{}, Filter: &WebhookFilter{Actions: []string{}}})
	mustNil(t, err)
	calls := f.calls()
	for i, want := range []string{`{"variables":{},"variable_schema":[]}`, `{"name":"n"}`, `{"events":[],"filter":{"actions":[]}}`} {
		if string(calls[i].body) != want {
			t.Errorf("%s, want %s", calls[i].body, want)
		}
	}
}
