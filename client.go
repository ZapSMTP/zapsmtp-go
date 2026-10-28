package zapsmtp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the API's address when neither WithBaseURL nor ZAP_BASE_URL says otherwise.
const DefaultBaseURL = "https://api.zapsmtp.com/v1"

const (
	defaultTimeout    = 60 * time.Second
	defaultMaxRetries = 2
)

var userAgent = "zapsmtp-go/" + Version + " (go/" + strings.TrimPrefix(runtime.Version(), "go") + ")"

// errMissingAPIKey is returned by every call of a client made without a key.
var errMissingAPIKey = errors.New("zapsmtp: missing API key: pass it to NewClient or set ZAP_API_KEY")

// ClientOption configures a Client; pass it to NewClient.
type ClientOption func(*core)

// WithBaseURL sets the API's base URL (default: ZAP_BASE_URL, else https://api.zapsmtp.com/v1).
func WithBaseURL(baseURL string) ClientOption {
	return func(c *core) { c.baseURL = baseURL }
}

// WithHTTPClient sets the HTTP client requests go through (default: a client with
// http.DefaultTransport). Its Timeout, if any, applies on top of WithTimeout.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *core) { c.http = hc }
}

// WithMaxRetries sets how many times a failed request that is safe to repeat is
// retried (default 2; 0 turns retries off).
func WithMaxRetries(n int) ClientOption {
	return func(c *core) { c.maxRetries = max(n, 0) }
}

// WithTimeout sets the time limit of one attempt, reading the answer included
// (default 60 s). Zero or less means no limit besides the context's.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *core) { c.timeout = d }
}

// WithDefaultHeader adds a header to every request of the client.
func WithDefaultHeader(key, value string) ClientOption {
	return func(c *core) { c.header.Set(key, value) }
}

// NewClient returns a client of the ZapSMTP API.
//
// An empty apiKey reads ZAP_API_KEY. The base URL comes from WithBaseURL, else
// ZAP_BASE_URL, else https://api.zapsmtp.com/v1. Without a key, every call
// returns an error.
func NewClient(apiKey string, opts ...ClientOption) *Client {
	c := &core{
		apiKey:     apiKey,
		baseURL:    os.Getenv("ZAP_BASE_URL"),
		http:       &http.Client{},
		maxRetries: defaultMaxRetries,
		timeout:    defaultTimeout,
		header:     http.Header{},
		sleep:      sleepContext,
	}
	if c.apiKey == "" {
		c.apiKey = os.Getenv("ZAP_API_KEY")
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	for _, o := range opts {
		o(c)
	}
	c.baseURL = strings.TrimRight(c.baseURL, "/")
	cl := &Client{c: c}
	cl.initServices(c)
	return cl
}

// RequestOption configures one call; pass it last to any method.
type RequestOption func(*requestConfig)

type requestConfig struct {
	idempotencyKey *string
	header         http.Header
	timeout        time.Duration
	maxRetries     int
	response       *Response
}

// WithIdempotencyKey sets the Idempotency-Key of a call that takes one
// (Emails.Send, Emails.SendBatch, Emails.Receiving.Reply). Without it the SDK
// sends a random one. Either way every retry of the call carries the same key,
// so the API does the work at most once. An empty or blank key is refused
// before sending: the API would not deduplicate it.
func WithIdempotencyKey(key string) RequestOption {
	return func(r *requestConfig) { r.idempotencyKey = &key }
}

// WithHeader sets a header of one call.
func WithHeader(key, value string) RequestOption {
	return func(r *requestConfig) { r.header.Set(key, value) }
}

// WithRequestTimeout sets the time limit of each attempt of one call (see WithTimeout).
func WithRequestTimeout(d time.Duration) RequestOption {
	return func(r *requestConfig) { r.timeout = d }
}

// WithRequestMaxRetries sets how many times one call may be retried (see WithMaxRetries).
func WithRequestMaxRetries(n int) RequestOption {
	return func(r *requestConfig) { r.maxRetries = max(n, 0) }
}

// Response is what the API answered besides the body.
type Response struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Header holds the response headers: RateLimit-*, X-Quota-*, Retry-After and the rest.
	Header http.Header
	// RequestID is the X-Request-Id header: quote it to support.
	RequestID string
}

// WithResponse fills *resp with the status, headers and request id of the
// call's last answer, the answer to an error included, and an answer whose
// body could not be read. It is left untouched when no answer arrived.
//
//	var resp zapsmtp.Response
//	email, err := client.Emails.Get(ctx, id, zapsmtp.WithResponse(&resp))
//	fmt.Println(resp.RequestID, resp.Header.Get("RateLimit-Remaining"))
func WithResponse(resp *Response) RequestOption {
	return func(r *requestConfig) { r.response = resp }
}

// ---- requests ----------------------------------------------------------------

type paging int

const (
	pagingNone   paging = iota
	pagingBefore        // before = the last item's id
	pagingAfter         // after = the last item's id
	pagingLogs          // after = the first item's id when the caller paged with after, else before = the last's
	pagingThread        // before = the last item's last_message_at, before_id = its id
	pagingCursor        // after = next_cursor
)

type request struct {
	method     string
	path       string   // with {name} placeholders
	pathArgs   []string // name, value pairs for the placeholders
	query      url.Values
	body       any    // sent as JSON
	form       []byte // sent as is, with formType
	formType   string
	idempotent bool // the operation takes Idempotency-Key: the SDK always sends one, so it may retry
	paging     paging
}

// core is what every service shares: the configuration and the HTTP loop.
type core struct {
	apiKey     string
	baseURL    string
	http       *http.Client
	maxRetries int
	timeout    time.Duration
	header     http.Header
	sleep      func(context.Context, time.Duration) error
}

func call[T any](ctx context.Context, c *core, req *request, opts []RequestOption) (*T, error) {
	data, err := c.do(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	out := new(T)
	decodeLenient(data, out)
	return out, nil
}

func callNoContent(ctx context.Context, c *core, req *request, opts []RequestOption) error {
	_, err := c.do(ctx, req, opts)
	return err
}

func callMultipart[T any](ctx context.Context, c *core, req *request, field string, file io.Reader, name, filename string, opts []RequestOption) (*T, error) {
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("zapsmtp: reading the file: %w", err)
	}
	if filename == "" {
		filename = name
	}
	if filename == "" {
		filename = "image"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	// writes to a bytes.Buffer do not fail
	part, _ := w.CreateFormFile(field, filename)
	_, _ = part.Write(content)
	if name != "" {
		_ = w.WriteField("name", name)
	}
	_ = w.Close()
	req.form, req.formType = buf.Bytes(), w.FormDataContentType()
	return call[T](ctx, c, req, opts)
}

// expandPath fills the path's placeholders with escaped arguments. An empty
// argument, "." or ".." would name another resource, so it is refused before
// anything is sent.
func (r *request) expandPath() (string, error) {
	path := r.path
	for i := 0; i+1 < len(r.pathArgs); i += 2 {
		name, v := r.pathArgs[i], r.pathArgs[i+1]
		if v == "" || v == "." || v == ".." {
			what := "empty"
			if v != "" {
				what = strconv.Quote(v)
			}
			return "", &Error{Code: "invalid_parameter", Message: name + " must not be " + what, local: ErrValidation}
		}
		path = strings.Replace(path, "{"+name+"}", url.PathEscape(v), 1)
	}
	return path, nil
}

// encode returns the request body and its content type.
func (r *request) encode() ([]byte, string, error) {
	if r.form != nil {
		return r.form, r.formType, nil
	}
	if r.body == nil {
		return nil, "", nil
	}
	b, err := json.Marshal(r.body)
	if err != nil {
		return nil, "", fmt.Errorf("zapsmtp: encoding the request body: %w", err)
	}
	if string(b) == "null" { // nil params: an empty object; a nil batch: an empty list
		b = []byte("{}")
		if reflect.ValueOf(r.body).Kind() == reflect.Slice {
			b = []byte("[]")
		}
	}
	return b, "application/json", nil
}

// do sends a request, retrying it when that is safe, and returns the body of a
// 2xx answer. A body that is not JSON comes back as a JSON string, so models
// still decode (empty) and keep it in Raw.
func (c *core) do(ctx context.Context, req *request, opts []RequestOption) ([]byte, error) {
	if c.apiKey == "" {
		return nil, errMissingAPIKey
	}
	rc := requestConfig{header: http.Header{}, timeout: c.timeout, maxRetries: c.maxRetries}
	for _, o := range opts {
		o(&rc)
	}
	path, err := req.expandPath()
	if err != nil {
		return nil, err
	}
	u := c.baseURL + path
	if len(req.query) > 0 {
		u += "?" + req.query.Encode()
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.apiKey)
	header.Set("Accept", "application/json")
	header.Set("User-Agent", userAgent)
	for k, v := range c.header {
		header[k] = v
	}
	if req.idempotent {
		key := newUUID()
		if rc.idempotencyKey != nil {
			key = *rc.idempotencyKey
		}
		header.Set("Idempotency-Key", key)
	}
	// the caller's headers replace ours (http.Header keys are canonical, so whatever their case)
	for k, v := range rc.header {
		header[k] = v
	}
	// an empty key turns deduplication off on the server, so a retried send
	// could go out twice: refused before anything is sent
	if v, ok := header["Idempotency-Key"]; ok && strings.TrimSpace(strings.Join(v, "")) == "" {
		return nil, &Error{Code: "invalid_parameter", Message: "idempotency key must not be empty", local: ErrValidation}
	}
	body, contentType, err := req.encode()
	if err != nil {
		return nil, err
	}
	if body != nil {
		header.Set("Content-Type", contentType)
	}
	proto, err := http.NewRequestWithContext(ctx, req.method, u, nil)
	if err != nil {
		return nil, fmt.Errorf("zapsmtp: %w", err)
	}
	proto.Header = header
	safe := req.method == http.MethodGet || req.method == http.MethodPut || req.method == http.MethodDelete || req.idempotent

	for attempt := 0; ; attempt++ {
		res, data, err := c.attempt(ctx, proto, body, rc.timeout)
		if res != nil && rc.response != nil {
			*rc.response = Response{StatusCode: res.StatusCode, Header: res.Header, RequestID: res.Header.Get("X-Request-Id")}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// a timeout or a broken connection (sending, or reading the answer):
			// the request may have reached the server, so only a request that
			// is safe to repeat is sent again
			if attempt < rc.maxRetries && safe {
				if err := c.sleep(ctx, backoff(attempt, nil)); err != nil {
					return nil, err
				}
				continue
			}
			return nil, &Error{Message: err.Error(), cause: err}
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if len(data) > 0 && !json.Valid(data) {
				data, _ = json.Marshal(string(data))
			}
			return data, nil
		}
		apiErr := errorFor(res.StatusCode, data, res.Header)
		if attempt < rc.maxRetries && retryable(res.StatusCode, apiErr.Code, safe) {
			if err := c.sleep(ctx, backoff(attempt, retryAfterSeconds(res.Header))); err != nil {
				return nil, err
			}
			continue
		}
		return nil, apiErr
	}
}

// attempt sends the request once and reads the whole answer, both within the
// timeout: a server that sends its headers and then stalls times out too.
func (c *core) attempt(ctx context.Context, proto *http.Request, body []byte, timeout time.Duration) (*http.Response, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err // cancelled before sending: nothing goes out
	}
	actx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	// res is kept when the body fails, for WithResponse
	fail := func(res *http.Response, err error) (*http.Response, []byte, error) {
		if ctx.Err() == nil && actx.Err() != nil {
			err = fmt.Errorf("timed out after %s: %w", timeout, err)
		}
		return res, nil, err
	}
	r := proto.Clone(actx)
	if body != nil {
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		r.Body, _ = r.GetBody()
		r.ContentLength = int64(len(body))
	}
	res, err := c.http.Do(r)
	if err != nil {
		return fail(nil, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return fail(res, err)
	}
	return res, data, nil
}

// ---- retries -------------------------------------------------------------------

var retryStatuses = map[int]bool{408: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// 429 codes that are not worth another attempt: the quota is spent
var noRetry429 = map[string]bool{"monthly_quota_exceeded": true, "daily_quota_exceeded": true, "thread_reply_limit": true}

// retryable reports whether an error answer is worth another attempt.
func retryable(status int, code string, safe bool) bool {
	switch status {
	case 429:
		return !noRetry429[code] // refused before any work: always safe
	case 409:
		return code == "idempotency_request_in_progress"
	}
	return safe && retryStatuses[status]
}

// backoff is the wait before attempt number attempt+1: Retry-After (seconds)
// up to 60 s when the API said, else 0.5 s, 1 s, 2 s … capped at 8 s, with jitter.
func backoff(attempt int, retryAfter *float64) time.Duration {
	if retryAfter != nil {
		return time.Duration(math.Min(*retryAfter, 60) * float64(time.Second))
	}
	base := math.Min(500*math.Pow(2, float64(attempt)), 8000)
	ms := base/2 + mathrand.Float64()*(base/2)
	return time.Duration(ms * float64(time.Millisecond))
}

// timeNow is the clock (tests replace it).
var timeNow = time.Now

// retryAfterSeconds reads Retry-After: seconds, or an HTTP date.
func retryAfterSeconds(h http.Header) *float64 {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return nil
	}
	var s float64
	if n, err := strconv.ParseFloat(v, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
		s = n
	} else if at, err := http.ParseTime(v); err == nil {
		s = at.Sub(timeNow()).Seconds()
	} else {
		return nil
	}
	s = math.Max(0, s)
	return &s
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
