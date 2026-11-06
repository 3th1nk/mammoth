package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/3th1nk/mammoth/internal/store"
)

// agentLogSuite plants a job + running task carrying a render token, and a
// gin engine mounting only the agent-log ship route. PG-gated like the
// other repo-backed suites.
func agentLogSuite(t *testing.T) (*gin.Engine, *sql.DB, string, string) {
	t.Helper()
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping agent-log ship suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	job, task := store.NewID("tst_agl_j_"), store.NewID("tst_agl_t_")
	token := "agltok_" + strings.TrimPrefix(task, "tst_agl_t_")
	machine, cred := store.NewID("tst_agl_m_"), store.NewID("tst_agl_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM task_logs WHERE task_id = $1`, task)
		_, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, task)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job)
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'agentlog-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://agentlog', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, state)
		VALUES ($1, 'install', '{}'::jsonb, 'running')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context)
		VALUES ($1, $2, $3, 'install', 'running', 0, jsonb_build_object('token', $4::text))`,
		task, job, machine, token); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	s := &Server{Deps: Deps{Jobs: store.NewJobRepo(db), Logs: store.NewTaskLogRepo(db)}}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/render/:token/agentlog", func(c *gin.Context) {
		s.UploadAgentLog(c)
	})
	return r, db, task, token
}

func agentLogLines(t *testing.T, db *sql.DB, task string) []map[string]string {
	t.Helper()
	rows, err := db.Query(`
		SELECT level, stage, message FROM task_logs
		WHERE task_id = $1 AND stage = 'agent' ORDER BY id`, task)
	if err != nil {
		t.Fatalf("query task_logs: %v", err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var level, stage, message string
		if err := rows.Scan(&level, &stage, &message); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, map[string]string{"level": level, "stage": stage, "message": message})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestAgentLogShip(t *testing.T) {
	r, db, task, token := agentLogSuite(t)

	// A normal ship: lines filed one by one, blank lines dropped.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/render/"+token+"/agentlog",
		strings.NewReader("[mammoth-agent] agent runtime ab-v16 starting\n\n[mammoth-agent] storage: sfdisk done\n")))
	if w.Code != http.StatusNoContent {
		t.Fatalf("ship = %d %q, want 204", w.Code, w.Body.String())
	}
	lines := agentLogLines(t, db, task)
	if len(lines) != 2 ||
		lines[0]["message"] != "[mammoth-agent] agent runtime ab-v16 starting" ||
		lines[1]["message"] != "[mammoth-agent] storage: sfdisk done" {
		t.Fatalf("filed lines wrong: %+v", lines)
	}
	if lines[0]["level"] != "INFO" || lines[0]["stage"] != "agent" {
		t.Fatalf("level/stage wrong: %+v", lines[0])
	}

	// A second ship (the incremental beat) appends rather than replaces.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/render/"+token+"/agentlog",
		strings.NewReader("[mammoth-agent] base system installed\n")))
	if w.Code != http.StatusNoContent {
		t.Fatalf("second ship = %d, want 204", w.Code)
	}
	if got := len(agentLogLines(t, db, task)); got != 3 {
		t.Fatalf("append ships produced %d lines, want 3", got)
	}

	// An unknown token is the machine-face credential failing — 404, and
	// nothing filed.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/render/agltok_nope/agentlog",
		strings.NewReader("[mammoth-agent] intruder line\n")))
	if w.Code == http.StatusNoContent {
		t.Fatalf("unknown token accepted")
	}

	// An oversized line is truncated to the filed-line cap, not rejected.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/render/"+token+"/agentlog",
		strings.NewReader("[mammoth-agent] "+strings.Repeat("x", 20<<10)+"\n")))
	if w.Code != http.StatusNoContent {
		t.Fatalf("oversized-line ship = %d, want 204 (truncation contract)", w.Code)
	}
	lines = agentLogLines(t, db, task)
	last := lines[len(lines)-1]
	if len(last["message"]) != agentLogLineMax {
		t.Fatalf("oversized line filed at %d bytes, want capped at %d", len(last["message"]), agentLogLineMax)
	}

	// An empty body is a silent 204 (a beat with no new lines).
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/render/"+token+"/agentlog",
		strings.NewReader("")))
	if w.Code != http.StatusNoContent {
		t.Fatalf("empty ship = %d, want 204", w.Code)
	}
}
