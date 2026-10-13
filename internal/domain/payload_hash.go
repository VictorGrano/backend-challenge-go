package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type WagerPayload struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           WagerKind
	Money                          Money
	ReferenceExternalTransactionID string
}

func (p WagerPayload) Hash() (string, error) {
	if !p.Money.IsValid() {
		return "", ErrInvalidCurrency
	}

	fields := map[string]any{
		"providerId":            p.ProviderID,
		"externalTransactionId": p.ExternalTransactionID,
		"playerId":              p.PlayerID,
		"walletId":              p.WalletID,
		"roundId":               p.RoundID,
		"gameId":                p.GameID,
		"kind":                  string(p.Kind),
		"money": map[string]string{
			"amount":   p.Money.String(),
			"currency": string(p.Money.Currency()),
		},
	}

	if p.ReferenceExternalTransactionID != "" {
		fields["referenceExternalTransactionId"] = p.ReferenceExternalTransactionID
	}

	canonical, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)

	return hex.EncodeToString(sum[:]), nil
}
