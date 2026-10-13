package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/VictorGrano/backend-challenge-go/internal/auth"
	"github.com/VictorGrano/backend-challenge-go/internal/metrics"
	"github.com/VictorGrano/backend-challenge-go/internal/wagering"
)

type Pinger interface {
	Ping(ctx context.Context) error
}

type ReadinessCheck struct {
	Name   string
	Pinger Pinger
}

type API struct {
	service        *wagering.Service
	authenticator  auth.Authenticator
	metrics        *metrics.Metrics
	logger         *slog.Logger
	requestTimeout time.Duration
	checks         []ReadinessCheck
}

func New(
	service *wagering.Service,
	authenticator auth.Authenticator,
	m *metrics.Metrics,
	logger *slog.Logger,
	requestTimeout time.Duration,
	checks []ReadinessCheck,
) *API {
	return &API{
		service:        service,
		authenticator:  authenticator,
		metrics:        m,
		logger:         logger,
		requestTimeout: requestTimeout,
		checks:         checks,
	}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health/live", a.live)
	mux.HandleFunc("GET /health/ready", a.ready)
	mux.Handle("GET /metrics", a.metrics.Handler())

	mux.Handle("POST /wallets", a.secured(internalOnly, a.openWallet))
	mux.Handle("GET /wallets/{walletId}", a.secured(internalOnly, a.getWallet))
	mux.Handle("GET /wallets/{walletId}/ledger", a.secured(internalOnly, a.getLedger))
	mux.Handle("POST /wallets/{walletId}/reconciliation", a.secured(internalOnly, a.reconcile))

	mux.Handle("POST /wagering/transactions", a.secured(providerOnly, a.submitTransaction))
	mux.Handle("GET /wagering/transactions/{transactionId}", a.secured(providerOrInternal, a.getTransaction))
	mux.Handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}",
		a.secured(providerOrInternal, a.getProviderTransaction))

	return a.observe(mux)
}

type permission func(auth.Principal) bool

func internalOnly(p auth.Principal) bool       { return p.IsInternal() }
func providerOnly(p auth.Principal) bool       { return p.IsProvider() }
func providerOrInternal(p auth.Principal) bool { return p.IsInternal() || p.IsProvider() }

func (a *API) secured(allowed permission, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
		if !found || !strings.EqualFold(scheme, "Bearer") {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			a.writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "missing bearer token")

			return
		}

		principal, err := a.authenticator.Authenticate(r.Context(), strings.TrimSpace(token))
		if err != nil {
			a.requestLogger(r).Warn("authentication failed", "error", err.Error())
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			a.writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "invalid or expired token")

			return
		}

		if state, ok := r.Context().Value(requestStateKey{}).(*requestState); ok {
			state.providerID = principal.ProviderID
		}

		if !allowed(principal) {
			a.writeError(w, r, http.StatusForbidden, codeForbidden, "not allowed to perform this operation")

			return
		}

		next(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	})
}

type requestStateKey struct{}

type requestState struct {
	correlationID string
	providerID    string
	walletID      string
	transactionID string
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

const correlationHeader = "X-Correlation-ID"

func (a *API) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		state := &requestState{correlationID: sanitizeCorrelationID(r.Header.Get(correlationHeader))}
		if state.correlationID == "" {
			state.correlationID = newCorrelationID()
		}

		ctx, cancel := context.WithTimeout(r.Context(), a.requestTimeout)
		defer cancel()

		ctx = context.WithValue(ctx, requestStateKey{}, state)
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		recorder.Header().Set(correlationHeader, state.correlationID)

		r = r.WithContext(ctx)
		next.ServeHTTP(recorder, r)

		probe := strings.HasPrefix(r.URL.Path, "/health/") || r.URL.Path == "/metrics"
		if probe && recorder.status < http.StatusInternalServerError {
			return
		}

		attrs := []any{
			"method", r.Method,
			"route", r.Pattern,
			"status", recorder.status,
			"durationMs", time.Since(start).Milliseconds(),
			"correlationId", state.correlationID,
		}

		for key, value := range map[string]string{
			"providerId":    state.providerID,
			"walletId":      state.walletID,
			"transactionId": state.transactionID,
		} {
			if value != "" {
				attrs = append(attrs, key, value)
			}
		}

		a.logger.Info("http request", attrs...)
	})
}

func (a *API) requestLogger(r *http.Request) *slog.Logger {
	return a.logger.With("correlationId", correlationID(r))
}

func correlationID(r *http.Request) string {
	if state, ok := r.Context().Value(requestStateKey{}).(*requestState); ok {
		return state.correlationID
	}

	return ""
}

func track(r *http.Request, walletID, transactionID string) {
	state, ok := r.Context().Value(requestStateKey{}).(*requestState)
	if !ok {
		return
	}

	if walletID != "" {
		state.walletID = walletID
	}

	if transactionID != "" {
		state.transactionID = transactionID
	}
}

func sanitizeCorrelationID(value string) string {
	if len(value) == 0 || len(value) > 64 {
		return ""
	}

	for _, r := range value {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':'
		if !ok {
			return ""
		}
	}

	return value
}

func newCorrelationID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "unavailable"
	}

	return hex.EncodeToString(raw[:])
}
