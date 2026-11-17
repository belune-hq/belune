CREATE TABLE registry_credentials (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    registry_host text NOT NULL UNIQUE,
    username text NOT NULL,
    password_encrypted bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
