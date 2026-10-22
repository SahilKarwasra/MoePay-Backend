-- +goose Up
-- One row per OTP sent. Codes are stored as HMAC-SHA256(server secret, phone || code).
CREATE TABLE otp_challenges (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_e164   text        NOT NULL,
    code_hash    bytea       NOT NULL,
    attempts     smallint    NOT NULL DEFAULT 0,
    max_attempts smallint    NOT NULL,
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,
    request_ip   inet,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT otp_phone_e164_format CHECK (phone_e164 ~ '^\+[1-9][0-9]{6,14}$'),
    CONSTRAINT otp_attempts_range CHECK (attempts >= 0 AND attempts <= max_attempts),
    CONSTRAINT otp_max_attempts_positive CHECK (max_attempts > 0),
    CONSTRAINT otp_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX otp_challenges_phone_created_idx ON otp_challenges (phone_e164, created_at DESC);
CREATE INDEX otp_challenges_expires_idx ON otp_challenges (expires_at);

-- One row per signed-in device. Refresh tokens are opaque random values stored as
-- SHA-256 hashes and rotated on every refresh; the previous hash is kept so that
-- replay of a rotated token can be detected and the session revoked.
CREATE TABLE sessions (
    id                          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    refresh_token_hash          bytea       NOT NULL,
    previous_refresh_token_hash bytea,
    user_agent                  text,
    ip                          inet,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    last_used_at                timestamptz NOT NULL DEFAULT now(),
    expires_at                  timestamptz NOT NULL,
    revoked_at                  timestamptz,
    revoked_reason              text,

    CONSTRAINT sessions_revoked_reason_valid CHECK (
        revoked_reason IS NULL OR revoked_reason IN ('logout', 'logout_all', 'refresh_reuse', 'admin')
    ),
    CONSTRAINT sessions_revocation_consistent CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL)),
    CONSTRAINT sessions_user_agent_length CHECK (user_agent IS NULL OR char_length(user_agent) <= 512)
);

CREATE UNIQUE INDEX sessions_refresh_token_hash_key ON sessions (refresh_token_hash);
CREATE INDEX sessions_previous_refresh_token_hash_idx ON sessions (previous_refresh_token_hash)
    WHERE previous_refresh_token_hash IS NOT NULL;
CREATE INDEX sessions_user_active_idx ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- +goose Down
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS otp_challenges;
