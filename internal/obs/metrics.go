package obs

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the M0 metric set (docs/10-tech-stack.md D6): job/task counts by
// state, stage duration, BMC call latency and errors, queue depth.
type Metrics struct {
	JobsTotal   *prometheus.CounterVec // label: type, state
	TasksTotal  *prometheus.CounterVec // label: type, state
	StageDur    *prometheus.HistogramVec
	BMCDuration *prometheus.HistogramVec // label: vendor, operation, result
	BMCErrors   *prometheus.CounterVec   // label: code
	QueueDepth  *prometheus.GaugeVec     // label: queue
	QueueMsgs   *prometheus.CounterVec   // label: queue, outcome
	HTTPDur     *prometheus.HistogramVec // label: method, route, status
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	factory := prometheus.WrapRegistererWithPrefix("mammoth_", reg)
	m := &Metrics{
		JobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jobs_total",
			Help: "Jobs transitioned to a state, by job type.",
		}, []string{"type", "state"}),
		TasksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tasks_total",
			Help: "Tasks transitioned to a state, by job type.",
		}, []string{"type", "state"}),
		StageDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "task_stage_duration_seconds",
			Help:    "Stage execution duration.",
			Buckets: []float64{.1, .5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600},
		}, []string{"flow", "stage"}),
		BMCDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "bmc_request_duration_seconds",
			Help:    "Out-of-band call duration by vendor, operation and result.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"vendor", "operation", "result"}),
		BMCErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bmc_errors_total",
			Help: "Out-of-band call failures by error code.",
		}, []string{"code"}),
		QueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "queue_depth",
			Help: "Messages waiting in a queue (including leased).",
		}, []string{"queue"}),
		QueueMsgs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "queue_messages_total",
			Help: "Queue message outcomes.",
		}, []string{"queue", "outcome"}),
		HTTPDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "API request duration by method, route and status.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route", "status"}),
	}
	factory.MustRegister(
		m.JobsTotal, m.TasksTotal, m.StageDur, m.BMCDuration,
		m.BMCErrors, m.QueueDepth, m.QueueMsgs, m.HTTPDur,
	)
	return m
}
