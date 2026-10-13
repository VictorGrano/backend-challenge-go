package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	SourceHTTP   = "http"
	SourceSQS    = "sqs"
	SourceWorker = "worker"
)

type Metrics struct {
	registry *prometheus.Registry

	transactions     *prometheus.CounterVec
	duplicates       *prometheus.CounterVec
	processing       *prometheus.HistogramVec
	conflicts        *prometheus.CounterVec
	sqsMessages      *prometheus.CounterVec
	sqsRetries       prometheus.Counter
	sqsDLQ           *prometheus.CounterVec
	outboxPublished  *prometheus.CounterVec
	outboxLag        prometheus.Gauge
	referenceRetries prometheus.Counter
	divergences      prometheus.Counter
}

func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),

		transactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_transactions_total",
			Help: "Operações tratadas, por estado resultante, tipo e origem (replays não contam).",
		}, []string{"status", "kind", "source"}),

		duplicates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_duplicates_total",
			Help: "Recebimentos repetidos reconhecidos pela idempotência, por origem.",
		}, []string{"source"}),

		processing: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wager_processing_duration_seconds",
			Help:    "Latência do processamento de uma operação, por origem.",
			Buckets: prometheus.DefBuckets,
		}, []string{"source"}),

		conflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_concurrency_conflicts_total",
			Help: "Conflitos de concorrência: colisão na inserção da mesma operação ou conflito de idempotência.",
		}, []string{"kind"}),

		sqsMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqs_messages_total",
			Help: "Mensagens recebidas do SQS, por desfecho.",
		}, []string{"outcome"}),

		sqsRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sqs_retries_total",
			Help: "Mensagens devolvidas à fila para nova tentativa após falha transitória.",
		}),

		sqsDLQ: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqs_dlq_total",
			Help: "Mensagens enviadas à DLQ pela aplicação, por motivo.",
		}, []string{"reason"}),

		outboxPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_publish_total",
			Help: "Tentativas de publicação de eventos da outbox, por resultado.",
		}, []string{"result"}),

		outboxLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_lag_seconds",
			Help: "Idade do evento pendente mais antigo da outbox.",
		}),

		referenceRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_reference_retries_total",
			Help: "Operações em PENDING_REFERENCE reprocessadas pelo worker.",
		}),

		divergences: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wallet_reconciliation_divergences_total",
			Help: "Reconciliações em que o saldo armazenado diverge do ledger.",
		}),
	}

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.transactions, m.duplicates, m.processing, m.conflicts,
		m.sqsMessages, m.sqsRetries, m.sqsDLQ,
		m.outboxPublished, m.outboxLag, m.referenceRetries, m.divergences,
	)

	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

func (m *Metrics) Operation(source, status, kind string, replay bool, conflicts int, elapsed time.Duration) {
	m.processing.WithLabelValues(source).Observe(elapsed.Seconds())

	if conflicts > 0 {
		m.conflicts.WithLabelValues("insert_collision").Add(float64(conflicts))
	}

	if replay {
		m.duplicates.WithLabelValues(source).Inc()
		return
	}

	m.transactions.WithLabelValues(status, kind, source).Inc()
}

func (m *Metrics) Conflict(kind string) { m.conflicts.WithLabelValues(kind).Inc() }

func (m *Metrics) SQSMessage(outcome string) { m.sqsMessages.WithLabelValues(outcome).Inc() }
func (m *Metrics) SQSRetry()                 { m.sqsRetries.Inc() }
func (m *Metrics) SQSDeadLetter(reason string) {
	m.sqsDLQ.WithLabelValues(reason).Inc()
}

func (m *Metrics) OutboxPublished(result string) { m.outboxPublished.WithLabelValues(result).Inc() }
func (m *Metrics) OutboxLag(lag time.Duration)   { m.outboxLag.Set(lag.Seconds()) }
func (m *Metrics) ReferenceRetries(n int)        { m.referenceRetries.Add(float64(n)) }
func (m *Metrics) ReconciliationDivergence()     { m.divergences.Inc() }
