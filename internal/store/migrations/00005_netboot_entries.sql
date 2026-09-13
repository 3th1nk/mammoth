-- Network boot entries (docs/06-install-pipeline.md §3.3, M7 PXE boot path):
-- one row per machine armed to boot over the LAN. The runner registers a
-- row in prepare_media and deletes it on release (completed / terminal
-- failure / cancel / retry rebuild); the netboot service (proxyDHCP facet +
-- /netboot/* machine face) resolves incoming clients by MAC. UNIQUE(mac)
-- makes "at most one pending boot per machine" a database guarantee.
-- Orphans from a crashed runner are swept by the reaper against task
-- terminal states — no second, time-based lifecycle on purpose.

-- +goose Up
CREATE TABLE netboot_entries (
    id          bigserial PRIMARY KEY,
    mac         text NOT NULL,           -- normalized lowercase colon form
    task_id     text NOT NULL,
    machine_id  text NOT NULL,
    token       text NOT NULL,           -- machine-face credential + boot tree dir
    kind        text NOT NULL,           -- install | probe
    kernel      text NOT NULL,           -- file names inside the boot tree
    initrd      text NOT NULL,
    kernel_args text NOT NULL DEFAULT '',
    extra       jsonb,                   -- additional servable files (e.g. modloop)
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_netboot_entries_mac ON netboot_entries (mac);
CREATE INDEX idx_netboot_entries_task ON netboot_entries (task_id);

-- +goose Down
DROP TABLE netboot_entries;
