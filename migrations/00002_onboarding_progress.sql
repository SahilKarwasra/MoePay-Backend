-- +goose Up
CREATE TABLE onboarding_progress (
    user_id         uuid        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    status          text        NOT NULL DEFAULT 'in_progress',
    current_step    text        NOT NULL DEFAULT 'profile',
    completed_steps text[]      NOT NULL DEFAULT '{}',
    completed_at    timestamptz,
    version         integer     NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT onboarding_status_valid CHECK (status IN ('in_progress', 'completed')),
    CONSTRAINT onboarding_current_step_valid CHECK (current_step IN ('profile', 'username', 'review', 'completed')),
    CONSTRAINT onboarding_completed_steps_valid CHECK (completed_steps <@ ARRAY['profile', 'username']::text[]),
    CONSTRAINT onboarding_completed_consistent CHECK (
        (status = 'completed') = (completed_at IS NOT NULL)
        AND (status = 'completed') = (current_step = 'completed')
    )
);

CREATE INDEX onboarding_progress_in_progress_idx ON onboarding_progress (updated_at) WHERE status = 'in_progress';

CREATE TRIGGER onboarding_progress_set_updated_at BEFORE UPDATE ON onboarding_progress
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS onboarding_progress;
