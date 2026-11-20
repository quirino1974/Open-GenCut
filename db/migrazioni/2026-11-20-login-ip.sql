-- Migrazione per i database MariaDB creati prima del 20/11/2026.
-- Il blocco del login ora vale per utente + indirizzo e i tentativi vecchi si cancellano da soli:
-- serve la colonna ip in login_attempt e gli indici per i controlli e per la pulizia.
-- Script idempotente. Applicazione:
--   docker exec -i -e MYSQL_PWD="$DB_ROOT_PASSWORD" opengencut-db mariadb -uroot geniuscut < db/migrazioni/2026-11-20-login-ip.sql
-- Fare il backup prima (vedi il manuale operativo, sezione 5).

ALTER TABLE login_attempt ADD COLUMN IF NOT EXISTS ip VARCHAR(45) NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS ix_attempt_ip_time ON login_attempt (ip, at);
CREATE INDEX IF NOT EXISTS ix_attempt_at ON login_attempt (at);
CREATE INDEX IF NOT EXISTS ix_session_expires ON login_session (expires_at);
