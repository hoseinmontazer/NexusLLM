-- NexusLLM Migration 052 — User Authentication & Credentials
--
-- Adds password_hash and seeds initial default admin account for NexusLLM.

BEGIN;

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS password_hash VARCHAR(255),
    ADD COLUMN IF NOT EXISTS name VARCHAR(255);

-- Ensure a default root admin account exists if missing
DO $$
DECLARE
    default_org_id UUID;
BEGIN
    SELECT id INTO default_org_id FROM organizations ORDER BY created_at ASC LIMIT 1;
    IF default_org_id IS NULL THEN
        INSERT INTO organizations (name, slug) VALUES ('Default Organization', 'default-org')
        RETURNING id INTO default_org_id;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM users WHERE email = 'admin@nexusllm.io') THEN
        INSERT INTO users (id, org_id, email, name, role, password_hash, active, created_at, updated_at)
        VALUES (
            gen_random_uuid(),
            default_org_id,
            'admin@nexusllm.io',
            'Platform Administrator',
            'admin',
            -- bcrypt hash for password "admin123"
            '$2a$10$CWeP43NFxYSMK0nF.qt8EuQL.a2MhGYjekLxAeQ3XtfJsJewxWUXe',
            TRUE,
            NOW(),
            NOW()
        );
    ELSE
        -- Ensure default admin user has valid password_hash if previously seeded with placeholder/null
        UPDATE users
        SET password_hash = '$2a$10$CWeP43NFxYSMK0nF.qt8EuQL.a2MhGYjekLxAeQ3XtfJsJewxWUXe',
            updated_at = NOW()
        WHERE email = 'admin@nexusllm.io'
          AND (password_hash IS NULL OR password_hash = '' OR password_hash = '$2a$10$w3V8Pj/7B0Tz0K5sZ8W2.O4e3V8Pj/7B0Tz0K5sZ8W2.O4e3V8Pj.');
    END IF;
END $$;

COMMIT;
