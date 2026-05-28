-- Phase-5 schema addition: usage events for metering.

CREATE TABLE IF NOT EXISTS stripe_connector_usage_events (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id   TEXT         NOT NULL,
    metric       TEXT         NOT NULL,
    quantity     BIGINT       NOT NULL CHECK (quantity > 0),
    occurred_at  TIMESTAMPTZ  NOT NULL,
    request_id   TEXT         NOT NULL,
    metadata     JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (subject_id, metric, request_id)
);

CREATE INDEX IF NOT EXISTS idx_usage_subject_metric_time
    ON stripe_connector_usage_events (subject_id, metric, occurred_at);

CREATE INDEX IF NOT EXISTS idx_usage_metric_time
    ON stripe_connector_usage_events (metric, occurred_at);
