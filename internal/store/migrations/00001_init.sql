-- Initial schema (docs/08-data-model.md).
-- PostgreSQL is the authoritative store; the table queue lives in the same
-- database so dequeue / state transition / event write share one transaction.

-- +goose Up
CREATE TABLE credentials (
    id               text PRIMARY KEY,
    name             text NOT NULL,
    type             text NOT NULL CHECK (type IN ('bmc', 'ssh')),
    secret_encrypted bytea NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_credentials_name ON credentials (name);

CREATE TABLE machines (
    id                text PRIMARY KEY,
    labels            jsonb NOT NULL DEFAULT '{}'::jsonb,
    bmc_address       text NOT NULL,
    bmc_protocol      text NOT NULL DEFAULT 'auto' CHECK (bmc_protocol IN ('redfish', 'ipmi', 'auto', 'fake')),
    bmc_credential_id text NOT NULL REFERENCES credentials (id),
    ssh_credential_id text REFERENCES credentials (id),
    vendor            text,
    model             text,
    serial_number     text,
    firmware_version  text,
    hardware          jsonb,
    power_state       text NOT NULL DEFAULT 'unknown' CHECK (power_state IN ('on', 'off', 'unknown')),
    state             text NOT NULL DEFAULT 'registering' CHECK (state IN ('registering', 'discovering', 'ready', 'error')),
    last_error        jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_machines_bmc_address ON machines (bmc_address);
CREATE INDEX idx_machines_state ON machines (state);
CREATE INDEX idx_machines_labels ON machines USING gin (labels);

CREATE TABLE layout_snapshots (
    id          bigserial PRIMARY KEY,
    machine_id  text NOT NULL REFERENCES machines (id),
    captured_at timestamptz NOT NULL DEFAULT now(),
    source      text NOT NULL CHECK (source IN ('inband_ssh', 'ramdisk')),
    content     jsonb NOT NULL
);
CREATE INDEX idx_layout_snapshots_machine ON layout_snapshots (machine_id, captured_at);

CREATE TABLE jobs (
    id              text PRIMARY KEY,
    type            text NOT NULL CHECK (type IN ('install', 'power', 'discover')),
    request         jsonb NOT NULL,
    action          jsonb,
    spec_resolved   jsonb,
    policy          jsonb NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key text,
    state           text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'succeeded', 'partial', 'failed', 'canceled')),
    summary         jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);
CREATE UNIQUE INDEX uk_jobs_idempotency_key ON jobs (idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_jobs_state_created ON jobs (state, created_at);

CREATE TABLE tasks (
    id               text PRIMARY KEY,
    job_id           text NOT NULL REFERENCES jobs (id),
    machine_id       text NOT NULL REFERENCES machines (id),
    state            text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'canceled', 'interrupted')),
    flow_name        text NOT NULL,
    stage_index      int NOT NULL DEFAULT 0,
    stage_attempt    int NOT NULL DEFAULT 0,
    delivery_count   int NOT NULL DEFAULT 0,
    stage_deadline   timestamptz,
    heartbeat_at     timestamptz,
    owner_runner     text,
    context          jsonb NOT NULL DEFAULT '{}'::jsonb,
    cancel_requested boolean NOT NULL DEFAULT false,
    error            jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz
);
CREATE INDEX idx_tasks_job_state ON tasks (job_id, state);
CREATE INDEX idx_tasks_heartbeat ON tasks (heartbeat_at);

CREATE TABLE task_stages (
    task_id     text NOT NULL REFERENCES tasks (id),
    seq         int NOT NULL,
    name        text NOT NULL,
    state       text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'skipped', 'canceled')),
    attempt     int NOT NULL DEFAULT 0,
    started_at  timestamptz,
    finished_at timestamptz,
    duration_ms int,
    PRIMARY KEY (task_id, seq)
);

CREATE TABLE events (
    id            bigserial PRIMARY KEY,
    resource_type text NOT NULL,
    resource_id   text NOT NULL,
    type          text NOT NULL,
    payload       jsonb,
    ts            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_events_resource ON events (resource_type, resource_id, id);

-- Table queue (docs/10-tech-stack.md D3): lease = visibility timeout.
-- A message is pollable when: not dead, visible_at <= now, and
-- (lease_expired_at IS NULL OR lease_expired_at < now). At-least-once delivery.
CREATE TABLE queue_messages (
    id               bigserial PRIMARY KEY,
    queue            text NOT NULL,
    payload          bytea NOT NULL,
    visible_at       timestamptz NOT NULL DEFAULT now(),
    lease_token      text,
    lease_expired_at timestamptz,
    delivery_count   int NOT NULL DEFAULT 0,
    dead             boolean NOT NULL DEFAULT false,
    dead_reason      text,
    enqueued_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_queue_poll ON queue_messages (queue, visible_at)
    WHERE dead = false;

-- +goose Down
DROP TABLE queue_messages;
DROP TABLE events;
DROP TABLE task_stages;
DROP TABLE tasks;
DROP TABLE jobs;
DROP TABLE layout_snapshots;
DROP TABLE machines;
DROP TABLE credentials;
