-- +goose Up
-- One-to-one conversations. direct_key is the two member ids in sorted order, so
-- each pair of users has exactly one conversation.
CREATE TABLE conversations (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       text        NOT NULL DEFAULT 'direct',
    direct_key text        NOT NULL,
    -- Position of the newest message; incremented under this row's lock so message
    -- order matches commit order.
    last_seq   bigint      NOT NULL DEFAULT 0,
    -- Drives the conversation list order (most recent activity first).
    last_message_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT conversations_kind_valid CHECK (kind = 'direct'),
    CONSTRAINT conversations_last_seq_non_negative CHECK (last_seq >= 0)
);

CREATE UNIQUE INDEX conversations_direct_key ON conversations (direct_key);

CREATE TRIGGER conversations_set_updated_at BEFORE UPDATE ON conversations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE conversation_members (
    conversation_id uuid        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    last_read_seq   bigint      NOT NULL DEFAULT 0,
    joined_at       timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (conversation_id, user_id),
    CONSTRAINT conversation_members_read_seq_non_negative CHECK (last_read_seq >= 0)
);

CREATE INDEX conversation_members_user_idx ON conversation_members (user_id);
CREATE INDEX conversations_activity_idx ON conversations (last_message_at DESC, id DESC);

CREATE TRIGGER conversation_members_set_updated_at BEFORE UPDATE ON conversation_members
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE messages (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id   uuid        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    sender_id         uuid        NOT NULL REFERENCES users (id),
    seq               bigint      NOT NULL,
    kind              text        NOT NULL,
    body              text,
    -- Client-generated id so an offline client can retry a send without duplicates
    -- and match the server copy to its local pending message.
    client_message_id uuid,
    reply_to_id       uuid        REFERENCES messages (id),
    transaction_id    uuid        REFERENCES transactions (id),
    money_request_id  uuid        REFERENCES money_requests (id),
    version           integer     NOT NULL DEFAULT 1,
    edited_at         timestamptz,
    deleted_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT messages_kind_valid CHECK (kind IN ('text', 'payment', 'request')),
    CONSTRAINT messages_seq_positive CHECK (seq > 0),
    CONSTRAINT messages_body_length CHECK (body IS NULL OR char_length(body) BETWEEN 1 AND 4000),
    CONSTRAINT messages_text_has_body CHECK (kind <> 'text' OR deleted_at IS NOT NULL OR body IS NOT NULL),
    CONSTRAINT messages_payment_has_transaction CHECK ((kind = 'payment') = (transaction_id IS NOT NULL)),
    CONSTRAINT messages_request_has_request CHECK ((kind = 'request') = (money_request_id IS NOT NULL)),
    CONSTRAINT messages_only_text_editable CHECK (kind = 'text' OR (edited_at IS NULL AND deleted_at IS NULL)),
    CONSTRAINT messages_version_positive CHECK (version > 0)
);

CREATE UNIQUE INDEX messages_conversation_seq_key ON messages (conversation_id, seq);
CREATE UNIQUE INDEX messages_client_id_key ON messages (sender_id, client_message_id) WHERE client_message_id IS NOT NULL;
CREATE UNIQUE INDEX messages_transaction_key ON messages (transaction_id) WHERE transaction_id IS NOT NULL;
CREATE UNIQUE INDEX messages_money_request_key ON messages (money_request_id) WHERE money_request_id IS NOT NULL;

CREATE TRIGGER messages_set_updated_at BEFORE UPDATE ON messages
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Previous bodies of edited messages, for audit.
CREATE TABLE message_edits (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id    uuid        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    previous_body text        NOT NULL,
    version       integer     NOT NULL,
    edited_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX message_edits_message_idx ON message_edits (message_id, version);

-- Per-user change feed for offline-first sync. Each user has a counter; every change
-- visible to the user takes the next value while holding that user's cursor row
-- lock, so a client that has seen change N has also seen every change before it.
-- chat_changes keeps only the latest change per entity, so a full resync returns
-- current state rather than replaying history.
CREATE TABLE chat_sync_cursors (
    user_id  uuid   PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    last_seq bigint NOT NULL DEFAULT 0
);

CREATE TABLE chat_changes (
    user_id         uuid   NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    entity_type     text   NOT NULL,
    entity_id       uuid   NOT NULL,
    conversation_id uuid   NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq             bigint NOT NULL,

    PRIMARY KEY (user_id, entity_type, entity_id),
    CONSTRAINT chat_changes_entity_type_valid CHECK (entity_type IN ('conversation', 'message'))
);

CREATE UNIQUE INDEX chat_changes_user_seq_key ON chat_changes (user_id, seq);

-- +goose Down
DROP TABLE IF EXISTS chat_changes;
DROP TABLE IF EXISTS chat_sync_cursors;
DROP TABLE IF EXISTS message_edits;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS conversation_members;
DROP TABLE IF EXISTS conversations;
