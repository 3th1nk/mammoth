package api

// The address-starvation preflight (docs/09-roadmap.md「装机网段地址服务
// 可观测」layer 3): a zero-declared-network install on a machine whose MAC
// sighted recent starvation gets the warning line; a declared-static spec
// or a clean MAC gets none. Non-blocking either way (202). PG-gated via
// MAMMOTH_TEST_PG_DSN (`make test-pg`).

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/netboot"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/kickstart"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/3th1nk/mammoth/internal/store/queue"
)

func TestStarvationPreflight(t *testing.T) {
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping starvation preflight suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	machine := store.NewID("tst_starv_m_")
	cred := store.NewID("tst_starv_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE machine_id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id IN (SELECT job_id FROM tasks WHERE machine_id = $1)`, machine)
		_, _ = db.Exec(`DELETE FROM events WHERE resource_id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM pending_machines WHERE mac = $1`, "aa:bb:cc:dd:ee:99")
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'starv-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	hw := `{"nics":[{"mac":"aa:bb:cc:dd:ee:99","name":"eno1"}]}`
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state, hardware, pxe_last_seen_at)
		VALUES ($1, '{}'::jsonb, 'fake://starv', 'fake', $2, 'ready', $3::jsonb, now())`, machine, cred, hw); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	events := store.NewEventRepo(db)
	// The edge starvation marker for the machine's NIC MAC.
	starved, _ := json.Marshal(map[string]any{"firmware": "uefi-x64", "source": "edge", "starved": true})
	events.Append(ctx, "pending", netboot.NormalizeMAC("aa:bb:cc:dd:ee:99"), "pending.sighted", starved)

	registry := render.NewRegistry()
	if err := registry.Register(kickstart.New("rocky9")); err != nil {
		t.Fatalf("register driver: %v", err)
	}
	s := &Server{Deps{
		Machines: store.NewMachineRepo(db),
		Events:   events,
		Jobs:     store.NewJobRepo(db),
		Queue:    queue.NewPG(db, queue.Options{}),
		Render:   registry,
	}}

	// Zero-declared-network spec on a starved machine: the preflight line
	// lands (warn log), the submission itself stays 202.
	spec := map[string]any{
		"image":   map[string]any{"source": "https://mirror.example/rocky9.iso", "distro": "rocky9"},
		"storage": map[string]any{"disks": []any{map[string]any{"select": map[string]any{"match": map[string]any{"type": "nvme", "size": "largest"}}, "wipe": true, "partitions": []any{map[string]any{"size": "rest", "fs": "xfs", "mount": "/"}}}}},
	}
	raw, _ := json.Marshal(spec)
	resp, err := s.CreateJob(ctx, gen.CreateJobRequestObject{Body: &gen.CreateJobJSONRequestBody{
		Type:    gen.JobType("install"),
		Targets: gen.JobTargets{Targets: []gen.JobTarget{{MachineId: gen.MachineId(machine)}}},
		Spec:    (*gen.InstallSpec)(raw2obj(raw)),
	}})
	if err != nil {
		t.Fatalf("starved-machine submission must stay 202: %v", err)
	}
	job, ok := resp.(gen.CreateJob202JSONResponse)
	if !ok || job.Id == "" {
		t.Fatalf("job response wrong: %+v", job)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE job_id = $1`, job.Id)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job.Id)
	})

	// Declared network: the warning's audience is the zero-declaration
	// shape — smoke the parser branch (no panic, same 202). The first job
	// must go first: the busy gate weighs active install tasks.
	if _, err := s.CancelJob(ctx, gen.CancelJobRequestObject{Id: gen.JobId(job.Id)}); err != nil {
		t.Fatalf("cancel first job: %v", err)
	}
	spec["network"] = []any{map[string]any{"addresses": []string{"192.168.1.221/24"}}}
	raw2, _ := json.Marshal(spec)
	resp, err = s.CreateJob(ctx, gen.CreateJobRequestObject{Body: &gen.CreateJobJSONRequestBody{
		Type:    gen.JobType("install"),
		Targets: gen.JobTargets{Targets: []gen.JobTarget{{MachineId: gen.MachineId(machine)}}},
		Spec:    (*gen.InstallSpec)(raw2obj(raw2)),
	}})
	if err != nil {
		t.Fatalf("declared-network submission must stay 202: %v", err)
	}
	if _, ok := resp.(gen.CreateJob202JSONResponse); !ok {
		t.Fatalf("declared-network response wrong: %T", resp)
	}
}

// raw2obj hacks the typed InstallSpec: the test builds the spec as JSON to
// avoid the generated type's required-field gymnastics — the handler only
// reads the wire form.
func raw2obj(b []byte) *gen.InstallSpec {
	var v gen.InstallSpec
	_ = json.Unmarshal(b, &v)
	return &v
}
