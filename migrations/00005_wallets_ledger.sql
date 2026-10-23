-- +goose Up
-- Double-entry ledger. Every money movement is one transactions row plus ledger
-- entries that sum to zero. wallets.balance_pence is a cache of the entries, updated
-- in the same database transaction.
CREATE TABLE wallets (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             uuid        REFERENCES users (id) ON DELETE RESTRICT,
    kind                text        NOT NULL DEFAULT 'user',
    system_code         text,
    currency            text        NOT NULL DEFAULT 'GBP',
    balance_pence       bigint      NOT NULL DEFAULT 0,
    status              text        NOT NULL DEFAULT 'active',
    pin_hash            text,
    pin_failed_attempts smallint    NOT NULL DEFAULT 0,
    pin_locked_until    timestamptz,
    pin_set_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT wallets_kind_valid CHECK (kind IN ('user', 'system')),
    CONSTRAINT wallets_owner_matches_kind CHECK ((kind = 'user') = (user_id IS NOT NULL)),
    CONSTRAINT wallets_system_code_matches_kind CHECK ((kind = 'system') = (system_code IS NOT NULL)),
    CONSTRAINT wallets_currency_valid CHECK (currency = 'GBP'),
    -- Only system (clearing) wallets may go negative.
    CONSTRAINT wallets_user_balance_non_negative CHECK (kind = 'system' OR balance_pence >= 0),
    CONSTRAINT wallets_status_valid CHECK (status IN ('active', 'frozen')),
    CONSTRAINT wallets_pin_attempts_non_negative CHECK (pin_failed_attempts >= 0),
    CONSTRAINT wallets_pin_set_consistent CHECK ((pin_hash IS NULL) = (pin_set_at IS NULL))
);

CREATE UNIQUE INDEX wallets_user_id_key ON wallets (user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX wallets_system_code_key ON wallets (system_code) WHERE system_code IS NOT NULL;

CREATE TRIGGER wallets_set_updated_at BEFORE UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Counterparty for money moving between MoePay and the (dummy) bank.
INSERT INTO wallets (kind, system_code) VALUES ('system', 'BANK_CLEARING');

CREATE TABLE transactions (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    reference         text        NOT NULL,
    type              text        NOT NULL,
    status            text        NOT NULL,
    amount_pence      bigint      NOT NULL,
    currency          text        NOT NULL DEFAULT 'GBP',
    from_wallet_id    uuid        NOT NULL REFERENCES wallets (id),
    to_wallet_id      uuid        NOT NULL REFERENCES wallets (id),
    initiator_user_id uuid        NOT NULL REFERENCES users (id),
    bank_account_id   uuid        REFERENCES bank_accounts (id),
    note              text,
    failure_code      text,
    failure_reason    text,
    idempotency_key   text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    completed_at      timestamptz,

    CONSTRAINT transactions_type_valid CHECK (type IN ('p2p_transfer', 'add_money', 'withdraw', 'request_payment')),
    CONSTRAINT transactions_status_valid CHECK (status IN ('succeeded', 'failed')),
    CONSTRAINT transactions_amount_positive CHECK (amount_pence > 0),
    CONSTRAINT transactions_currency_valid CHECK (currency = 'GBP'),
    CONSTRAINT transactions_distinct_wallets CHECK (from_wallet_id <> to_wallet_id),
    CONSTRAINT transactions_note_length CHECK (note IS NULL OR char_length(note) <= 140),
    CONSTRAINT transactions_failure_consistent CHECK ((status = 'failed') = (failure_code IS NOT NULL)),
    CONSTRAINT transactions_bank_flows_have_account CHECK (
        (type IN ('add_money', 'withdraw')) = (bank_account_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX transactions_reference_key ON transactions (reference);
CREATE UNIQUE INDEX transactions_idempotency_key ON transactions (initiator_user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- History is read per wallet in both directions with keyset pagination.
CREATE INDEX transactions_from_wallet_history_idx ON transactions (from_wallet_id, created_at DESC, id DESC);
CREATE INDEX transactions_to_wallet_history_idx ON transactions (to_wallet_id, created_at DESC, id DESC);

CREATE TABLE ledger_entries (
    id                  bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id      uuid        NOT NULL REFERENCES transactions (id),
    wallet_id           uuid        NOT NULL REFERENCES wallets (id),
    direction           text        NOT NULL,
    amount_pence        bigint      NOT NULL,
    balance_after_pence bigint      NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ledger_entries_direction_valid CHECK (direction IN ('debit', 'credit')),
    CONSTRAINT ledger_entries_amount_positive CHECK (amount_pence > 0)
);

CREATE UNIQUE INDEX ledger_entries_txn_wallet_direction_key ON ledger_entries (transaction_id, wallet_id, direction);
CREATE INDEX ledger_entries_wallet_idx ON ledger_entries (wallet_id, id);

-- +goose StatementBegin
-- Checked at commit: every transaction's entries must sum to zero.
CREATE FUNCTION ledger_check_balanced() RETURNS trigger AS $$
DECLARE
    net bigint;
BEGIN
    SELECT COALESCE(SUM(CASE direction WHEN 'credit' THEN amount_pence ELSE -amount_pence END), 0)
    INTO net FROM ledger_entries WHERE transaction_id = NEW.transaction_id;
    IF net <> 0 THEN
        RAISE EXCEPTION 'ledger transaction % is unbalanced by %', NEW.transaction_id, net
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();

-- +goose StatementBegin
-- The ledger is append-only.
CREATE FUNCTION ledger_reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only' USING ERRCODE = '42501';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();

-- +goose Down
DROP TABLE IF EXISTS ledger_entries;
DROP FUNCTION IF EXISTS ledger_reject_mutation();
DROP FUNCTION IF EXISTS ledger_check_balanced();
DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS wallets;
