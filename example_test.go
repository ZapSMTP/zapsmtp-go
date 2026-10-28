package zapsmtp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/zapsmtp/zapsmtp-go"
)

func Example() {
	client := zapsmtp.NewClient("") // reads ZAP_API_KEY
	sent, err := client.Emails.Send(context.Background(), &zapsmtp.SendEmailParams{
		From:     zapsmtp.String("Acme <hello@acme.com>"),
		To:       []string{"jane@example.org"},
		Subject:  zapsmtp.String("Your receipt"),
		HTML:     zapsmtp.String("<p>Thanks for your order.</p>"),
		Tags:     []zapsmtp.Tag{{Name: "category", Value: "receipt"}},
		Metadata: map[string]string{"order_id": "A-1042"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(sent.ID)
}

func ExampleEmailsService_Send_template() {
	client := zapsmtp.NewClient("")
	_, err := client.Emails.Send(context.Background(), &zapsmtp.SendEmailParams{
		From: zapsmtp.String("Acme <hello@acme.com>"),
		To:   []string{"jane@example.org"},
		Template: &zapsmtp.TemplateRef{
			ID:        "receipt",
			Variables: map[string]any{"name": "Jane", "total": 42},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleAttachmentFromFile() {
	invoice, err := zapsmtp.AttachmentFromFile("invoice.pdf")
	if err != nil {
		log.Fatal(err)
	}
	client := zapsmtp.NewClient("")
	_, err = client.Emails.Send(context.Background(), &zapsmtp.SendEmailParams{
		From:        zapsmtp.String("Acme <hello@acme.com>"),
		To:          []string{"jane@example.org"},
		Subject:     zapsmtp.String("Your invoice"),
		Text:        zapsmtp.String("Attached."),
		Attachments: []zapsmtp.OutgoingAttachment{invoice},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func ExampleEmailsService_SendBatch() {
	client := zapsmtp.NewClient("")
	res, err := client.Emails.SendBatch(context.Background(), []zapsmtp.SendEmailParams{
		{From: zapsmtp.String("hello@acme.com"), To: []string{"a@example.org"}, Subject: zapsmtp.String("Hi"), Text: zapsmtp.String("Hello")},
		{From: zapsmtp.String("hello@acme.com"), To: []string{"b@example.org"}, Subject: zapsmtp.String("Hi"), Text: zapsmtp.String("Hello")},
	}, zapsmtp.WithBatchValidation(zapsmtp.BatchValidationPermissive))
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range res.Errors {
		fmt.Println("email", e.Index, "not sent:", e.Message)
	}
}

func ExampleEmailsService_ListAutoPaging() {
	client := zapsmtp.NewClient("")
	params := &zapsmtp.EmailsListParams{Status: zapsmtp.Ptr(zapsmtp.EmailStatusBounced)}
	for email, err := range client.Emails.ListAutoPaging(context.Background(), params) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(email.ID, email.To)
	}
}

func ExampleError() {
	client := zapsmtp.NewClient("")
	_, err := client.Emails.Get(context.Background(), "019a...")
	var apiErr *zapsmtp.Error
	switch {
	case err == nil:
	case zapsmtp.IsNotFound(err):
		fmt.Println("no such email")
	case errors.As(err, &apiErr) && apiErr.StatusCode == 429:
		wait, _ := apiErr.RetryAfter()
		fmt.Println("retry in", wait)
	default:
		log.Fatal(err)
	}
}

func ExampleWithResponse() {
	client := zapsmtp.NewClient("")
	var resp zapsmtp.Response
	if _, err := client.Usage.Get(context.Background(), zapsmtp.WithResponse(&resp)); err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.RequestID, resp.Header.Get("RateLimit-Remaining"))
}

func ExampleNewClient() {
	client := zapsmtp.NewClient("zap_...",
		zapsmtp.WithMaxRetries(4),
		zapsmtp.WithTimeout(20*time.Second),
	)
	_, _ = client.Emails.Get(context.Background(), "019a...",
		zapsmtp.WithRequestTimeout(5*time.Second), zapsmtp.WithRequestMaxRetries(0))
}

func ExampleVerifyWebhook() {
	secret := os.Getenv("ZAP_WEBHOOK_SECRET")
	http.HandleFunc("POST /webhooks/zapsmtp", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		event, err := zapsmtp.VerifyWebhook(body, r.Header.Get("Zap-Signature"), secret)
		if err != nil {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}
		if event.Type == string(zapsmtp.WebhookEventTypeEmailBounced) {
			log.Println("bounced:", event.Data["to"])
		}
		w.WriteHeader(http.StatusOK)
	})
}

func ExampleVerifyWebhookEvents() {
	secret := os.Getenv("ZAP_WEBHOOK_SECRET")
	http.HandleFunc("POST /webhooks/zapsmtp-batch", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// a batch, or one event: either way a list
		events, err := zapsmtp.VerifyWebhookEvents(body, r.Header.Get("Zap-Signature"), secret)
		if err != nil {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}
		for _, event := range events {
			log.Println(event.Type, event.ID)
		}
		w.WriteHeader(http.StatusOK)
	})
}
