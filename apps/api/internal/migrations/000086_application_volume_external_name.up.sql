-- Adoption protects the volume independently of any application's lifetime.
CREATE TABLE adopted_volumes (
    name VARCHAR(255) PRIMARY KEY,
    adopted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Existing rows remain NULL: their immutable inputs still derive the name.
ALTER TABLE application_volumes ADD COLUMN external_name VARCHAR(255)
    REFERENCES adopted_volumes(name) ON DELETE RESTRICT;
-- A registry PK cannot prevent two attachments referencing the same entry.
CREATE UNIQUE INDEX idx_application_volumes_external
    ON application_volumes(external_name) WHERE external_name IS NOT NULL;
