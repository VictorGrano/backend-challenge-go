package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/VictorGrano/backend-challenge-go/internal/domain"
	"github.com/VictorGrano/backend-challenge-go/internal/postgres"
	"github.com/VictorGrano/backend-challenge-go/internal/wagering"
)

const (
	codeInvalidInput          = "INVALID_INPUT"
	codeUnauthenticated       = "UNAUTHENTICATED"
	codeForbidden             = "FORBIDDEN"
	codeWalletNotFound        = "WALLET_NOT_FOUND"
	codeTransactionNotFound   = "TRANSACTION_NOT_FOUND"
	codeWalletAlreadyExists   = "WALLET_ALREADY_EXISTS"
	codeIdempotencyConflict   = "IDEMPOTENCY_KEY_CONFLICT"
	codeExternalIDConflict    = "EXTERNAL_TRANSACTION_CONFLICT"
	codeTemporarilyUnavailble = "TEMPORARILY_UNAVAILABLE"
	codeInternal              = "INTERNAL_ERROR"
)

const (
	maxBodyBytes      = 64 << 10
	retryAfterSeconds = "1"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlationId,omitempty"`
}

func (a *API) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		a.requestLogger(r).Error("encode response", "error", err.Error())
		http.Error(w, `{"error":{"code":"INTERNAL_ERROR","message":"internal error"}}`, http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func (a *API) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	a.writeJSON(w, r, status, errorBody{Error: errorDetail{
		Code:          code,
		Message:       message,
		CorrelationID: correlationID(r),
	}})
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, wagering.ErrInvalidInput):
		a.writeError(w, r, http.StatusBadRequest, codeInvalidInput, err.Error())
	case errors.Is(err, domain.ErrWalletNotFound):
		a.writeError(w, r, http.StatusNotFound, codeWalletNotFound, "wallet not found")
	case errors.Is(err, domain.ErrTransactionNotFound):
		a.writeError(w, r, http.StatusNotFound, codeTransactionNotFound, "transaction not found")
	case errors.Is(err, domain.ErrWalletAlreadyExists):
		a.writeError(w, r, http.StatusConflict, codeWalletAlreadyExists,
			"player already has a wallet in this currency")
	case errors.Is(err, wagering.ErrIdempotencyConflict):
		a.writeError(w, r, http.StatusConflict, codeIdempotencyConflict,
			"idempotency key was already used with a different payload")
	case errors.Is(err, wagering.ErrExternalIDConflict),
		errors.Is(err, domain.ErrDuplicateTransaction),
		errors.Is(err, domain.ErrDuplicateIdempotencyKey):
		a.writeError(w, r, http.StatusConflict, codeExternalIDConflict,
			"external transaction was already submitted with another idempotency key")
	case postgres.IsTransient(err):
		a.requestLogger(r).Warn("transient failure", "error", err.Error())
		w.Header().Set("Retry-After", retryAfterSeconds)
		a.writeError(w, r, http.StatusServiceUnavailable, codeTemporarilyUnavailble,
			"temporarily unavailable, retry with the same Idempotency-Key")
	default:
		a.requestLogger(r).Error("unexpected failure", "error", err.Error())
		a.writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("%w: malformed JSON body: %v", wagering.ErrInvalidInput, err)
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: body must contain a single JSON object", wagering.ErrInvalidInput)
	}

	return nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

type walletResponse struct {
	ID        string       `json:"id"`
	PlayerID  string       `json:"playerId"`
	Balance   domain.Money `json:"balance"`
	Version   int64        `json:"version"`
	CreatedAt string       `json:"createdAt"`
	UpdatedAt string       `json:"updatedAt"`
}

func newWalletResponse(w *domain.Wallet) walletResponse {
	return walletResponse{
		ID:        w.ID(),
		PlayerID:  w.PlayerID(),
		Balance:   w.Balance(),
		Version:   w.Version(),
		CreatedAt: formatTime(w.CreatedAt()),
		UpdatedAt: formatTime(w.UpdatedAt()),
	}
}

type ledgerEntryResponse struct {
	ID            string                 `json:"id"`
	WalletID      string                 `json:"walletId"`
	TransactionID string                 `json:"transactionId"`
	Direction     domain.LedgerDirection `json:"direction"`
	Money         domain.Money           `json:"money"`
	BalanceBefore domain.Money           `json:"balanceBefore"`
	BalanceAfter  domain.Money           `json:"balanceAfter"`
	CreatedAt     string                 `json:"createdAt"`
}

type ledgerResponse struct {
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

func newLedgerResponse(page wagering.LedgerPage) ledgerResponse {
	res := ledgerResponse{
		Entries:    make([]ledgerEntryResponse, len(page.Entries)),
		NextCursor: page.NextCursor,
	}

	for i, e := range page.Entries {
		res.Entries[i] = ledgerEntryResponse{
			ID:            e.ID(),
			WalletID:      e.WalletID(),
			TransactionID: e.TransactionID(),
			Direction:     e.Direction(),
			Money:         e.Money(),
			BalanceBefore: e.BalanceBefore(),
			BalanceAfter:  e.BalanceAfter(),
			CreatedAt:     formatTime(e.CreatedAt()),
		}
	}

	return res
}

type submitResponse struct {
	TransactionID    string             `json:"transactionId"`
	Status           domain.WagerStatus `json:"status"`
	Balance          *domain.Money      `json:"balance,omitempty"`
	FailureCode      domain.FailureCode `json:"failureCode,omitempty"`
	IdempotentReplay bool               `json:"idempotentReplay"`
}

type transactionResponse struct {
	TransactionID                  string             `json:"transactionId"`
	ProviderID                     string             `json:"providerId,omitempty"`
	ExternalTransactionID          string             `json:"externalTransactionId,omitempty"`
	WalletID                       string             `json:"walletId"`
	PlayerID                       string             `json:"playerId"`
	RoundID                        string             `json:"roundId,omitempty"`
	GameID                         string             `json:"gameId,omitempty"`
	Kind                           domain.WagerKind   `json:"kind"`
	Money                          domain.Money       `json:"money"`
	Status                         domain.WagerStatus `json:"status"`
	Balance                        *domain.Money      `json:"balance,omitempty"`
	FailureCode                    domain.FailureCode `json:"failureCode,omitempty"`
	ReferenceExternalTransactionID string             `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string             `json:"referenceTransactionId,omitempty"`
	CreatedAt                      string             `json:"createdAt"`
	UpdatedAt                      string             `json:"updatedAt"`
}

func newTransactionResponse(tx *domain.WagerTransaction) transactionResponse {
	res := transactionResponse{
		TransactionID:                  tx.ID(),
		ProviderID:                     tx.ProviderID(),
		ExternalTransactionID:          tx.ExternalTransactionID(),
		WalletID:                       tx.WalletID(),
		PlayerID:                       tx.PlayerID(),
		RoundID:                        tx.RoundID(),
		GameID:                         tx.GameID(),
		Kind:                           tx.Kind(),
		Money:                          tx.Money(),
		Status:                         tx.Status(),
		FailureCode:                    tx.FailureCode(),
		ReferenceExternalTransactionID: tx.ReferenceExternalTransactionID(),
		ReferenceTransactionID:         tx.ReferenceTransactionID(),
		CreatedAt:                      formatTime(tx.CreatedAt()),
		UpdatedAt:                      formatTime(tx.UpdatedAt()),
	}

	if balance, ok := tx.ResultBalance(); ok {
		res.Balance = &balance
	}

	return res
}

type reconciliationResponse struct {
	WalletID          string       `json:"walletId"`
	StoredBalance     domain.Money `json:"storedBalance"`
	CalculatedBalance domain.Money `json:"calculatedBalance"`
	Difference        domain.Money `json:"difference"`
	Consistent        bool         `json:"consistent"`
	CheckedEntries    int64        `json:"checkedEntries"`
}
