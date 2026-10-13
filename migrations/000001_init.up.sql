CREATE TABLE wallets (
    id         UUID        PRIMARY KEY,
    player_id  UUID        NOT NULL,
    currency   CHAR(3)     NOT NULL,
    balance    BIGINT      NOT NULL,
    version    BIGINT      NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,

    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency),
    CONSTRAINT wallets_id_currency_key UNIQUE (id, currency),
    CONSTRAINT wallets_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallets_balance_non_negative CHECK (balance >= 0),
    CONSTRAINT wallets_version_positive CHECK (version >= 1),
    CONSTRAINT wallets_timestamps CHECK (updated_at >= created_at)
);

CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    kind                              TEXT        NOT NULL,
    status                            TEXT        NOT NULL,
    wallet_id                         UUID        NOT NULL REFERENCES wallets (id),
    player_id                         UUID        NOT NULL,
    amount                            BIGINT      NOT NULL,
    currency                          CHAR(3)     NOT NULL,
    provider_id                       TEXT,
    external_transaction_id           TEXT,
    idempotency_key                   TEXT,
    payload_hash                      TEXT,
    round_id                          TEXT,
    game_id                           TEXT,
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID        REFERENCES wager_transactions (id),
    failure_code                      TEXT,
    result_balance                    BIGINT,
    reference_attempts                INTEGER     NOT NULL DEFAULT 0,
    next_reference_attempt_at         TIMESTAMPTZ,
    created_at                        TIMESTAMPTZ NOT NULL,
    updated_at                        TIMESTAMPTZ NOT NULL,

    CONSTRAINT wager_transactions_id_wallet_key UNIQUE (id, wallet_id),
    CONSTRAINT wager_transactions_provider_external_key UNIQUE (provider_id, external_transaction_id),
    CONSTRAINT wager_transactions_provider_idempotency_key UNIQUE (provider_id, idempotency_key),

    CONSTRAINT wager_transactions_kind CHECK (
        kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')
    ),
    CONSTRAINT wager_transactions_status CHECK (
        status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')
    ),
    CONSTRAINT wager_transactions_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wager_transactions_amount CHECK (
        (kind = 'LOSS' AND amount = 0) OR (kind <> 'LOSS' AND amount > 0)
    ),
    CONSTRAINT wager_transactions_origin CHECK (
        (
            kind = 'OPENING'
            AND provider_id IS NULL
            AND external_transaction_id IS NULL
            AND idempotency_key IS NULL
            AND payload_hash IS NULL
            AND round_id IS NULL
            AND game_id IS NULL
            AND reference_external_transaction_id IS NULL
            AND reference_transaction_id IS NULL
        ) OR (
            kind <> 'OPENING'
            AND provider_id IS NOT NULL
            AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL
            AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL
            AND game_id IS NOT NULL
        )
    ),
    CONSTRAINT wager_transactions_reference CHECK (
        (kind NOT IN ('REFUND', 'ROLLBACK') OR reference_external_transaction_id IS NOT NULL)
        AND (reference_transaction_id IS NULL OR reference_external_transaction_id IS NOT NULL)
        AND (reference_transaction_id IS NULL OR reference_transaction_id <> id)
    ),
    CONSTRAINT wager_transactions_status_result CHECK (
        CASE status
            WHEN 'PROCESSED' THEN
                result_balance IS NOT NULL
                AND result_balance >= 0
                AND failure_code IS NULL
                AND (kind NOT IN ('REFUND', 'ROLLBACK') OR reference_transaction_id IS NOT NULL)
            WHEN 'REJECTED' THEN failure_code IS NOT NULL AND result_balance IS NULL
            WHEN 'FAILED' THEN failure_code IS NOT NULL AND result_balance IS NULL
            WHEN 'PENDING_REFERENCE' THEN
                failure_code IS NULL
                AND result_balance IS NULL
                AND reference_external_transaction_id IS NOT NULL
                AND next_reference_attempt_at IS NOT NULL
            ELSE failure_code IS NULL AND result_balance IS NULL
        END
    ),
    CONSTRAINT wager_transactions_reference_attempts CHECK (reference_attempts >= 0),
    CONSTRAINT wager_transactions_timestamps CHECK (updated_at >= created_at)
);

CREATE UNIQUE INDEX wager_transactions_opening_per_wallet
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

CREATE UNIQUE INDEX wager_transactions_single_reversal
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

CREATE INDEX wager_transactions_pending_reference
    ON wager_transactions (next_reference_attempt_at)
    WHERE status = 'PENDING_REFERENCE';

CREATE INDEX wager_transactions_wallet ON wager_transactions (wallet_id);

CREATE FUNCTION wager_transactions_protect() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wager_transactions rows cannot be deleted'
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager transaction % is terminal (%)', OLD.id, OLD.status
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wager_transactions_protect
    BEFORE UPDATE OR DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_protect();

CREATE TABLE wallet_ledger_entries (
    seq            BIGINT      GENERATED ALWAYS AS IDENTITY,
    id             UUID        PRIMARY KEY,
    wallet_id      UUID        NOT NULL,
    transaction_id UUID        NOT NULL,
    direction      TEXT        NOT NULL,
    amount         BIGINT      NOT NULL,
    currency       CHAR(3)     NOT NULL,
    balance_before BIGINT      NOT NULL,
    balance_after  BIGINT      NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,

    CONSTRAINT wallet_ledger_entries_wallet_transaction_key UNIQUE (wallet_id, transaction_id),
    CONSTRAINT wallet_ledger_entries_wallet_seq_key UNIQUE (wallet_id, seq),
    CONSTRAINT wallet_ledger_entries_wallet_fk
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),
    CONSTRAINT wallet_ledger_entries_transaction_fk
        FOREIGN KEY (transaction_id, wallet_id) REFERENCES wager_transactions (id, wallet_id),
    CONSTRAINT wallet_ledger_entries_direction CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT wallet_ledger_entries_amount_positive CHECK (amount > 0),
    CONSTRAINT wallet_ledger_entries_balances_non_negative CHECK (
        balance_before >= 0 AND balance_after >= 0
    ),
    CONSTRAINT wallet_ledger_entries_balance_invariant CHECK (
        (direction = 'DEBIT' AND balance_after = balance_before - amount)
        OR (direction = 'CREDIT' AND balance_after = balance_before + amount)
    )
);

CREATE FUNCTION wallet_ledger_entries_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only (% blocked)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_ledger_entries_no_update_delete
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_immutable();

CREATE TRIGGER wallet_ledger_entries_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION wallet_ledger_entries_immutable();

CREATE TABLE inbox_messages (
    consumer_name  TEXT        NOT NULL,
    message_id     TEXT        NOT NULL,
    payload_hash   TEXT        NOT NULL,
    transaction_id UUID        REFERENCES wager_transactions (id),
    received_at    TIMESTAMPTZ NOT NULL,
    completed_at   TIMESTAMPTZ,

    PRIMARY KEY (consumer_name, message_id),
    CONSTRAINT inbox_messages_timestamps CHECK (completed_at IS NULL OR completed_at >= received_at)
);

CREATE TABLE outbox_events (
    seq             BIGINT      GENERATED ALWAYS AS IDENTITY,
    event_id        UUID        PRIMARY KEY,
    aggregate_id    UUID        NOT NULL,
    event_type      TEXT        NOT NULL,
    payload         JSON        NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    published_at    TIMESTAMPTZ,
    last_error      TEXT,

    CONSTRAINT outbox_events_attempts CHECK (attempts >= 0)
);

CREATE INDEX outbox_events_pending
    ON outbox_events (next_attempt_at, seq)
    WHERE published_at IS NULL;

CREATE FUNCTION outbox_events_protect() RETURNS trigger AS $$
BEGIN
    IF NEW.event_id IS DISTINCT FROM OLD.event_id
        OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id
        OR NEW.event_type IS DISTINCT FROM OLD.event_type
        OR NEW.payload::text IS DISTINCT FROM OLD.payload::text
        OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at THEN
        RAISE EXCEPTION 'outbox event % is immutable', OLD.event_id
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER outbox_events_protect
    BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_protect();
