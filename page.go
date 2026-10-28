package zapsmtp

import (
	"context"
	"encoding/json"
	"iter"
	"maps"
)

// Page is one page of a list.
//
// To walk every item of every page, use the method's AutoPaging variant
// (client.Emails.ListAutoPaging and the like), or call GetNextPage yourself.
type Page[T any] struct {
	// Object is "list".
	Object string `json:"object"`
	// Data holds this page's items.
	Data []T `json:"data"`
	// HasMore is true when more items follow this page.
	HasMore bool `json:"has_more"`
	// NextCursor is the cursor of the next page, for lists paged by cursor (suppressions).
	NextCursor string `json:"next_cursor,omitempty"`
	// Raw is the page as the API sent it.
	Raw json.RawMessage `json:"-"`

	next func(context.Context) (*Page[T], error)
}

// HasNextPage reports whether there is a next page to ask for. It makes no request.
func (p *Page[T]) HasNextPage() bool { return p.next != nil }

// GetNextPage asks for the next page, with the same parameters and options as
// this one. It returns nil and no error after the last page.
func (p *Page[T]) GetNextPage(ctx context.Context) (*Page[T], error) {
	if p.next == nil {
		return nil, nil
	}
	return p.next(ctx)
}

func list[T any](ctx context.Context, c *core, req *request, opts []RequestOption) (*Page[T], error) {
	data, err := c.do(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	p := new(Page[T])
	decodeLenient(data, p)
	p.Raw = data
	if p.HasMore {
		if next := nextPage(req, p); next != nil {
			p.next = func(ctx context.Context) (*Page[T], error) { return list[T](ctx, c, next, opts) }
		}
	}
	return p, nil
}

// pageItem is what paging reads of an item.
type pageItem struct {
	ID            string `json:"id"`
	LastMessageAt string `json:"last_message_at"`
}

func (i *pageItem) UnmarshalJSON(data []byte) error {
	type plain pageItem
	decodeLenient(data, (*plain)(i))
	return nil
}

// nextPage is the request of the page after p, or nil when there is none to
// ask for. It keeps the caller's parameters.
func nextPage[T any](req *request, p *Page[T]) *request {
	var items struct {
		Data []pageItem `json:"data"`
	}
	decodeLenient(p.Raw, &items)
	var first, last pageItem
	if n := len(items.Data); n > 0 {
		first, last = items.Data[0], items.Data[n-1]
	}
	set := map[string]string{}
	switch req.paging {
	case pagingCursor:
		set["after"] = p.NextCursor
	case pagingBefore:
		set["before"] = last.ID
	case pagingAfter:
		set["after"] = last.ID
	case pagingLogs:
		// with after, logs come oldest-first from the server and newest-first
		// in the page: the next newer page starts after the page's newest entry
		if req.query.Has("after") {
			set["after"] = first.ID
		} else {
			set["before"] = last.ID
		}
	case pagingThread:
		if last.LastMessageAt == "" {
			return nil
		}
		set["before"], set["before_id"] = last.LastMessageAt, last.ID
	default:
		return nil
	}
	next := *req
	next.query = maps.Clone(req.query)
	if next.query == nil {
		next.query = map[string][]string{}
	}
	for k, v := range set {
		if v == "" {
			return nil
		}
		next.query.Set(k, v)
	}
	return &next
}

// autoPaging walks every item of every page, asking for each page as the loop
// reaches it. A cancelled context ends it with the context's error, even
// between the items of a page already loaded.
func autoPaging[T any](ctx context.Context, first func() (*Page[T], error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		p, err := first()
		for {
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, item := range p.Data {
				if err := ctx.Err(); err != nil { // cancelled: stop between items too
					var zero T
					yield(zero, err)
					return
				}
				if !yield(item, nil) {
					return
				}
			}
			if p, err = p.GetNextPage(ctx); p == nil && err == nil {
				return
			}
		}
	}
}
