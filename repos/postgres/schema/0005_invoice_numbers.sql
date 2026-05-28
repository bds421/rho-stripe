-- Phase-6 schema addition: invoice number issuance ledger for apps
-- that own their invoice numbering (Austrian §11 UStG and similar).
--
-- The "issued" row is created by InvoiceNumberRepo.Next BEFORE the
-- Stripe call so that a crashed process never reuses a number on
-- retry. The state column transitions issued → used | voided once
-- the Stripe call returns.

CREATE TABLE IF NOT EXISTS stripe_connector_invoice_numbers (
    number             TEXT         PRIMARY KEY,
    prefix             TEXT         NOT NULL,
    counter            BIGINT       NOT NULL,
    state              TEXT         NOT NULL CHECK (state IN ('issued', 'used', 'voided')),
    stripe_invoice_id  TEXT         NULL,
    void_reason        TEXT         NULL,
    issued_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    resolved_at        TIMESTAMPTZ  NULL,
    UNIQUE (prefix, counter)
);

CREATE INDEX IF NOT EXISTS idx_invoice_numbers_state
    ON stripe_connector_invoice_numbers (state);

CREATE INDEX IF NOT EXISTS idx_invoice_numbers_stripe_id
    ON stripe_connector_invoice_numbers (stripe_invoice_id)
    WHERE stripe_invoice_id IS NOT NULL;
