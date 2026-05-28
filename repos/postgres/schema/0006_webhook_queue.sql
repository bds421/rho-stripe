-- Phase-7 schema: durable webhook dispatch queue for multi-process
-- apps. The MemoryQueue is fine for single-process; this table-backed
-- queue survives process restarts and lets multiple workers (in
-- separate processes) compete for items via SELECT ... FOR UPDATE SKIP LOCKED.
--
-- Lifecycle:
--   1. webhooks.Handle calls Queue.Enqueue → INSERT a row with state='queued'.
--   2. A worker calls Dequeue → atomically marks ONE row state='processing'
--      via SELECT ... FOR UPDATE SKIP LOCKED + UPDATE, returns the QueueItem.
--   3. Worker calls Webhooks.ProcessQueued → dispatch + dedup-store update.
--   4. On success: DELETE the row (or UPDATE state='done' if you want
--      audit retention — the impl deletes by default to keep the table small).
--   5. On error: UPDATE state='queued' + increment attempt_count for retry.

CREATE TABLE IF NOT EXISTS stripe_connector_webhook_queue (
    id            BIGSERIAL    PRIMARY KEY,
    event_id      TEXT         NOT NULL UNIQUE,
    event_type    TEXT         NOT NULL,
    namespace     TEXT         NOT NULL DEFAULT '',
    body          BYTEA        NOT NULL,
    token         TEXT         NOT NULL,
    state         TEXT         NOT NULL DEFAULT 'queued'
                  CHECK (state IN ('queued', 'processing')),
    attempt_count INT          NOT NULL DEFAULT 0,
    enqueued_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    locked_at     TIMESTAMPTZ  NULL,
    locked_by     TEXT         NULL
);

-- Worker lookup index: drives the SKIP LOCKED scan.
CREATE INDEX IF NOT EXISTS idx_webhook_queue_state_enqueued
    ON stripe_connector_webhook_queue (state, enqueued_at)
    WHERE state = 'queued';

-- For stuck-job recovery (workers crashed mid-process).
CREATE INDEX IF NOT EXISTS idx_webhook_queue_locked
    ON stripe_connector_webhook_queue (state, locked_at)
    WHERE state = 'processing';
