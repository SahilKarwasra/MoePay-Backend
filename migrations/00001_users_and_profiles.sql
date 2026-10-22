-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TABLE users (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_e164          text        NOT NULL,
    phone_verified_at   timestamptz,
    status              text        NOT NULL DEFAULT 'active',
    username            text,
    username_normalized text,
    kyc_status          text        NOT NULL DEFAULT 'not_started',
    last_login_at       timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_phone_e164_format CHECK (phone_e164 ~ '^\+[1-9][0-9]{6,14}$'),
    CONSTRAINT users_status_valid CHECK (status IN ('active', 'suspended', 'deleted')),
    CONSTRAINT users_kyc_status_valid CHECK (kyc_status IN ('not_started', 'pending', 'verified', 'rejected')),
    -- username and its normalized form are set together; the normalized form is the
    -- lowercase ASCII identity used for uniqueness.
    CONSTRAINT users_username_pair CHECK ((username IS NULL) = (username_normalized IS NULL)),
    CONSTRAINT users_username_normalized_format CHECK (username_normalized ~ '^[a-z][a-z0-9_.]{2,29}$'),
    CONSTRAINT users_username_matches_normalized CHECK (lower(username) = username_normalized)
);

CREATE UNIQUE INDEX users_phone_e164_key ON users (phone_e164);
CREATE UNIQUE INDEX users_username_normalized_key ON users (username_normalized) WHERE username_normalized IS NOT NULL;

CREATE TRIGGER users_set_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE profiles (
    user_id       uuid        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    first_name    text,
    last_name     text,
    email         text,
    date_of_birth date,
    avatar_url    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT profiles_first_name_length CHECK (first_name IS NULL OR char_length(first_name) BETWEEN 1 AND 50),
    CONSTRAINT profiles_last_name_length CHECK (last_name IS NULL OR char_length(last_name) BETWEEN 1 AND 50),
    CONSTRAINT profiles_email_format CHECK (email IS NULL OR (char_length(email) <= 254 AND email ~ '^[^@\s]+@[^@\s]+\.[^@\s]+$')),
    CONSTRAINT profiles_dob_range CHECK (date_of_birth IS NULL OR date_of_birth >= DATE '1900-01-01'),
    CONSTRAINT profiles_avatar_url_length CHECK (avatar_url IS NULL OR char_length(avatar_url) <= 2048)
);

CREATE TRIGGER profiles_set_updated_at BEFORE UPDATE ON profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS profiles;
DROP TABLE IF EXISTS users;
DROP FUNCTION IF EXISTS set_updated_at();
