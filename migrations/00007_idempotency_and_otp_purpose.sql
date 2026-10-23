-- +goose Up
-- Stored outcome of money-moving requests, written in the same database transaction
-- as the ledger postings so a retry with the same key replays the original result.
CREATE TABLE idempotency_keys (
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key          text        NOT NULL,
    endpoint     text        NOT NULL,
    request_hash bytea       NOT NULL,
    outcome      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, key),
    CONSTRAINT idempotency_keys_key_length CHECK (char_length(key) BETWEEN 8 AND 100)
);

CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

ALTER TABLE otp_challenges ADD COLUMN purpose text NOT NULL DEFAULT 'login';
ALTER TABLE otp_challenges ADD CONSTRAINT otp_purpose_valid CHECK (purpose IN ('login', 'pin_reset'));
DROP INDEX otp_challenges_phone_created_idx;
CREATE INDEX otp_challenges_phone_purpose_created_idx ON otp_challenges (phone_e164, purpose, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS otp_challenges_phone_purpose_created_idx;
CREATE INDEX otp_challenges_phone_created_idx ON otp_challenges (phone_e164, created_at DESC);
ALTER TABLE otp_challenges DROP CONSTRAINT IF EXISTS otp_purpose_valid;
ALTER TABLE otp_challenges DROP COLUMN IF EXISTS purpose;
DROP TABLE IF EXISTS idempotency_keys;
