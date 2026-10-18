CREATE TABLE clients (
    id text PRIMARY KEY,
    name text NOT NULL,
    role text NOT NULL CHECK (role IN ('editor','staff','outstaff')),
    trust double precision NOT NULL CHECK (trust BETWEEN 0 AND 1),
    enabled boolean NOT NULL DEFAULT true,
    key_hash text NOT NULL
);
CREATE TABLE editorial_policy (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    threshold double precision NOT NULL CHECK (threshold > 0),
    half_life_seconds bigint NOT NULL CHECK (half_life_seconds > 0),
    congratulations_count integer NOT NULL CHECK (congratulations_count >= 0)
);
-- Значения по умолчанию
INSERT INTO editorial_policy VALUES (true, 1.1, 21600, 3);
CREATE TABLE stories (
    id text PRIMARY KEY,
    category text COLLATE "C" NOT NULL,
    topic text COLLATE "C" NOT NULL,
    half_life_seconds bigint NOT NULL,
    UNIQUE (category,topic)
);
CREATE TABLE notes (
    id text PRIMARY KEY,
    client_id text NOT NULL REFERENCES clients(id),
    request_id text NOT NULL,
    payload_hash text NOT NULL,
    story_id text NOT NULL REFERENCES stories(id),
    body text NOT NULL,
    event_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    published boolean NOT NULL DEFAULT false,
    UNIQUE (client_id, request_id)
);
CREATE INDEX notes_story_idx ON notes(story_id);
CREATE TABLE contributions (
    story_id text NOT NULL REFERENCES stories(id),
    client_id text NOT NULL REFERENCES clients(id),
    note_id text NOT NULL UNIQUE REFERENCES notes(id),
    source_name text NOT NULL,
    trust double precision NOT NULL,
    event_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    rank integer NOT NULL,
    PRIMARY KEY (story_id,client_id),
    UNIQUE (story_id,rank)
);
CREATE INDEX contributions_client_idx ON contributions(client_id);
CREATE TABLE publications (
    story_id text PRIMARY KEY REFERENCES stories(id),
    published_at timestamptz NOT NULL,
    trust double precision NOT NULL,
    threshold double precision NOT NULL,
    half_life_seconds bigint NOT NULL,
    congratulations_count integer NOT NULL
);
CREATE TABLE notifications (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    client_id text NOT NULL REFERENCES clients(id),
    story_id text NOT NULL REFERENCES publications(story_id),
    rank integer NOT NULL,
    message text NOT NULL,
    created_at timestamptz NOT NULL,
    acknowledged boolean NOT NULL DEFAULT false,
    UNIQUE (client_id,story_id)
);
CREATE INDEX notifications_inbox_idx ON notifications(client_id,id);
CREATE INDEX notifications_pending_idx ON notifications(client_id,id) WHERE NOT acknowledged;
