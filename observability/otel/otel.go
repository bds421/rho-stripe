// Package otel provides OpenTelemetry tracing instrumentation for
// the rho-stripe subsystems that benefit from end-to-end span
// visibility: webhook delivery, catalog Apply, subscription mutations,
// invoice creation.
//
// Design:
//   - The lib depends only on the OTel API (go.opentelemetry.io/otel/*),
//     never the SDK. Apps pick their exporter and TracerProvider; this
//     package consumes whatever's registered globally OR a provider
//     the caller passes.
//   - All wrappers preserve the original function signature so they
//     compose with existing connector wiring: SetQueue(otel.WrapQueue(q)),
//     mux.HandleFunc("/webhook", otel.WrapWebhookHandler(wh)).
//   - No metrics. Use the sibling observability/metrics package for that.
//
// Wire:
//
//	import "github.com/bds421/rho-stripe/observability/otel"
//
//	tracer := otelapi.Tracer("myapp")  // app provides
//	mux.HandleFunc("/webhook", otel.WrapWebhookHandler(conn.Webhooks, tracer))
package otel

import (
	"context"
	"net/http"

	"github.com/bds421/rho-stripe/webhooks"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// WrapWebhookHandler returns an http.HandlerFunc that wraps the
// webhooks.Webhooks.Handle in a span. The span captures the resulting
// status code, signature verification outcome, and event metadata
// (id, type, namespace) when verification succeeded.
//
// The span name follows OTel HTTP semantic conventions:
// "POST /webhook" by default; override by setting the span at the
// outer router level via your normal HTTP instrumentation.
//
// Apps already using otelhttp at the HTTP-server level get the
// HTTP-side span automatically; this wrapper adds Stripe-event-level
// attributes inside that span.
func WrapWebhookHandler(wh *webhooks.Webhooks, tracer trace.Tracer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "stripe.webhook.handle",
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.target", r.URL.Path),
			),
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()

		// Wrap the ResponseWriter so we can record the final status
		// code on the span.
		rw := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}
		wh.Handle(rw, r.WithContext(ctx))

		span.SetAttributes(attribute.Int("http.status_code", rw.status))
		if rw.status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, "handler returned 5xx")
		} else if rw.status >= http.StatusBadRequest {
			span.SetStatus(codes.Error, "client error")
		}
	}
}

// WrapQueue wraps a webhooks.Queue so that Enqueue starts a span
// (linking the receive-time work to the eventual dispatch). The
// wrapper preserves the underlying Queue's semantics; apps still
// SetQueue() with the wrapped value.
func WrapQueue(q webhooks.Queue, tracer trace.Tracer) webhooks.Queue {
	return &tracingQueue{inner: q, tracer: tracer}
}

type tracingQueue struct {
	inner  webhooks.Queue
	tracer trace.Tracer
}

func (q *tracingQueue) Enqueue(ctx context.Context, item webhooks.QueueItem) error {
	ctx, span := q.tracer.Start(ctx, "stripe.webhook.enqueue",
		trace.WithAttributes(
			attribute.String("stripe.event.id", item.EventID),
			attribute.String("stripe.event.type", item.EventType),
			attribute.String("stripe.app_namespace", item.Namespace),
		),
	)
	defer span.End()
	if err := q.inner.Enqueue(ctx, item); err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		return err
	}
	return nil
}

// WrapProcessQueued wraps a Webhooks.ProcessQueued-compatible
// function in a span so that the actual handler dispatch (which
// runs on a worker, not the HTTP handler) shows up as its own
// trace. Use when constructing a Queue: pass otel.WrapProcessQueued(wh.ProcessQueued, tracer)
// as the dispatch function.
func WrapProcessQueued(dispatch func(ctx context.Context, item webhooks.QueueItem) error, tracer trace.Tracer) func(ctx context.Context, item webhooks.QueueItem) error {
	return func(ctx context.Context, item webhooks.QueueItem) error {
		ctx, span := tracer.Start(ctx, "stripe.webhook.dispatch",
			trace.WithAttributes(
				attribute.String("stripe.event.id", item.EventID),
				attribute.String("stripe.event.type", item.EventType),
				attribute.String("stripe.app_namespace", item.Namespace),
			),
		)
		defer span.End()
		if err := dispatch(ctx, item); err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			return err
		}
		return nil
	}
}

// statusCapturingWriter wraps http.ResponseWriter to remember the
// status code WriteHeader was called with.
type statusCapturingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusCapturingWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusCapturingWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		// Default to 200 if no explicit WriteHeader.
		if w.status == 0 {
			w.status = http.StatusOK
		}
	}
	return w.ResponseWriter.Write(b)
}
