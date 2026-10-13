package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/VictorGrano/backend-challenge-go/internal/auth"
	"github.com/VictorGrano/backend-challenge-go/internal/domain"
	"github.com/VictorGrano/backend-challenge-go/internal/metrics"
	"github.com/VictorGrano/backend-challenge-go/internal/wagering"
)

func (a *API) live(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	checks := make(map[string]string, len(a.checks))

	for _, check := range a.checks {
		if err := check.Pinger.Ping(r.Context()); err != nil {
			a.requestLogger(r).Warn("readiness check failed", "dependency", check.Name, "error", err.Error())
			checks[check.Name] = "unavailable"
			status = http.StatusServiceUnavailable

			continue
		}

		checks[check.Name] = "ok"
	}

	overall := "ok"
	if status != http.StatusOK {
		overall = "unavailable"
	}

	a.writeJSON(w, r, status, map[string]any{"status": overall, "checks": checks})
}

func (a *API) openWallet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlayerID       string        `json:"playerId"`
		InitialBalance *domain.Money `json:"initialBalance"`
	}

	if err := decodeBody(w, r, &body); err != nil {
		a.fail(w, r, err)
		return
	}

	if body.InitialBalance == nil {
		a.fail(w, r, fmt.Errorf("%w: initialBalance is required", wagering.ErrInvalidInput))
		return
	}

	wallet, err := a.service.OpenWallet(r.Context(), body.PlayerID, *body.InitialBalance, correlationID(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}

	track(r, wallet.ID(), "")
	w.Header().Set("Location", "/wallets/"+wallet.ID())
	a.writeJSON(w, r, http.StatusCreated, newWalletResponse(wallet))
}

func (a *API) getWallet(w http.ResponseWriter, r *http.Request) {
	track(r, r.PathValue("walletId"), "")

	wallet, err := a.service.GetWallet(r.Context(), r.PathValue("walletId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}

	a.writeJSON(w, r, http.StatusOK, newWalletResponse(wallet))
}

func (a *API) getLedger(w http.ResponseWriter, r *http.Request) {
	track(r, r.PathValue("walletId"), "")

	limit := 0

	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error

		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 {
			a.fail(w, r, fmt.Errorf("%w: limit must be a positive integer", wagering.ErrInvalidInput))
			return
		}
	}

	page, err := a.service.GetLedger(r.Context(), r.PathValue("walletId"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}

	a.writeJSON(w, r, http.StatusOK, newLedgerResponse(page))
}

func (a *API) reconcile(w http.ResponseWriter, r *http.Request) {
	track(r, r.PathValue("walletId"), "")

	rec, err := a.service.Reconcile(r.Context(), r.PathValue("walletId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}

	if !rec.Consistent {
		a.metrics.ReconciliationDivergence()
		a.requestLogger(r).Error("wallet reconciliation divergence",
			"walletId", rec.WalletID,
			"difference", rec.Difference.String(),
			"currency", string(rec.Difference.Currency()),
			"checkedEntries", rec.CheckedEntries,
		)
	}

	a.writeJSON(w, r, http.StatusOK, reconciliationResponse{
		WalletID:          rec.WalletID,
		StoredBalance:     rec.StoredBalance,
		CalculatedBalance: rec.CalculatedBalance,
		Difference:        rec.Difference,
		Consistent:        rec.Consistent,
		CheckedEntries:    rec.CheckedEntries,
	})
}

func (a *API) submitTransaction(w http.ResponseWriter, r *http.Request) {
	principal, _ := auth.PrincipalFrom(r.Context())

	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		a.fail(w, r, fmt.Errorf("%w: Idempotency-Key header is required", wagering.ErrInvalidInput))
		return
	}

	var body struct {
		ProviderID                     string           `json:"providerId"`
		ExternalTransactionID          string           `json:"externalTransactionId"`
		PlayerID                       string           `json:"playerId"`
		WalletID                       string           `json:"walletId"`
		RoundID                        string           `json:"roundId"`
		GameID                         string           `json:"gameId"`
		Kind                           domain.WagerKind `json:"kind"`
		Money                          *domain.Money    `json:"money"`
		ReferenceExternalTransactionID string           `json:"referenceExternalTransactionId"`
	}

	if err := decodeBody(w, r, &body); err != nil {
		a.fail(w, r, err)
		return
	}

	if body.Money == nil {
		a.fail(w, r, fmt.Errorf("%w: money is required", wagering.ErrInvalidInput))
		return
	}

	if body.ProviderID != principal.ProviderID {
		a.writeError(w, r, http.StatusForbidden, codeForbidden, "providerId does not match the authenticated provider")
		return
	}

	track(r, body.WalletID, "")
	start := time.Now()

	res, err := a.service.Submit(r.Context(), wagering.SubmitCommand{
		ProviderID:                     principal.ProviderID,
		ExternalTransactionID:          body.ExternalTransactionID,
		IdempotencyKey:                 idempotencyKey,
		PlayerID:                       body.PlayerID,
		WalletID:                       body.WalletID,
		RoundID:                        body.RoundID,
		GameID:                         body.GameID,
		Kind:                           body.Kind,
		Money:                          *body.Money,
		ReferenceExternalTransactionID: body.ReferenceExternalTransactionID,
		CorrelationID:                  correlationID(r),
	})
	if err != nil {
		switch {
		case errors.Is(err, wagering.ErrIdempotencyConflict):
			a.metrics.Conflict("idempotency_key")
		case errors.Is(err, wagering.ErrExternalIDConflict):
			a.metrics.Conflict("external_transaction")
		}

		a.fail(w, r, err)

		return
	}

	a.metrics.Operation(metrics.SourceHTTP, string(res.Status), string(body.Kind),
		res.IdempotentReplay, res.Conflicts, time.Since(start))
	track(r, "", res.TransactionID)

	status := http.StatusOK

	switch res.Status {
	case domain.WagerRejected:
		status = http.StatusUnprocessableEntity
	case domain.WagerPending, domain.WagerPendingReference:
		status = http.StatusAccepted
	}

	a.writeJSON(w, r, status, submitResponse{
		TransactionID:    res.TransactionID,
		Status:           res.Status,
		Balance:          res.Balance,
		FailureCode:      res.FailureCode,
		IdempotentReplay: res.IdempotentReplay,
	})
}

func (a *API) getTransaction(w http.ResponseWriter, r *http.Request) {
	principal, _ := auth.PrincipalFrom(r.Context())
	track(r, "", r.PathValue("transactionId"))

	tx, err := a.service.GetTransaction(r.Context(), r.PathValue("transactionId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}

	if !principal.IsInternal() && !principal.CanAccessProvider(tx.ProviderID()) {
		a.fail(w, r, domain.ErrTransactionNotFound)
		return
	}

	track(r, tx.WalletID(), "")
	a.writeJSON(w, r, http.StatusOK, newTransactionResponse(tx))
}

func (a *API) getProviderTransaction(w http.ResponseWriter, r *http.Request) {
	principal, _ := auth.PrincipalFrom(r.Context())
	providerID := r.PathValue("providerId")

	if !principal.CanAccessProvider(providerID) {
		a.writeError(w, r, http.StatusForbidden, codeForbidden, "not allowed to access this provider")
		return
	}

	tx, err := a.service.GetProviderTransaction(r.Context(), providerID, r.PathValue("externalTransactionId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}

	track(r, tx.WalletID(), tx.ID())
	a.writeJSON(w, r, http.StatusOK, newTransactionResponse(tx))
}
