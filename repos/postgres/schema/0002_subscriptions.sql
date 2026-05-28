-- Phase-1 schema addition: subscription mirror.
--
-- Items are denormalized into a JSONB column rather than split into
-- a child table — the hot-path queries (HasActivePrice, ListByPriceKey)
-- need the parent row anyway, so a separate items table would only
-- add JOINs without benefit. The trade-off: no SQL-level uniqueness
-- on (subscription, price) per item; the lib doesn't need it.

CREATE TABLE IF NOT EXISTS stripe_connector_subscriptions (
    stripe_id            TEXT         PRIMARY KEY,
    subject_id           TEXT         NOT NULL,
    stripe_customer_id   TEXT         NOT NULL,
    status               TEXT         NOT NULL,
    current_period_start TIMESTAMPTZ  NULL,
    current_period_end   TIMESTAMPTZ  NULL,
    cancel_at_period_end BOOLEAN      NOT NULL DEFAULT false,
    canceled_at          TIMESTAMPTZ  NULL,
    ended_at             TIMESTAMPTZ  NULL,
    trial_start          TIMESTAMPTZ  NULL,
    trial_end            TIMESTAMPTZ  NULL,
    metadata             JSONB        NOT NULL DEFAULT '{}'::jsonb,
    items                JSONB        NOT NULL DEFAULT '[]'::jsonb,
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    stripe_updated_at    TIMESTAMPTZ  NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_subs_by_subject  ON stripe_connector_subscriptions (subject_id);
CREATE INDEX IF NOT EXISTS idx_subs_by_customer ON stripe_connector_subscriptions (stripe_customer_id);
CREATE INDEX IF NOT EXISTS idx_subs_active      ON stripe_connector_subscriptions (subject_id, status)
    WHERE status IN ('active', 'trialing', 'past_due');
