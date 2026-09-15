-- Zero-registration PXE sightings (docs/09-roadmap.md): machines that
-- announced themselves over PXE but are not registered yet. Deliberately NOT
-- rows in machines — a pending sighting has no BMC credential, and machines
-- are anchored by bmc_address with a required credential reference. The PXE
-- responder feeds firmware (option 93) on every DISCOVER; the enrollment
-- probe feeds the /sys report once it has booted. Claim (promote to a
-- registered machine) is a follow-up operation.

-- +goose Up
CREATE TABLE pending_machines (
    mac           text PRIMARY KEY,      -- normalized lowercase colon form
    firmware      text,                  -- last option 93 label ("uefi-x64", …)
    report        jsonb,                 -- enrollment probe's /sys scan, verbatim
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE pending_machines;
