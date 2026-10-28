// Package zapsmtp is the official Go client of the ZapSMTP email API: send,
// receive and track email, manage templates, domains, webhooks and everything
// else the API does.
//
// Make a client with NewClient; each of its fields is a group of operations:
//
//	client := zapsmtp.NewClient("zap_...") // or "" to read ZAP_API_KEY
//	sent, err := client.Emails.Send(ctx, &zapsmtp.SendEmailParams{
//		From:    zapsmtp.String("Acme <hello@acme.com>"),
//		To:      []string{"jane@example.org"},
//		Subject: zapsmtp.String("Your receipt"),
//		HTML:    zapsmtp.String("<p>Thanks for your order.</p>"),
//	})
//
// Every method takes a context.Context first and RequestOption values last
// (WithIdempotencyKey, WithHeader, WithRequestTimeout, WithRequestMaxRetries,
// WithResponse).
//
// # Params and responses
//
// Optional fields of params are pointers: set them with String, Int, Bool,
// Float or Ptr. A field the API lets you clear is a *Nullable: NewNullable(v)
// sets it, Null[T]() clears it. Enums are string types with named constants,
// such as StreamBroadcast. A nil slice or map is left out of the request; an
// empty, non-nil one is sent as [] or {}, which clears the field on update.
//
// Response models keep the whole JSON object in Raw, unknown fields included,
// and their decoding never fails on a field of an unexpected type: that field
// is left empty and Raw still has it.
//
// # Errors
//
// Every API error is an *Error with the status, code, message, type and
// request id. Check its kind with errors.Is (ErrNotFound, ErrRateLimit, …) or
// the IsNotFound-style helpers. A request that got no answer is an *Error too
// (StatusCode 0, ErrConnection) wrapping its cause. When the context ends, its
// error is returned as is.
//
// # Retries
//
// Requests that are safe to repeat are retried twice by default on 408, 429,
// 5xx and lost connections, honouring Retry-After: reads, PUT and DELETE, and
// sends, which always carry an Idempotency-Key. Other POST and PATCH requests
// are retried only on 429.
//
// # Lists
//
// A List method returns one Page; its AutoPaging variant is an iter.Seq2 over
// every item of every page:
//
//	for email, err := range client.Emails.ListAutoPaging(ctx, nil) {
//		if err != nil {
//			return err
//		}
//		fmt.Println(email.ID)
//	}
//
// # Webhooks
//
// VerifyWebhook checks the Zap-Signature header against the raw body and
// returns the event. A webhook with batch delivery sends an array of events:
// read it with VerifyWebhookEvents, which returns the events of either kind.
package zapsmtp
