-- stripe-connector reference Postgres schema, phase 0.
--
-- Apps integrating the lib's repos/postgres adapter apply this with
-- their preferred migration tool (goose, atlas, golang-migrate, raw
-- psql) — the adapter does not run migrations at runtime.
--
-- Tables:
--   stripe_connector_customers       — checkout.CustomerRepo backing
--   stripe_connector_credit_grants   — credits.CreditRepo backing
--
-- The idempotency.Store (webhook dedup) is rho-kit's pgstore. Its
-- backing table is included below for one-pass schema application;
-- kept in sync with rho-kit/data/idempotency/pgstore/migrations.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    key           VARCHAR(512) PRIMARY KEY,
    status_code   INT,
    headers       JSONB,
    response_body BYTEA,
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    owner_token   VARCHAR(64),
    fingerprint   BYTEA
);
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_expires_at ON idempotency_keys (expires_at);

CREATE TABLE IF NOT EXISTS stripe_connector_customers (
    subject_id          TEXT         PRIMARY KEY,
    stripe_customer_id  TEXT         NOT NULL UNIQUE,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS stripe_connector_credit_grants (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id        TEXT         NOT NULL,
    bucket            TEXT         NOT NULL,
    amount_initial    BIGINT       NOT NULL CHECK (amount_initial > 0),
    amount_remaining  BIGINT       NOT NULL CHECK (amount_remaining >= 0),
    granted_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ  NULL,
    source            TEXT         NOT NULL,
    source_ref        TEXT         NULL,
    metadata          JSONB        NOT NULL DEFAULT '{}'::jsonb,
    revoked_at        TIMESTAMPTZ  NULL,
    revoked_reason    TEXT         NULL
);

-- Webhook auto-grant idempotency: same Stripe payment intent +
-- subject + bucket must not produce duplicate ledger rows on
-- redelivery. Enforced via partial unique index so multiple
-- non-Stripe grants (admin grants without source_ref) coexist.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_grants_source
    ON stripe_connector_credit_grants (subject_id, bucket, source_ref)
    WHERE source_ref IS NOT NULL;

-- Hot path: listing a subject's grants ordered for FIFO-by-expiry
-- deduction (phase 3 will exercise this).
CREATE INDEX IF NOT EXISTS idx_credit_grants_active_lookup
    ON stripe_connector_credit_grants (subject_id, bucket, expires_at NULLS LAST, granted_at)
    WHERE amount_remaining > 0 AND revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_credit_grants_expiry
    ON stripe_connector_credit_grants (expires_at)
    WHERE amount_remaining > 0 AND revoked_at IS NULL AND expires_at IS NOT NULL;
