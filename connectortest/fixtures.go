package connectortest

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/credits"
	"github.com/bds421/rho-stripe/webhooks"
	stripe "github.com/stripe/stripe-go/v82"
)

// EventFixtures is a small library of pre-built event payloads for
// the most common Stripe events. Each function returns a webhooks.Event
// with realistic fields (correct api_version, app_namespace metadata
// stamp, subject id, etc.) so apps can post them at their own
// webhook.Handle via NewSigner without involving real Stripe.
//
// All fixtures set metadata.app_namespace to match the namespace
// argument; tests must use the same namespace in webhooks.Config or
// the namespace filter will silently drop the event.
var EventFixtures eventFixtures

type eventFixtures struct{}

// CheckoutCompleted builds a checkout.session.completed event for
// the given namespace/subject.
func (eventFixtures) CheckoutCompleted(namespace, subject, sessionID string) webhooks.Event {
	body := map[string]any{
		"id":             sessionID,
		"object":         "checkout.session",
		"payment_intent": "pi_" + sessionID,
		"metadata": map[string]string{
			"app_namespace": namespace,
			"subject_id":    subject,
		},
	}
	return wrapEvent(sessionID, "checkout.session.completed", body)
}

// CheckoutCompletedWithCredits builds a checkout.session.completed
// event whose metadata already carries the encoded credit_grants list.
// Useful for testing the credit-grant auto-handler in isolation.
func (eventFixtures) CheckoutCompletedWithCredits(namespace, subject, sessionID string, grants []credits.PendingGrant) (webhooks.Event, error) {
	encoded, err := credits.EncodeSessionMetadata(grants)
	if err != nil {
		return webhooks.Event{}, err
	}
	body := map[string]any{
		"id":             sessionID,
		"object":         "checkout.session",
		"payment_intent": "pi_" + sessionID,
		"metadata": map[string]string{
			"app_namespace": namespace,
			"subject_id":    subject,
			"credit_grants": encoded,
		},
	}
	return wrapEvent(sessionID, "checkout.session.completed", body), nil
}

// InvoicePaid builds an invoice.paid event.
func (eventFixtures) InvoicePaid(namespace, subject, invoiceID string, amountPaid int64) webhooks.Event {
	body := map[string]any{
		"id":          invoiceID,
		"object":      "invoice",
		"amount_paid": amountPaid,
		"metadata": map[string]string{
			"app_namespace": namespace,
			"subject_id":    subject,
		},
	}
	return wrapEvent(invoiceID, "invoice.paid", body)
}

// SubscriptionCreated builds a customer.subscription.created event.
func (eventFixtures) SubscriptionCreated(namespace, subject, subscriptionID, customerID string) webhooks.Event {
	body := map[string]any{
		"id":       subscriptionID,
		"object":   "subscription",
		"customer": customerID,
		"status":   "active",
		"metadata": map[string]string{
			"app_namespace": namespace,
			"subject_id":    subject,
		},
	}
	return wrapEvent(subscriptionID, "customer.subscription.created", body)
}

// SubscriptionCanceled builds a customer.subscription.deleted event.
func (eventFixtures) SubscriptionCanceled(namespace, subject, subscriptionID string) webhooks.Event {
	body := map[string]any{
		"id":     subscriptionID,
		"object": "subscription",
		"status": "canceled",
		"metadata": map[string]string{
			"app_namespace": namespace,
			"subject_id":    subject,
		},
	}
	return wrapEvent(subscriptionID, "customer.subscription.deleted", body)
}

// PaymentFailed builds a payment_intent.payment_failed event.
func (eventFixtures) PaymentFailed(namespace, subject, paymentIntentID, declineCode string) webhooks.Event {
	body := map[string]any{
		"id":     paymentIntentID,
		"object": "payment_intent",
		"status": "requires_payment_method",
		"last_payment_error": map[string]any{
			"code":         declineCode,
			"decline_code": declineCode,
		},
		"metadata": map[string]string{
			"app_namespace": namespace,
			"subject_id":    subject,
		},
	}
	return wrapEvent(paymentIntentID, "payment_intent.payment_failed", body)
}

// wrapEvent serializes body and packages it into a webhooks.Event
// whose Raw field carries a stripe.Event whose Data.Raw is the
// serialized body. This is the shape downstream code (namespace
// filter, credit-grant auto-handler) consumes.
func wrapEvent(id, eventType string, body map[string]any) webhooks.Event {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(fmt.Sprintf("connectortest: serialize fixture body: %v", err))
	}
	evt := &stripe.Event{
		ID:         "evt_" + id,
		APIVersion: stripe.APIVersion,
		Type:       stripe.EventType(eventType),
		Created:    time.Now().Unix(),
		Livemode:   false,
		Data:       &stripe.EventData{Raw: raw},
	}
	return webhooks.Event{
		ID:          evt.ID,
		Type:        string(evt.Type),
		LiveMode:    evt.Livemode,
		CreatedUnix: evt.Created,
		Raw:         evt,
	}
}

// SignedBody serializes a fixture into the exact bytes a real Stripe
// webhook POST would contain, then signs them with the given secret.
// Apps use this to POST fixtures at their own webhook handler in tests.
//
// Returns the body bytes + Stripe-Signature header value.
func SignedBody(secret string, evt webhooks.Event) (body []byte, signature string, err error) {
	if evt.Raw == nil {
		return nil, "", fmt.Errorf("connectortest: SignedBody: Event.Raw is nil")
	}
	// Build the JSON Stripe would actually send. The webhook handler
	// reads this same shape via webhook.ConstructEventWithOptions.
	envelope := map[string]any{
		"id":          evt.Raw.ID,
		"object":      "event",
		"api_version": evt.Raw.APIVersion,
		"type":        string(evt.Raw.Type),
		"livemode":    evt.Raw.Livemode,
		"created":     evt.Raw.Created,
		"data":        map[string]any{},
	}
	if len(evt.Raw.Data.Raw) > 0 {
		var inner any
		if err := json.Unmarshal(evt.Raw.Data.Raw, &inner); err != nil {
			return nil, "", fmt.Errorf("connectortest: decode inner: %w", err)
		}
		envelope["data"] = map[string]any{"object": inner}
	}
	body, err = json.Marshal(envelope)
	if err != nil {
		return nil, "", fmt.Errorf("connectortest: marshal envelope: %w", err)
	}
	signature = NewSigner(secret).SignNow(body)
	return body, signature, nil
}
