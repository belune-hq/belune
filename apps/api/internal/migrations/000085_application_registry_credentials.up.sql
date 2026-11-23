-- App credentials are scoped to source_image's derived registry host; no second host column can drift.
ALTER TABLE applications
 ADD COLUMN registry_username text,
 ADD COLUMN registry_password_encrypted bytea;
