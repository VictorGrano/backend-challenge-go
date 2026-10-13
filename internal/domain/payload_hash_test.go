package domain

import (
	"errors"
	"testing"
)

func testPayload(t *testing.T) WagerPayload {
	t.Helper()

	return WagerPayload{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		PlayerID:              "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
		WalletID:              "0192f291-27dd-7d3f-8071-5f8685deef37",
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  WagerBet,
		Money:                 mustMoney(t, "25.00"),
	}
}

func mustHash(t *testing.T, p WagerPayload) string {
	t.Helper()

	hash, err := p.Hash()
	if err != nil {
		t.Fatalf("Hash returned error: %v", err)
	}

	return hash
}

func TestWagerPayloadHashIsDeterministic(t *testing.T) {
	const want = "629836932b79106b99523d06a1e7fa80689b0ea1e1c47aa3f0a5a2c87d0c4344"

	if got := mustHash(t, testPayload(t)); got != want {
		t.Errorf("hash = %s, want %s", got, want)
	}
}

func TestWagerPayloadHashNormalizesMoney(t *testing.T) {
	want := mustHash(t, testPayload(t))

	for _, amount := range []string{"25", "25.0", "025.00"} {
		p := testPayload(t)
		p.Money = mustMoney(t, amount)

		if got := mustHash(t, p); got != want {
			t.Errorf("hash for amount %q differs from canonical 25.00", amount)
		}
	}
}

func TestWagerPayloadHashChangesWithAnyField(t *testing.T) {
	base := mustHash(t, testPayload(t))

	mutations := map[string]func(*WagerPayload){
		"providerId":            func(p *WagerPayload) { p.ProviderID = "provider-b" },
		"externalTransactionId": func(p *WagerPayload) { p.ExternalTransactionID = "transaction-124" },
		"playerId":              func(p *WagerPayload) { p.PlayerID = "other-player" },
		"walletId":              func(p *WagerPayload) { p.WalletID = "other-wallet" },
		"roundId":               func(p *WagerPayload) { p.RoundID = "round-988" },
		"gameId":                func(p *WagerPayload) { p.GameID = "other-game" },
		"kind":                  func(p *WagerPayload) { p.Kind = WagerWin },
		"amount":                func(p *WagerPayload) { p.Money = mustMoney(t, "25.01") },
		"currency":              func(p *WagerPayload) { p.Money = mustUnits(t, 2500, USD) },
		"reference":             func(p *WagerPayload) { p.ReferenceExternalTransactionID = "transaction-100" },
	}

	for name, mutate := range mutations {
		p := testPayload(t)
		mutate(&p)

		if mustHash(t, p) == base {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}

func TestWagerPayloadHashRejectsUninitializedMoney(t *testing.T) {
	p := testPayload(t)
	p.Money = Money{}

	if _, err := p.Hash(); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("error = %v, want ErrInvalidCurrency", err)
	}
}
