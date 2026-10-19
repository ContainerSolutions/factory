package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/provider"
)

// Webhook ingress (github-provider.md §Ingress and normalisation).
const (
	// MaxWebhookBytes bounds a webhook body, like the API server's body limit (1 MB).
	MaxWebhookBytes = 1 << 20
	// DeliveryPrefix starts every delivery ID: "gh-<X-GitHub-Delivery>".
	DeliveryPrefix = "gh-"

	headerSignature = "X-Hub-Signature-256"
	headerDelivery  = "X-GitHub-Delivery"
	headerEvent     = "X-GitHub-Event"
	signaturePrefix = "sha256="

	// maxRecoverPages bounds the deliveries Recover lists (100 per page).
	maxRecoverPages = 50
)

// deliveryID is what a GitHub delivery GUID (and an event name) may look like; anything else
// can't become a store key.
var (
	deliveryID = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)
	eventName  = regexp.MustCompile(`^[a-z_]{1,64}$`)
)

// droppedHeaders are never stored with a delivery.
var droppedHeaders = []string{"Authorization", "Cookie", "Proxy-Authorization"}

// Handler implements provider.Conn: POST /hooks/<provider>. It verifies X-Hub-Signature-256
// (HMAC-SHA256 of the body with the webhook secret, compared in constant time), then calls
// sink.Accept with the delivery and answers only once Accept has returned: 202 when it stored
// the delivery, 200 for a duplicate, 503 for any other error (GitHub then redelivers). A bad
// or missing signature is 401; a request that isn't a POST 405, a body over MaxWebhookBytes
// 413, a delivery without a delivery ID or event name 400. Bodies are never logged.
func (c *Conn) Handler(sink provider.IngressSink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		if !c.validSignature(r.Header.Get(headerSignature), body) {
			c.log.WarnContext(r.Context(), "webhook signature rejected", "delivery", safeHeader(r.Header.Get(headerDelivery)))
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		id, event := r.Header.Get(headerDelivery), r.Header.Get(headerEvent)
		if !deliveryID.MatchString(id) || !eventName.MatchString(event) {
			http.Error(w, "missing or malformed X-GitHub-Delivery or X-GitHub-Event", http.StatusBadRequest)
			return
		}
		if sink == nil {
			http.Error(w, "delivery not stored", http.StatusServiceUnavailable)
			return
		}
		headers := r.Header.Clone()
		for _, h := range droppedHeaders {
			headers.Del(h)
		}
		ev := provider.RawEvent{
			ID:         DeliveryPrefix + id,
			Provider:   c.name,
			Type:       event,
			Headers:    headers,
			Body:       body,
			ReceivedAt: c.clock.Now(),
		}
		switch err := sink.Accept(r.Context(), ev); {
		case err == nil:
			c.log.DebugContext(r.Context(), "webhook accepted", "delivery", ev.ID, "event", event)
			w.WriteHeader(http.StatusAccepted)
		case errors.Is(err, provider.ErrDuplicateDelivery):
			c.log.DebugContext(r.Context(), "webhook duplicate", "delivery", ev.ID, "event", event)
			w.WriteHeader(http.StatusOK)
		default:
			c.log.WarnContext(r.Context(), "webhook not stored", "delivery", ev.ID, "event", event, "err", err)
			http.Error(w, "delivery not stored", http.StatusServiceUnavailable)
		}
	})
}

// validSignature checks header ("sha256=<hex>") against the body's HMAC-SHA256 under the
// webhook secret, in constant time.
func (c *Conn) validSignature(header string, body []byte) bool {
	hexSig, ok := strings.CutPrefix(header, signaturePrefix)
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, c.webhookSecret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// safeHeader is a header value fit for a log line: a delivery ID, or a placeholder.
func safeHeader(v string) string {
	if deliveryID.MatchString(v) {
		return v
	}
	return "(malformed)"
}

// hookDelivery is an entry of GET /app/hook/deliveries.
type hookDelivery struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	Event       string    `json:"event"`

	identity string
}

// Recover implements provider.Conn (the restart gap): it lists each App's webhook deliveries
// (GET /app/hook/deliveries, newest first, which GitHub keeps for a few days) back to since,
// fetches each one's request (GET /app/hook/deliveries/{id}) and sends it to sink.Accept,
// oldest first, once per GUID (redeliveries share it; the first delivery is used). A delivery the sink already has comes
// back as ErrDuplicateDelivery and is skipped; any other Accept error stops Recover. An App
// without a webhook (404) has nothing to recover.
//
// A recovered delivery has the headers and payload GitHub recorded and ReceivedAt set to when
// GitHub delivered it. Its body is GitHub's re-encoding of the payload, so it carries no
// signature to verify; the App JWT authenticated the listing instead.
func (c *Conn) Recover(ctx context.Context, since time.Time, sink provider.IngressSink) error {
	if sink == nil {
		return fmt.Errorf("github %s: recover: no sink: %w", c.name, provider.ErrInvalid)
	}
	var listed []hookDelivery
	for _, id := range c.minter.Identities() {
		ds, err := c.listDeliveries(ctx, id, since)
		c.record(id, err)
		if errors.Is(err, provider.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("github %s: recover: list %s deliveries: %w", c.name, id, err)
		}
		listed = append(listed, ds...)
	}
	// Oldest first, and each GUID once: its first delivery, not a redelivery.
	slices.SortStableFunc(listed, func(a, b hookDelivery) int { return a.DeliveredAt.Compare(b.DeliveredAt) })
	seen := map[string]bool{}
	var todo []hookDelivery
	for _, d := range listed {
		if !seen[d.GUID] {
			seen[d.GUID] = true
			todo = append(todo, d)
		}
	}
	accepted := 0
	for _, d := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, ok, err := c.fetchDelivery(ctx, d)
		if err != nil {
			return fmt.Errorf("github %s: recover: %w", c.name, err)
		}
		if !ok {
			continue
		}
		switch err := sink.Accept(ctx, ev); {
		case err == nil:
			accepted++
		case errors.Is(err, provider.ErrDuplicateDelivery):
		default:
			return fmt.Errorf("github %s: recover %s: %w", c.name, ev.ID, err)
		}
	}
	c.log.InfoContext(ctx, "webhook deliveries recovered", "since", since, "listed", len(todo), "accepted", accepted)
	return nil
}

// listDeliveries lists the App's deliveries delivered at or after since.
func (c *Conn) listDeliveries(ctx context.Context, identity string, since time.Time) ([]hookDelivery, error) {
	q := url.Values{"per_page": {strconv.Itoa(perPage)}}
	var out []hookDelivery
	for range maxRecoverPages {
		var page []hookDelivery
		h, err := c.minter.AppGet(ctx, identity, "app/hook/deliveries", q, &page)
		if err != nil {
			return nil, err
		}
		older := false
		for _, d := range page {
			if d.DeliveredAt.Before(since) {
				older = true
				continue
			}
			if !deliveryID.MatchString(d.GUID) {
				c.log.WarnContext(ctx, "recover: skipped a delivery with a malformed GUID", "id", d.ID)
				continue
			}
			d.identity = identity
			out = append(out, d)
		}
		next, ok := nextQuery(h)
		if older || !ok || next.Get("cursor") == "" {
			return out, nil
		}
		q.Set("cursor", next.Get("cursor"))
	}
	c.log.WarnContext(ctx, "recover: delivery listing truncated", "identity", identity, "pages", maxRecoverPages)
	return out, nil
}

// fetchDelivery reads one delivery's request; ok is false for one GitHub kept no payload of.
func (c *Conn) fetchDelivery(ctx context.Context, d hookDelivery) (provider.RawEvent, bool, error) {
	var detail struct {
		Request struct {
			Headers map[string]string `json:"headers"`
			Payload json.RawMessage   `json:"payload"`
		} `json:"request"`
	}
	if _, err := c.minter.AppGet(ctx, d.identity, "app/hook/deliveries/"+strconv.FormatInt(d.ID, 10), nil, &detail); err != nil {
		return provider.RawEvent{}, false, fmt.Errorf("get delivery %s: %w", d.GUID, err)
	}
	payload := bytes.TrimSpace(detail.Request.Payload)
	if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
		c.log.WarnContext(ctx, "recover: delivery has no payload", "delivery", DeliveryPrefix+d.GUID)
		return provider.RawEvent{}, false, nil
	}
	headers := http.Header{}
	for k, v := range detail.Request.Headers {
		headers.Set(k, v)
	}
	for _, h := range droppedHeaders {
		headers.Del(h)
	}
	event := d.Event
	if event == "" {
		event = headers.Get(headerEvent)
	}
	return provider.RawEvent{
		ID:         DeliveryPrefix + d.GUID,
		Provider:   c.name,
		Type:       event,
		Headers:    headers,
		Body:       slices.Clone(payload),
		ReceivedAt: d.DeliveredAt.UTC(),
	}, true, nil
}
