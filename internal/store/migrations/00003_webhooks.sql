-- Webhook subscriptions (docs/09-roadmap.md M6: Webhook 事件投递).
-- The delivery worker advances each subscription's watermark independently;
-- at-least-once semantics with exponential backoff on failure.

-- +goose Up
CREATE TABLE webhook_subscriptions (
    id          text PRIMARY KEY,
    url         text NOT NULL,
    secret_encrypted bytea NOT NULL,
    types       jsonb NOT NULL DEFAULT '[]'::jsonb,
    resource_id text,
    enabled     boolean NOT NULL DEFAULT true,
    watermark   bigint NOT NULL DEFAULT 0,
    fail_count  int NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE webhook_subscriptions;
