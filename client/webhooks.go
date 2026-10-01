package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Every webhook route requires webhooks:manage, and registration and edits
// also need grants covering what the filter subscribes to (spec section
// 11.2).

// ListWebhooks reads every registered webhook, newest first.
func (c *Client) ListWebhooks(ctx context.Context) (WebhookList, error) {
	var list WebhookList
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "webhooks"}, nil, nil, &list)
	return list, err
}

// CreateWebhook registers a webhook. The signing secret in the answer is
// never available again. A webhook observes only what lands after it is
// registered.
func (c *Client) CreateWebhook(ctx context.Context, request CreateWebhookRequest) (CreatedWebhook, error) {
	var created CreatedWebhook
	err := c.do(ctx, http.MethodPost, []string{"api", "v1", "webhooks"}, nil, request, &created)
	return created, err
}

// UpdateWebhook edits a registration in place, or pauses or resumes it, and
// returns the registration as it stands after the edit. A request with no
// member set is the hub's bad_request. The secret is never rotated.
func (c *Client) UpdateWebhook(ctx context.Context, webhookID string, request UpdateWebhookRequest) (Webhook, error) {
	// A pointer to a nil slice would travel as null; the caller meant the
	// empty list. The pointer is replaced on this copy of the request, so
	// the caller's slice is left alone.
	for _, list := range []**[]string{&request.Events, &request.ServerIDs, &request.Redact} {
		if *list != nil && **list == nil {
			empty := []string{}
			*list = &empty
		}
	}
	var answer struct {
		Webhook Webhook `json:"webhook"`
	}
	err := c.do(ctx, http.MethodPatch, []string{"api", "v1", "webhooks", webhookID}, nil, request, &answer)
	return answer.Webhook, err
}

// DeleteWebhook deletes a webhook. Its pending deliveries are abandoned.
func (c *Client) DeleteWebhook(ctx context.Context, webhookID string) error {
	return c.do(ctx, http.MethodDelete, []string{"api", "v1", "webhooks", webhookID}, nil, nil, nil)
}

// WebhookDeliveries reads a webhook's most recent deliveries, newest first:
// pending, delivered, and dead alike. limit zero means the hub's default;
// the hub clamps a large one.
func (c *Client) WebhookDeliveries(ctx context.Context, webhookID string, limit int) (DeliveryList, error) {
	var query url.Values
	if limit > 0 {
		query = url.Values{"limit": {strconv.Itoa(limit)}}
	}
	var list DeliveryList
	err := c.do(ctx, http.MethodGet, []string{"api", "v1", "webhooks", webhookID, "deliveries"}, query, nil, &list)
	return list, err
}

// ReplayDelivery re-arms one delivery for a further attempt, now, and
// returns it as re-armed. Its id and body are unchanged, so a receiver that
// deduplicates on the id discards a replay of something it already has.
func (c *Client) ReplayDelivery(ctx context.Context, webhookID, deliveryID string) (Delivery, error) {
	var answer struct {
		Delivery Delivery `json:"delivery"`
	}
	err := c.do(ctx, http.MethodPost,
		[]string{"api", "v1", "webhooks", webhookID, "deliveries", deliveryID, "replay"}, nil, nil, &answer)
	return answer.Delivery, err
}
