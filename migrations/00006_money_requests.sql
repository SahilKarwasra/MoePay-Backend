-- +goose Up
CREATE TABLE money_requests (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    requester_user_id   uuid        NOT NULL REFERENCES users (id),
    payer_user_id       uuid        NOT NULL REFERENCES users (id),
    amount_pence        bigint      NOT NULL,
    currency            text        NOT NULL DEFAULT 'GBP',
    note                text,
    status              text        NOT NULL DEFAULT 'pending',
    paid_transaction_id uuid        REFERENCES transactions (id),
    expires_at          timestamptz NOT NULL,
    resolved_at         timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT money_requests_distinct_parties CHECK (requester_user_id <> payer_user_id),
    CONSTRAINT money_requests_amount_positive CHECK (amount_pence > 0),
    CONSTRAINT money_requests_currency_valid CHECK (currency = 'GBP'),
    CONSTRAINT money_requests_note_length CHECK (note IS NULL OR char_length(note) <= 140),
    CONSTRAINT money_requests_status_valid CHECK (status IN ('pending', 'paid', 'declined', 'cancelled', 'expired')),
    CONSTRAINT money_requests_paid_consistent CHECK ((status = 'paid') = (paid_transaction_id IS NOT NULL)),
    CONSTRAINT money_requests_resolved_consistent CHECK ((status = 'pending') = (resolved_at IS NULL)),
    CONSTRAINT money_requests_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX money_requests_payer_idx ON money_requests (payer_user_id, created_at DESC, id DESC);
CREATE INDEX money_requests_requester_idx ON money_requests (requester_user_id, created_at DESC, id DESC);
CREATE INDEX money_requests_pending_expiry_idx ON money_requests (expires_at) WHERE status = 'pending';
CREATE UNIQUE INDEX money_requests_paid_transaction_key ON money_requests (paid_transaction_id)
    WHERE paid_transaction_id IS NOT NULL;

CREATE TRIGGER money_requests_set_updated_at BEFORE UPDATE ON money_requests
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE transactions ADD COLUMN money_request_id uuid REFERENCES money_requests (id);
ALTER TABLE transactions ADD CONSTRAINT transactions_request_payment_has_request
    CHECK ((type = 'request_payment') = (money_request_id IS NOT NULL));
-- A request can be paid successfully at most once.
CREATE UNIQUE INDEX transactions_money_request_succeeded_key ON transactions (money_request_id)
    WHERE money_request_id IS NOT NULL AND status = 'succeeded';

-- +goose Down
ALTER TABLE transactions DROP COLUMN IF EXISTS money_request_id;
DROP TABLE IF EXISTS money_requests;
