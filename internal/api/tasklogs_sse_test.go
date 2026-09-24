package api

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
)

// sseConnWriter adapts a net.Conn (both ends of net.Pipe) into the
// http.ResponseWriter surface ssePoll needs: Header/Write plus Flusher —
// flush is a no-op because net.Pipe is synchronous per Write.
type sseConnWriter struct {
	conn net.Conn
}

func (w sseConnWriter) Header() http.Header         { return http.Header{} }
func (w sseConnWriter) Write(b []byte) (int, error) { return w.conn.Write(b) }
func (w sseConnWriter) WriteHeader(int)             {}
func (w sseConnWriter) Flush()                      {}

// readUntil reads from the client side until want appears in the stream or
// the deadline passes; returns everything received.
func readUntil(t *testing.T, conn net.Conn, want ...string) string {
	t.Helper()
	var buf strings.Builder
	chunk := make([]byte, 4096)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := conn.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err != nil && !os.IsTimeout(err) && !strings.Contains(err.Error(), "i/o timeout") {
			t.Fatalf("read stream: %v", err)
		}
		done := true
		for _, w := range want {
			if !strings.Contains(buf.String(), w) {
				done = false
				break
			}
		}
		if done {
			return buf.String()
		}
	}
	t.Fatalf("stream timeout: got %q, want all of %v", buf.String(), want)
	return ""
}

// seedTaskLogs plants a job + running task with two log lines; returns the
// ids and a cleanup-known db handle.
func seedTaskLogs(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	dsn := os.Getenv("MAMMOTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MAMMOTH_TEST_PG_DSN not set; skipping task-log stream suite")
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	job, task := store.NewID("tst_sse_j_"), store.NewID("tst_sse_t_")
	machine, cred := store.NewID("tst_sse_m_"), store.NewID("tst_sse_c_")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM task_logs WHERE task_id = $1`, task)
		_, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, task)
		_, _ = db.Exec(`DELETE FROM jobs WHERE id = $1`, job)
		_, _ = db.Exec(`DELETE FROM machines WHERE id = $1`, machine)
		_, _ = db.Exec(`DELETE FROM credentials WHERE id = $1`, cred)
	})
	if _, err := db.Exec(`
		INSERT INTO credentials (id, name, type, secret_encrypted)
		VALUES ($1, 'tasklog-sse-test', 'bmc', '\x00'::bytea)`, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO machines (id, labels, bmc_address, bmc_protocol, bmc_credential_id, state)
		VALUES ($1, '{}'::jsonb, 'fake://sse', 'fake', $2, 'ready')`, machine, cred); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO jobs (id, type, request, state)
		VALUES ($1, 'install', '{}'::jsonb, 'running')`, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (id, job_id, machine_id, flow_name, state, stage_index, context)
		VALUES ($1, $2, $3, 'install', 'running', 0, '{}'::jsonb)`, task, job, machine); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	logs := store.NewTaskLogRepo(db)
	logs.AppendTaskLog(context.Background(), task, "INFO", "boot", "first line", nil)
	logs.AppendTaskLog(context.Background(), task, "WARN", "install_os", "second line", map[string]any{"k": "v"})
	return db, job, task
}

// TestStreamJobTaskLogs replays the backlog, tails live lines through the
// shared SSE loop, and closes with eos once the task goes terminal.
func TestStreamJobTaskLogs(t *testing.T) {
	db, job, task := seedTaskLogs(t)
	s := &Server{Deps{Jobs: store.NewJobRepo(db), Logs: store.NewTaskLogRepo(db)}}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := gen.StreamJobTaskLogsRequestObject{Id: gen.JobId(job), TaskId: gen.TaskId(task)}
	vis, err := s.StreamJobTaskLogs(ctx, req)
	if err != nil {
		t.Fatalf("stream handler: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- vis.VisitStreamJobTaskLogsResponse(sseConnWriter{conn: server}) }()

	// Backlog replay: both seeded lines arrive as `log` frames.
	stream := readUntil(t, client, "retry: 3000", "first line", "second line")
	if !strings.Contains(stream, "event: log") {
		t.Errorf("frames missing event name: %q", stream)
	}

	// Live tail: a line appended after connect lands on the same stream.
	store.NewTaskLogRepo(db).AppendTaskLog(context.Background(), task, "INFO", "boot", "live line", nil)
	readUntil(t, client, "live line")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("visitor: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("visitor did not return after context cancel")
	}
}

// TestStreamJobTaskLogsEOS: a task already terminal answers with the
// backlog followed by the terminal `eos` frame, then closes by itself.
func TestStreamJobTaskLogsEOS(t *testing.T) {
	db, job, task := seedTaskLogs(t)
	if _, err := db.Exec(`UPDATE tasks SET state = 'failed' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	s := &Server{Deps{Jobs: store.NewJobRepo(db), Logs: store.NewTaskLogRepo(db)}}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	req := gen.StreamJobTaskLogsRequestObject{Id: gen.JobId(job), TaskId: gen.TaskId(task)}
	vis, err := s.StreamJobTaskLogs(context.Background(), req)
	if err != nil {
		t.Fatalf("stream handler: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- vis.VisitStreamJobTaskLogsResponse(sseConnWriter{conn: server}) }()

	stream := readUntil(t, client, "first line", "event: eos")
	if !strings.Contains(stream, "first line") || !strings.Contains(stream, "second line") {
		t.Errorf("backlog incomplete before eos: %q", stream)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("visitor did not close after eos")
	}
}

// TestStreamJobTaskLogsResume: `after` replays only the tail after the
// given log id (Last-Event-ID would win, absent here).
func TestStreamJobTaskLogsResume(t *testing.T) {
	db, job, task := seedTaskLogs(t)
	s := &Server{Deps{Jobs: store.NewJobRepo(db), Logs: store.NewTaskLogRepo(db)}}

	logs, err := store.NewTaskLogRepo(db).List(context.Background(), store.TaskLogFilter{TaskID: task})
	if err != nil || len(logs) < 2 {
		t.Fatalf("seed logs: %d items, %v", len(logs), err)
	}
	after := logs[0].ID

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	afterParam := strconv.FormatInt(after, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := gen.StreamJobTaskLogsRequestObject{
		Id:     gen.JobId(job),
		TaskId: gen.TaskId(task),
		Params: gen.StreamJobTaskLogsParams{After: &afterParam},
	}
	vis, err := s.StreamJobTaskLogs(ctx, req)
	if err != nil {
		t.Fatalf("stream handler: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- vis.VisitStreamJobTaskLogsResponse(sseConnWriter{conn: server}) }()

	stream := readUntil(t, client, "second line")
	if strings.Contains(stream, "first line") {
		t.Errorf("after=%d replayed the first line: %q", after, stream)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("visitor did not return after context cancel")
	}
}
