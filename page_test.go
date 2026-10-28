package zapsmtp

import (
	"errors"
	"net/url"
	"testing"
)

func TestPagingBefore(t *testing.T) {
	f := newFake(t, page(true, `{"id":"a"}`, `{"id":"b"}`), page(false, `{"id":"c"}`))
	var ids []string
	for e, err := range f.client().Emails.ListAutoPaging(ctx, &EmailsListParams{Limit: Int(2), After: String("2026-01-01T00:00:00Z")}) {
		mustNil(t, err)
		ids = append(ids, e.ID)
	}
	if len(ids) != 3 || ids[2] != "c" {
		t.Fatal(ids)
	}
	q := f.calls()[1].query
	if q.Get("before") != "b" || q.Get("limit") != "2" || q.Get("after") != "2026-01-01T00:00:00Z" {
		t.Fatal(q)
	}
}

func TestPagingCursor(t *testing.T) {
	f := newFake(t,
		answer(200, `{"object":"list","has_more":true,"next_cursor":"cur1","data":[{"email":"a"}]}`),
		answer(200, `{"object":"list","has_more":true,"next_cursor":null,"data":[{"email":"b"}]}`))
	var got []string
	for s, err := range f.client().Suppressions.ListAutoPaging(ctx, nil) {
		mustNil(t, err)
		got = append(got, s.Email)
	}
	if len(got) != 2 || len(f.calls()) != 2 || f.calls()[1].query.Encode() != "after=cur1" {
		t.Fatal(got, f.calls()[1].query)
	}
}

func TestPagingAfter(t *testing.T) {
	f := newFake(t, page(true, `{"id":"a1"}`, `{"id":"a2"}`))
	p, err := f.client().Emails.Receiving.Attachments.List(ctx, "m1", &EmailsReceivingAttachmentsListParams{Limit: Int(2)})
	mustNil(t, err)
	_, err = p.GetNextPage(ctx)
	mustNil(t, err)
	if q := f.calls()[1].query; q.Get("after") != "a2" || q.Get("limit") != "2" || f.calls()[1].path != "/v1/emails/receiving/m1/attachments" {
		t.Fatal(q)
	}
	f = newFake(t, page(true))
	p, err = f.client().Emails.Receiving.Attachments.List(ctx, "m1", nil)
	mustNil(t, err)
	if p.HasNextPage() {
		t.Fatal("no item: nothing to ask for")
	}
}

func TestPagingLogs(t *testing.T) {
	f := newFake(t, page(true, `{"id":"l3"}`, `{"id":"l2"}`))
	p, err := f.client().Logs.List(ctx, nil)
	mustNil(t, err)
	_, err = p.GetNextPage(ctx)
	mustNil(t, err)
	if q := f.calls()[1].query; q.Get("before") != "l2" {
		t.Fatal(q)
	}
	f = newFake(t, page(true, `{"id":"l9"}`, `{"id":"l8"}`))
	p, err = f.client().Logs.List(ctx, &LogsListParams{After: String("l7")})
	mustNil(t, err)
	_, err = p.GetNextPage(ctx)
	mustNil(t, err)
	if q := f.calls()[1].query; q.Get("after") != "l9" || q.Has("before") {
		t.Fatal(q)
	}
	for _, params := range []*LogsListParams{{After: String("l7")}, nil} {
		f = newFake(t, page(true))
		p, err = f.client().Logs.List(ctx, params)
		mustNil(t, err)
		if p.HasNextPage() {
			t.Fatal("empty page")
		}
	}
	f = newFake(t, page(true))
	r, err := f.client().Emails.Receiving.List(ctx, nil)
	mustNil(t, err)
	if r.HasNextPage() {
		t.Fatal("empty page")
	}
}

func TestPagingThreads(t *testing.T) {
	last := `{"id":"019aaa00-0000-7000-8000-000000000001","last_message_at":"2026-10-28T08:00:00Z"}`
	f := newFake(t, page(true, last))
	p, err := f.client().Threads.List(ctx, &ThreadsListParams{Limit: Int(1), Unread: Bool(true)})
	mustNil(t, err)
	_, err = p.GetNextPage(ctx)
	mustNil(t, err)
	q := f.calls()[1].query
	if q.Get("before") != "2026-10-28T08:00:00Z" || q.Get("before_id") != "019aaa00-0000-7000-8000-000000000001" || q.Get("unread") != "true" {
		t.Fatal(q)
	}
	f = newFake(t, page(true, `{"id":"x"}`))
	p, err = f.client().Threads.List(ctx, nil)
	mustNil(t, err)
	if p.HasNextPage() {
		t.Fatal("no last_message_at: nothing to ask for")
	}
	f = newFake(t, page(true, `{"last_message_at":"2026-10-28T08:00:00Z"}`))
	p, err = f.client().Threads.List(ctx, nil)
	mustNil(t, err)
	if p.HasNextPage() {
		t.Fatal("no id: nothing to ask for")
	}
}

func TestHasNextPageMakesNoRequest(t *testing.T) {
	f := newFake(t, page(true, `{"id":"email-1"}`))
	p, err := f.client().Emails.List(ctx, nil)
	mustNil(t, err)
	first, second := p.HasNextPage(), p.HasNextPage()
	if !first || !second || len(f.calls()) != 1 {
		t.Fatal(len(f.calls()))
	}
	if p.Object != "list" || len(p.Data) != 1 || p.Data[0].ID != "email-1" {
		t.Fatalf("%+v", p)
	}
}

func TestPagingStops(t *testing.T) {
	f := newFake(t, page(true, `{}`))
	p, err := f.client().Emails.List(ctx, nil)
	mustNil(t, err)
	if next, err := p.GetNextPage(ctx); next != nil || err != nil || p.HasNextPage() {
		t.Fatal("no id")
	}
	f = newFake(t, page(true, `{"id":"a"}`))
	d, err := f.client().Domains.List(ctx)
	mustNil(t, err)
	if next, _ := d.GetNextPage(ctx); next != nil {
		t.Fatal("an unpaged list")
	}
	f = newFake(t, page(false, `{"id":"a"}`))
	p, err = f.client().Emails.List(ctx, nil)
	mustNil(t, err)
	if p.HasNextPage() {
		t.Fatal("the last page")
	}
	f = newFake(t, answer(200, `{"object":"list"}`))
	p, err = f.client().Emails.List(ctx, nil)
	mustNil(t, err)
	if len(p.Data) != 0 || p.HasMore {
		t.Fatalf("%+v", p)
	}
	if nextPage(&request{paging: pagingCursor, query: url.Values{}}, &Page[Email]{Raw: []byte(`{}`)}) != nil {
		t.Fatal("no cursor")
	}
	if r := nextPage(&request{paging: pagingBefore}, &Page[Email]{Raw: []byte(`{"data":[{"id":"z"}]}`)}); r == nil || r.query.Get("before") != "z" {
		t.Fatal("a request without a query")
	}
}

func TestAutoPagingErrorsAndBreak(t *testing.T) {
	f := newFake(t, page(true, `{"id":"a"}`), apiError(404, "not_found"))
	var ids []string
	var last error
	for e, err := range f.client(WithMaxRetries(0)).Emails.ListAutoPaging(ctx, nil) {
		if err != nil {
			last = err
			continue
		}
		ids = append(ids, e.ID)
	}
	if len(ids) != 1 || !IsNotFound(last) {
		t.Fatal(ids, last)
	}
	f = newFake(t, apiError(401, "invalid_api_key"))
	for _, err := range f.client().Emails.ListAutoPaging(ctx, nil) {
		if !errors.Is(err, ErrAuthentication) {
			t.Fatal(err)
		}
	}
	f = newFake(t, page(true, `{"id":"a"}`, `{"id":"b"}`))
	for range f.client().Emails.ListAutoPaging(ctx, nil) {
		break
	}
	if len(f.calls()) != 1 {
		t.Fatal("stopping the loop stops the paging")
	}
}
