package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// checkWebhookRetargetCoverage grades the retargeting rule of section 11.2
// and the replay coverage of section 11.5: a token whose grants do not cover
// a pending delivery's type may neither move the webhook's URL nor replay
// that delivery, while it may still narrow the filter and replay what it can
// read. The webhook is paused throughout, so every answer is about
// authorization and nothing is delivered.
func checkWebhookRetargetCoverage(ctx context.Context, env Env) error {
	receiver, err := env.startHookReceiver(http.StatusNoContent)
	if err != nil {
		return err
	}
	defer receiver.close()

	plugin, err := env.newFakePlugin(ctx, "conformance:webhook-retarget", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	serverID := plugin.Server.Server.ID
	publicType := uniqueWebhookEventType()
	privateType := "conformance-retarget-private." + publicType[len("conformance-webhook."):]

	registered, err := env.registerWebhook(ctx, map[string]any{
		"url": receiver.url, "events": []string{"*"}, "serverIds": []string{serverID},
	})
	if err != nil {
		return err
	}
	webhookID := registered.Webhook.ID
	defer env.deleteWebhook(context.WithoutCancel(ctx), webhookID)
	path := "/api/v1/webhooks/" + webhookID

	if err := env.expect(ctx, http.MethodPatch, path, env.AdminToken,
		map[string]any{"paused": true}, http.StatusOK, nil); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if _, err := plugin.sendEvents(ctx,
		map[string]any{"t": privateType, "data": map[string]any{"secret": true}},
		map[string]any{"t": publicType, "data": map[string]any{}}); err != nil {
		return err
	}
	private, err := env.awaitDelivery(ctx, webhookID, 15*time.Second, "the private delivery pending",
		func(delivery deliveryRecord) bool { return delivery.Type == privateType && delivery.State == "pending" })
	if err != nil {
		return err
	}
	public, err := env.awaitDelivery(ctx, webhookID, 15*time.Second, "the public delivery pending",
		func(delivery deliveryRecord) bool { return delivery.Type == publicType && delivery.State == "pending" })
	if err != nil {
		return err
	}

	narrow, err := env.mintToken(ctx, "conformance: narrow webhook editor",
		"webhooks:manage", "servers:read", "events:read:conformance-webhook.*")
	if err != nil {
		return err
	}

	// Narrowing the filter is a subscription the token could have registered,
	// and nothing pending moves.
	if err := env.expect(ctx, http.MethodPatch, path, narrow.Secret,
		map[string]any{"events": []string{"conformance-webhook.*"}}, http.StatusOK, nil); err != nil {
		return fmt.Errorf("a narrowing edit within the token's grants was refused: %w", err)
	}
	// An edit that repeats the current URL is not a retarget; a client that
	// sends every field on every edit must not be refused for it.
	if err := env.expect(ctx, http.MethodPatch, path, narrow.Secret,
		map[string]any{"events": []string{"conformance-webhook.*"}, "url": receiver.url}, http.StatusOK, nil); err != nil {
		return fmt.Errorf("an edit repeating the current URL was treated as a retarget: %w (section 11.2)", err)
	}
	// Moving the URL would carry the private delivery along.
	if err := env.expectError(ctx, http.MethodPatch, path, narrow.Secret,
		map[string]any{"url": receiver.url + "/elsewhere"}, http.StatusForbidden, "forbidden"); err != nil {
		return fmt.Errorf("a URL change with a pending delivery outside the token's grants was not refused: %w (section 11.2)", err)
	}
	// So would sending it again by hand.
	if err := env.expectError(ctx, http.MethodPost, path+"/deliveries/"+private.ID+"/replay", narrow.Secret,
		nil, http.StatusForbidden, "forbidden"); err != nil {
		return fmt.Errorf("a replay of a delivery outside the token's grants was not refused: %w (section 11.5)", err)
	}
	// What the token may read, it may replay.
	if err := env.expect(ctx, http.MethodPost, path+"/deliveries/"+public.ID+"/replay", narrow.Secret,
		nil, http.StatusAccepted, nil); err != nil {
		return fmt.Errorf("a replay of a delivery within the token's grants was refused: %w", err)
	}
	// The suite's own credential covers everything and may retarget.
	if err := env.expect(ctx, http.MethodPatch, path, env.AdminToken,
		map[string]any{"url": receiver.url + "/elsewhere"}, http.StatusOK, nil); err != nil {
		return fmt.Errorf("a URL change by a credential covering every pending delivery was refused: %w", err)
	}

	// Paused throughout: the acceptances above queued and the refusals
	// refused, and nothing reached the receiver either way.
	time.Sleep(2 * time.Second)
	if receiver.count() != 0 {
		return fmt.Errorf("the receiver saw %d deliveries from a webhook paused for the whole check (section 11.2)", receiver.count())
	}
	return nil
}
