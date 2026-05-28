-- Phase-3 schema addition: credit deduction ledger.

CREATE TABLE IF NOT EXISTS stripe_connector_credit_deductions (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id   TEXT         NOT NULL,
    bucket       TEXT         NOT NULL,
    grant_id     UUID         NOT NULL REFERENCES stripe_connector_credit_grants(id),
    amount       BIGINT       NOT NULL CHECK (amount > 0),
    reason       TEXT         NOT NULL,
    request_id   TEXT         NOT NULL,
    metadata     JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- One logical TryDeduct may span multiple grants (FIFO across
    -- expiry tiers). The dedup key includes grant_id so a single
    -- request_id can produce N rows (one per drained grant). Overall
    -- request idempotency is enforced by the in-app pre-check in
    -- TryDeduct (a SELECT WHERE subject_id+request_id short-circuits
    -- before re-applying).
    UNIQUE (subject_id, request_id, grant_id)
);

CREATE INDEX IF NOT EXISTS idx_deductions_subject_bucket_time
    ON stripe_connector_credit_deductions (subject_id, bucket, created_at);

CREATE INDEX IF NOT EXISTS idx_deductions_grant
    ON stripe_connector_credit_deductions (grant_id);
