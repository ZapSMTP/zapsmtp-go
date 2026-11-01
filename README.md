# ZapSMTP Go SDK

The official Go library for the [ZapSMTP](https://zapsmtp.com) email API: send, receive and track
email, manage templates, domains, webhooks and everything else the API does.

- Every API operation, with typed params and response models
- Safe automatic retries: sends carry an `Idempotency-Key`, so a retry never sends twice
- `for ... range` over every page of any list (Go 1.23 iterators)
- Webhook signature verification
- `context.Context` everywhere; standard library only

## Install

```bash
go get github.com/zapsmtp/zapsmtp-go
```

Go 1.23 or newer.

## Send an email

```go
import "github.com/zapsmtp/zapsmtp-go"

client := zapsmtp.NewClient("zap_...") // or "" to read ZAP_API_KEY

sent, err := client.Emails.Send(ctx, &zapsmtp.SendEmailParams{
	From:     zapsmtp.String("Acme <hello@acme.com>"),
	To:       []string{"jane@example.org"},
	Subject:  zapsmtp.String("Your receipt"),
	HTML:     zapsmtp.String("<p>Thanks for your order.</p>"),
	Tags:     []zapsmtp.Tag{{Name: "category", Value: "receipt"}},
	Metadata: map[string]string{"order_id": "A-1042"},
})
if err != nil {
	return err
}
fmt.Println(sent.ID)
```

Optional fields are pointers: set them with `zapsmtp.String`, `Int`, `Bool`, `Float`, or `Ptr` for
enums (`Stream: zapsmtp.Ptr(zapsmtp.StreamBroadcast)`). A field the API lets you clear is a
`*Nullable`: `zapsmtp.NewNullable("wh_1")` sets it, `zapsmtp.Null[string]()` sends `null`.

A nil slice or map is left out; an empty, non-nil one is sent as `[]` or `{}` (as Go 1.24's
`omitzero` does, on every Go version). On an update that clears the field:

```go
client.Templates.Update(ctx, id, &zapsmtp.TemplatesUpdateParams{Variables: map[string]any{}}) // clears the sample variables
```

An empty id, `"."` or `".."` is refused before anything is sent, with an `*Error` of kind
`ErrValidation` and code `invalid_parameter`.

With a template:

```go
_, err := client.Emails.Send(ctx, &zapsmtp.SendEmailParams{
	From:     zapsmtp.String("Acme <hello@acme.com>"),
	To:       []string{"jane@example.org"},
	Template: &zapsmtp.TemplateRef{ID: "receipt", Variables: map[string]any{"name": "Jane", "total": 42}},
})
```

Attachments:

```go
invoice, err := zapsmtp.AttachmentFromFile("invoice.pdf") // or zapsmtp.AttachmentFromBytes(name, b)
if err != nil {
	return err
}
_, err = client.Emails.Send(ctx, &zapsmtp.SendEmailParams{
	From:        zapsmtp.String("Acme <hello@acme.com>"),
	To:          []string{"jane@example.org"},
	Subject:     zapsmtp.String("Your invoice"),
	Text:        zapsmtp.String("Attached."),
	Attachments: []zapsmtp.OutgoingAttachment{invoice},
})
```

Images for templates, from a file or a URL:

```go
f, err := os.Open("logo.png")
if err != nil {
	return err
}
defer f.Close()
// name is the asset's name, filename the file's (empty: the name)
asset, err := client.TemplateAssets.UploadFile(ctx, f, "Logo", "logo.png")
// or: client.TemplateAssets.Upload(ctx, &zapsmtp.TemplateAssetsUploadParams{URL: "https://acme.com/logo.png"})
```

A parameter with fixed values has a type and constants, such as
`zapsmtp.Ptr(zapsmtp.GranularityDay)` or `zapsmtp.EmailStatusBounced`; any string converts to it,
so a value newer than this SDK works too.

Up to 100 emails at once:

```go
res, err := client.Emails.SendBatch(ctx, emails, zapsmtp.WithBatchValidation(zapsmtp.BatchValidationPermissive))
for _, e := range res.Errors {
	log.Printf("email %d not sent: %s", e.Index, e.Message)
}
```

## Lists

`List` returns one page; `ListAutoPaging` walks every page, asking for each as the loop reaches it.

```go
page, err := client.Emails.List(ctx, &zapsmtp.EmailsListParams{Limit: zapsmtp.Int(50)})
// page.Data, page.HasMore, page.HasNextPage(), page.GetNextPage(ctx)

params := &zapsmtp.EmailsListParams{Status: zapsmtp.Ptr(zapsmtp.EmailStatusBounced)}
for email, err := range client.Emails.ListAutoPaging(ctx, params) {
	if err != nil {
		return err
	}
	fmt.Println(email.ID, email.To)
}
```

## Errors

Every API error is a `*zapsmtp.Error` with `StatusCode`, `Code`, `Message`, `Type`, `RequestID`,
`Header` and `Body`.

```go
_, err := client.Emails.Send(ctx, params)
var apiErr *zapsmtp.Error
switch {
case err == nil:
case zapsmtp.IsValidation(err) && errors.As(err, &apiErr):
	log.Println(apiErr.Code, apiErr.Message, apiErr.RequestID) // e.g. invalid_parameter
case zapsmtp.IsRateLimit(err) && errors.As(err, &apiErr):
	wait, _ := apiErr.RetryAfter()
	log.Println("retry in", wait)
default:
	return err
}
```

| Kind (`errors.Is`) | Helper | When |
|---|---|---|
| `ErrAuthentication` | `IsAuthentication` | 401: missing or invalid key |
| `ErrPermission` | `IsPermission` | 403: the key may not do this, or the domain is not verified |
| `ErrNotFound` | `IsNotFound` | 404 |
| `ErrConflict` | `IsConflict` | 409 |
| `ErrValidation` | `IsValidation` | 400, 413, 422: the request is wrong |
| `ErrRateLimit` | `IsRateLimit` | 429: `RetryAfter()` says how long to wait |
| `ErrServer` | `IsServer` | 5xx |
| `ErrConnection` | `IsConnection` | no answer: network failure or timeout (`StatusCode` 0, the cause in `errors.Unwrap`) |

When your context is cancelled or its deadline passes, the call stops at once and returns the
context's error as is; an auto-paging loop ends with that error, even between the items of a page.

## Retries and timeouts

A request that failed on the way (408, 429, 500, 502, 503, 504, a lost connection) is tried again twice by default,
waiting as the API's `Retry-After` says or backing off exponentially (0.5 s, 1 s, 2 s, … up to 8 s,
with jitter). Only requests that are safe to repeat are retried: reads, `PUT`, deletes, and sends,
which always carry an `Idempotency-Key` (pass your own with `zapsmtp.WithIdempotencyKey` to make
retries of your own safe too). Other `POST` and `PATCH` requests are retried only on 429. A 429 for a quota or `thread_reply_limit` is never retried; a 409 `idempotency_request_in_progress` is. An empty or blank
key (from `WithIdempotencyKey` or a header) is refused before sending, with an `*Error` of kind
`ErrValidation`: the API would not deduplicate it, so a retry could send twice.

```go
client := zapsmtp.NewClient("zap_...", zapsmtp.WithMaxRetries(4), zapsmtp.WithTimeout(20*time.Second))
email, err := client.Emails.Get(ctx, id, zapsmtp.WithRequestTimeout(5*time.Second), zapsmtp.WithRequestMaxRetries(0))
```

The timeout bounds each attempt, reading the answer included.

## Response headers

```go
var resp zapsmtp.Response
sent, err := client.Emails.Send(ctx, params, zapsmtp.WithResponse(&resp))
fmt.Println(resp.StatusCode, resp.RequestID, resp.Header.Get("RateLimit-Remaining"))
```

`WithResponse` is filled with the last answer, an error answer included, and also when reading the
answer's body failed after its headers arrived.

## Forward compatibility

Every response model keeps the object as the API sent it in `Raw` (a `json.RawMessage`), unknown
fields included, and decoding never fails on a field of an unexpected type: that field is left
empty, and `Raw` still has it. Enums are string types, so a value newer than the SDK still works.

## Webhooks

Check the signature on the raw body, before parsing it:

```go
secret := os.Getenv("ZAP_WEBHOOK_SECRET")

http.HandleFunc("POST /webhooks/zapsmtp", func(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	event, err := zapsmtp.VerifyWebhook(body, r.Header.Get("Zap-Signature"), secret)
	if err != nil { // errors.Is(err, zapsmtp.ErrWebhookSignature)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	if event.Type == string(zapsmtp.WebhookEventTypeEmailBounced) {
		// event.Data, or decode event.Raw into your own type
	}
	w.WriteHeader(http.StatusOK)
})
```

`VerifyWebhook` (also `client.Webhooks.Verify`) checks every `v1` signature in constant time and
refuses one older than five minutes; `zapsmtp.WithTolerance` changes that.

A webhook with batch delivery sends a JSON array of events. `VerifyWebhook` refuses it with
`ErrWebhookBatch` ("the body is a batch of events: use VerifyWebhookEvents"); `VerifyWebhookEvents`
(also `client.Webhooks.VerifyEvents`) returns the events of a batch, or the one event of any other
request, so a handler for either kind of webhook can use it:

```go
events, err := zapsmtp.VerifyWebhookEvents(body, r.Header.Get("Zap-Signature"), secret)
if err != nil {
	http.Error(w, "invalid signature", http.StatusBadRequest)
	return
}
for _, event := range events {
	// ...
}
```

## Configuration

| Option | Environment | Default |
|---|---|---|
| `NewClient(apiKey)` | `ZAP_API_KEY` | required |
| `WithBaseURL` | `ZAP_BASE_URL` | `https://api.zapsmtp.com/v1` |
| `WithTimeout` | | 60 s per attempt |
| `WithMaxRetries` | | 2 |
| `WithDefaultHeader` | | |
| `WithHTTPClient` | | a client on `http.DefaultTransport` |

A client is safe for concurrent use; make one and share it.

## Reference

Every method is listed with its parameters in the [API reference](https://zapsmtp.com/docs/) and on
[pkg.go.dev](https://pkg.go.dev/github.com/zapsmtp/zapsmtp-go). The client's fields mirror it:
`client.Emails`, `client.Emails.Receiving`, `client.Threads`, `client.Domains`, `client.Webhooks`,
`client.Actions`, `client.Templates`, `client.Variables`, `client.TemplateModules`,
`client.TemplateAssets`, `client.TemplateTimers`, `client.Inbound`, `client.Suppressions`,
`client.Stats`, `client.APIKeys`, `client.SMTPCredentials`, `client.Logs`, `client.Usage`,
`client.Simulator`, `client.DedicatedIPs`.

## Development

The `zz_generated_*.go` files are generated from the API reference by `sdks/codegen`
(`node sdks/codegen/generate.mjs --only go`); change the generator, never those files.
`go test -race -cover ./...` runs every API operation against the conformance server
(`sdks/conformance`, started with `node`, or `ZAP_CONFORMANCE_URL`) and the core tests.
