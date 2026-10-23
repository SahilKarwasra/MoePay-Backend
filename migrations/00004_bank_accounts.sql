-- +goose Up
-- Linked accounts at the dummy bank. Only the last four digits are kept in clear;
-- the full number is stored as an HMAC so duplicates can be detected.
CREATE TABLE bank_accounts (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    holder_name         text        NOT NULL,
    bank_name           text        NOT NULL,
    sort_code           text        NOT NULL,
    account_last4       text        NOT NULL,
    account_number_hash bytea       NOT NULL,
    is_primary          boolean     NOT NULL DEFAULT false,
    status              text        NOT NULL DEFAULT 'active',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    removed_at          timestamptz,

    CONSTRAINT bank_accounts_holder_name_length CHECK (char_length(holder_name) BETWEEN 1 AND 70),
    CONSTRAINT bank_accounts_sort_code_format CHECK (sort_code ~ '^[0-9]{6}$'),
    CONSTRAINT bank_accounts_last4_format CHECK (account_last4 ~ '^[0-9]{4}$'),
    CONSTRAINT bank_accounts_status_valid CHECK (status IN ('active', 'removed')),
    CONSTRAINT bank_accounts_removed_consistent CHECK ((status = 'removed') = (removed_at IS NOT NULL)),
    CONSTRAINT bank_accounts_removed_not_primary CHECK (NOT (is_primary AND status = 'removed'))
);

CREATE UNIQUE INDEX bank_accounts_user_account_active_key ON bank_accounts (user_id, account_number_hash)
    WHERE status = 'active';
CREATE UNIQUE INDEX bank_accounts_one_primary_key ON bank_accounts (user_id) WHERE is_primary;
CREATE INDEX bank_accounts_user_active_idx ON bank_accounts (user_id, created_at) WHERE status = 'active';

CREATE TRIGGER bank_accounts_set_updated_at BEFORE UPDATE ON bank_accounts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS bank_accounts;
