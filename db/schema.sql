-- ============================================================================
-- Open-GenCut v0.1 - schema MariaDB (registro unificato, punti 1-8 e identita')
-- Database: geniuscut, utf8mb4. Convertito da T-SQL (SQL Server); vedi backend/dialect.go.
-- Script idempotente: applicato alla prima inizializzazione del volume dal compose.
-- Autore: Quirino Scaramastra
-- ============================================================================


CREATE DATABASE IF NOT EXISTS geniuscut CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

USE geniuscut;

CREATE TABLE IF NOT EXISTS agent (
    id INT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(150) NOT NULL,
    description VARCHAR(1000) NULL,
    origin VARCHAR(20) NOT NULL,
    vendor VARCHAR(150) NULL,
    version VARCHAR(50) NULL,
    owner VARCHAR(150) NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'registered',
    model VARCHAR(150) NULL,
    endpoint VARCHAR(300) NULL,
    cost_monthly DECIMAL(18,4) NOT NULL DEFAULT 0,
    currency CHAR(3) NOT NULL DEFAULT 'EUR',
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT ck_agent_origin CHECK (origin IN ('internal', 'marketplace', 'third_party')),
    CONSTRAINT ck_agent_status CHECK (status IN ('registered', 'active', 'suspended', 'retired')),
    CONSTRAINT ck_agent_cost CHECK (cost_monthly >= 0),
    CONSTRAINT ck_agent_currency CHECK (currency IN ('EUR', 'USD'))
);

CREATE TABLE IF NOT EXISTS configuration (
    id INT AUTO_INCREMENT PRIMARY KEY,
    `key` VARCHAR(150) NOT NULL,
    value LONGTEXT NOT NULL DEFAULT '',
    scope VARCHAR(10) NOT NULL,
    agent_id INT NULL,
    description VARCHAR(1000) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT ck_configuration_scope_agent CHECK (
        (scope = 'global' AND agent_id IS NULL) OR
        (scope = 'agent'  AND agent_id IS NOT NULL)
    ),
    CONSTRAINT uq_configuration_key UNIQUE (`key`),
    CONSTRAINT ck_configuration_scope CHECK (scope IN ('global', 'agent')),
    CONSTRAINT fk_configuration_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS audit (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    actor VARCHAR(150) NOT NULL DEFAULT 'api',
    action VARCHAR(20) NOT NULL,
    entity VARCHAR(30) NOT NULL,
    entity_id INT NULL,
    detail VARCHAR(1000) NULL
);

CREATE TABLE IF NOT EXISTS usage_log (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    called_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    tokens INT NOT NULL DEFAULT 0,
    cost DECIMAL(18,4) NOT NULL DEFAULT 0,
    CONSTRAINT fk_usage_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS ix_agent_origin_status ON agent (origin, status);
CREATE INDEX IF NOT EXISTS ix_agent_owner ON agent (owner);
CREATE INDEX IF NOT EXISTS ix_configuration_agent ON configuration (agent_id);
CREATE INDEX IF NOT EXISTS ix_audit_at ON audit (at);
CREATE INDEX IF NOT EXISTS ix_usage_agent_time ON usage_log (agent_id, called_at);

CREATE TABLE IF NOT EXISTS integration_resource (
    id INT AUTO_INCREMENT PRIMARY KEY,
    kind VARCHAR(20) NOT NULL,
    name VARCHAR(150) NOT NULL,
    endpoint VARCHAR(300) NULL,
    description VARCHAR(1000) NULL,
    status VARCHAR(10) NOT NULL DEFAULT 'enabled',
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT uq_resource_kind_name UNIQUE (kind, name),
    CONSTRAINT ck_resource_kind CHECK (kind IN ('llm_model', 'api', 'mcp_server', 'data_source', 'ai_gateway')),
    CONSTRAINT ck_resource_status CHECK (status IN ('enabled', 'disabled'))
);

CREATE TABLE IF NOT EXISTS agent_access (
    id INT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    resource_id INT NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT uq_agent_access UNIQUE (agent_id, resource_id),
    CONSTRAINT fk_access_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT fk_access_resource FOREIGN KEY (resource_id) REFERENCES integration_resource (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS access_limit (
    id INT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    resource_id INT NOT NULL,
    max_calls_per_day INT NULL,
    max_cost_per_month DECIMAL(18,4) NULL,
    currency CHAR(3) NOT NULL DEFAULT 'EUR',
    CONSTRAINT uq_limit_pair UNIQUE (agent_id, resource_id),
    CONSTRAINT fk_limit_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT fk_limit_resource FOREIGN KEY (resource_id) REFERENCES integration_resource (id) ON DELETE CASCADE,
    CONSTRAINT ck_limit_calls CHECK (max_calls_per_day IS NULL OR max_calls_per_day > 0),
    CONSTRAINT ck_limit_cost CHECK (max_cost_per_month IS NULL OR max_cost_per_month >= 0),
    CONSTRAINT ck_limit_currency CHECK (currency IN ('EUR', 'USD'))
);

CREATE TABLE IF NOT EXISTS access_event (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    agent_id INT NOT NULL,
    resource_id INT NOT NULL,
    decision VARCHAR(10) NOT NULL,
    reason VARCHAR(50) NOT NULL,
    cost DECIMAL(18,4) NOT NULL DEFAULT 0,
    CONSTRAINT ck_event_decision CHECK (decision IN ('allowed', 'denied'))
);

CREATE INDEX IF NOT EXISTS ix_access_event_pair ON access_event (agent_id, resource_id, at);

ALTER TABLE usage_log ADD COLUMN IF NOT EXISTS success BOOLEAN NOT NULL DEFAULT 1;

ALTER TABLE usage_log ADD COLUMN IF NOT EXISTS latency_ms INT NULL;

ALTER TABLE usage_log ADD COLUMN IF NOT EXISTS environment VARCHAR(20) NULL CHECK (environment IS NULL OR environment IN ('development', 'staging', 'production'));

CREATE TABLE IF NOT EXISTS agent_version (
    id INT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    version_label VARCHAR(50) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT uq_agent_version UNIQUE (agent_id, version_label),
    CONSTRAINT fk_version_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS agent_environment (
    agent_id INT NOT NULL,
    environment VARCHAR(20) NOT NULL,
    version_label VARCHAR(50) NOT NULL,
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT pk_agent_environment PRIMARY KEY (agent_id, environment),
    CONSTRAINT fk_env_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT ck_env_name CHECK (environment IN ('development', 'staging', 'production'))
);

CREATE TABLE IF NOT EXISTS agent_deployment (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    action VARCHAR(10) NOT NULL,
    environment VARCHAR(20) NOT NULL,
    from_environment VARCHAR(20) NULL,
    version_label VARCHAR(50) NOT NULL,
    actor VARCHAR(150) NOT NULL DEFAULT 'api',
    note VARCHAR(500) NULL,
    at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT fk_deploy_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT ck_deploy_action CHECK (action IN ('deploy', 'promote')),
    CONSTRAINT ck_deploy_env CHECK (environment IN ('development', 'staging', 'production'))
);

CREATE INDEX IF NOT EXISTS ix_usage_agent_env_time ON usage_log (agent_id, environment, called_at);
CREATE INDEX IF NOT EXISTS ix_deployment_agent_time ON agent_deployment (agent_id, at);

ALTER TABLE audit ADD COLUMN IF NOT EXISTS prev_hash BINARY(32) NULL, ADD COLUMN IF NOT EXISTS hash BINARY(32) NULL;

ALTER TABLE access_event ADD COLUMN IF NOT EXISTS prev_hash BINARY(32) NULL, ADD COLUMN IF NOT EXISTS hash BINARY(32) NULL;

CREATE TABLE IF NOT EXISTS agent_evaluation (
    id INT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    version_label VARCHAR(50) NOT NULL,
    result VARCHAR(10) NOT NULL,
    evaluator VARCHAR(150) NOT NULL DEFAULT 'api',
    note VARCHAR(500) NULL,
    at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT fk_eval_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT ck_eval_result CHECK (result IN ('passed', 'failed'))
);

CREATE INDEX IF NOT EXISTS ix_eval_agent ON agent_evaluation (agent_id, id);

ALTER TABLE agent_access ADD COLUMN IF NOT EXISTS requires_approval BOOLEAN NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS integration_approval (
    id INT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    resource_id INT NOT NULL,
    status VARCHAR(10) NOT NULL DEFAULT 'pending',
    requested_by VARCHAR(150) NOT NULL DEFAULT 'api',
    justification VARCHAR(500) NULL,
    decided_by VARCHAR(150) NULL,
    decision_note VARCHAR(500) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    decided_at DATETIME(3) NULL,
    used_at DATETIME(3) NULL,
    CONSTRAINT fk_approval_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT fk_approval_resource FOREIGN KEY (resource_id) REFERENCES integration_resource (id) ON DELETE CASCADE,
    CONSTRAINT ck_approval_status CHECK (status IN ('pending', 'approved', 'rejected', 'used'))
);

CREATE INDEX IF NOT EXISTS ix_approval_pair ON integration_approval (agent_id, resource_id, status);

ALTER TABLE agent ADD COLUMN IF NOT EXISTS interacts_with_people BOOLEAN NOT NULL DEFAULT 0;

ALTER TABLE agent ADD COLUMN IF NOT EXISTS interaction_language VARCHAR(35) NULL;

CREATE TABLE IF NOT EXISTS resource_security (
    resource_id INT NOT NULL PRIMARY KEY,
    classification VARCHAR(12) NOT NULL DEFAULT 'internal',
    encrypted_at_rest BOOLEAN NOT NULL DEFAULT 0,
    encrypted_in_transit BOOLEAN NOT NULL DEFAULT 0,
    pii_allowed BOOLEAN NOT NULL DEFAULT 0,
    retention_days INT NULL,
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT fk_security_resource FOREIGN KEY (resource_id) REFERENCES integration_resource (id) ON DELETE CASCADE,
    CONSTRAINT ck_security_class CHECK (classification IN ('public', 'internal', 'confidential', 'restricted')),
    CONSTRAINT ck_security_retention CHECK (retention_days IS NULL OR retention_days >= 0)
);

CREATE TABLE IF NOT EXISTS compliance_policy (
    id INT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(150) NOT NULL,
    description VARCHAR(1000) NULL,
    rule_type VARCHAR(40) NOT NULL,
    params LONGTEXT NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT uq_policy_name UNIQUE (name),
    CONSTRAINT ck_policy_type CHECK (rule_type IN ('transparency_required', 'evaluation_before_production',
                                                               'max_classification', 'pii_requires_approval')),
    CONSTRAINT ck_policy_params CHECK (JSON_VALID(params) = 1)
);

CREATE TABLE IF NOT EXISTS compliance_framework (
    id INT AUTO_INCREMENT PRIMARY KEY,
    code VARCHAR(40) NOT NULL,
    name VARCHAR(150) NOT NULL,
    version VARCHAR(50) NULL,
    source_url VARCHAR(300) NULL,
    validation_note VARCHAR(300) NULL,
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT uq_framework_code UNIQUE (code)
);

CREATE TABLE IF NOT EXISTS compliance_control (
    id INT AUTO_INCREMENT PRIMARY KEY,
    framework_id INT NOT NULL,
    code VARCHAR(40) NOT NULL,
    title VARCHAR(200) NOT NULL,
    description VARCHAR(1000) NULL,
    CONSTRAINT uq_control_code UNIQUE (framework_id, code),
    CONSTRAINT fk_control_framework FOREIGN KEY (framework_id) REFERENCES compliance_framework (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS agent_control_assessment (
    agent_id INT NOT NULL,
    control_id INT NOT NULL,
    status VARCHAR(16) NOT NULL,
    evidence_note VARCHAR(1000) NULL,
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT pk_agent_control_assessment PRIMARY KEY (agent_id, control_id),
    CONSTRAINT fk_assess_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT fk_assess_control FOREIGN KEY (control_id) REFERENCES compliance_control (id) ON DELETE CASCADE,
    CONSTRAINT ck_assess_status CHECK (status IN ('compliant', 'non_compliant', 'not_assessed', 'not_applicable'))
);

-- Dati iniziali: da validare dal responsabile conformita'.
-- Titoli di sintesi, non il testo normativo: il responsabile conformita' li completa e li verifica.
INSERT IGNORE INTO compliance_framework (code, name, version, source_url, validation_note) VALUES
    ('GDPR', 'Regolamento generale sulla protezione dei dati (UE 2016/679)', '2016/679', 'https://eur-lex.europa.eu/eli/reg/2016/679/oj', 'da validare dal responsabile conformita'''),
    ('AI_ACT', 'Regolamento sull''intelligenza artificiale (UE 2024/1689)', '2024/1689', 'https://eur-lex.europa.eu/eli/reg/2024/1689/oj', 'da validare dal responsabile conformita'''),
    ('ISO_27001', 'ISO/IEC 27001 - sicurezza delle informazioni', '2022', 'https://www.iso.org/standard/27001', 'da validare dal responsabile conformita'''),
    ('ISO_42001', 'ISO/IEC 42001 - sistemi di gestione dell''IA', '2023', 'https://www.iso.org/standard/42001', 'da validare dal responsabile conformita''');

-- Controlli iniziali (da validare dal responsabile conformita')
INSERT IGNORE INTO compliance_control (framework_id, code, title, description)
SELECT f.id, c.code, c.title, c.description
  FROM (
        SELECT 'GDPR' AS fcode, 'ART5' AS code, 'Principi del trattamento' AS title, '[da validare] Liceita'', minimizzazione e limitazione della conservazione dei dati personali.' AS description
    UNION ALL SELECT 'GDPR' AS fcode, 'ART25' AS code, 'Protezione dei dati fin dalla progettazione' AS title, '[da validare] Misure tecniche e organizzative adeguate fin dalla progettazione.' AS description
    UNION ALL SELECT 'GDPR' AS fcode, 'ART32' AS code, 'Sicurezza del trattamento' AS title, '[da validare] Cifratura e misure di sicurezza proporzionate al rischio.' AS description
    UNION ALL SELECT 'AI_ACT' AS fcode, 'ART50' AS code, 'Trasparenza nell''interazione con le persone' AS title, '[da validare] Informare le persone che interagiscono con un sistema di IA.' AS description
    UNION ALL SELECT 'AI_ACT' AS fcode, 'ART9' AS code, 'Sistema di gestione dei rischi' AS title, '[da validare] Identificazione e gestione dei rischi lungo il ciclo di vita.' AS description
    UNION ALL SELECT 'AI_ACT' AS fcode, 'ART12' AS code, 'Registrazione degli eventi' AS title, '[da validare] Conservazione di registri adeguati per la tracciabilita''.' AS description
    UNION ALL SELECT 'ISO_27001' AS fcode, 'A.5.12' AS code, 'Classificazione delle informazioni' AS title, '[da validare] Classificare le informazioni in base al livello di riservatezza.' AS description
    UNION ALL SELECT 'ISO_27001' AS fcode, 'A.8.24' AS code, 'Uso della crittografia' AS title, '[da validare] Regole per l''uso della crittografia a riposo e in transito.' AS description
    UNION ALL SELECT 'ISO_42001' AS fcode, '6.1' AS code, 'Azioni per affrontare rischi e opportunita''' AS title, '[da validare] Valutazione dei rischi dei sistemi di IA.' AS description
    UNION ALL SELECT 'ISO_42001' AS fcode, '8.4' AS code, 'Valutazione dell''impatto dei sistemi di IA' AS title, '[da validare] Valutazione dell''impatto sugli utenti e sulle persone.' AS description
  ) AS c
  JOIN compliance_framework f ON f.code = c.fcode;

CREATE TABLE IF NOT EXISTS guardrail (
    id INT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(150) NOT NULL,
    type VARCHAR(30) NOT NULL,
    params LONGTEXT NOT NULL DEFAULT '{}',
    scope VARCHAR(10) NOT NULL,
    agent_id INT NULL,
    resource_id INT NULL,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT ck_guardrail_scope_target CHECK (
        (scope = 'global'   AND agent_id IS NULL     AND resource_id IS NULL) OR
        (scope = 'agent'    AND agent_id IS NOT NULL AND resource_id IS NULL) OR
        (scope = 'resource' AND agent_id IS NULL     AND resource_id IS NOT NULL)),
    CONSTRAINT ck_guardrail_type CHECK (type IN ('pii_mask', 'pii_block', 'keyword_block',
                                                             'max_length', 'rate_limit_per_minute', 'prompt_injection')),
    CONSTRAINT ck_guardrail_params CHECK (JSON_VALID(params) = 1),
    CONSTRAINT ck_guardrail_scope CHECK (scope IN ('global', 'agent', 'resource')),
    CONSTRAINT fk_guardrail_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT fk_guardrail_resource FOREIGN KEY (resource_id) REFERENCES integration_resource (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS guardrail_event (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    agent_id INT NOT NULL,
    resource_id INT NULL,
    direction VARCHAR(6) NOT NULL,
    decision VARCHAR(6) NOT NULL,
    violations LONGTEXT NOT NULL DEFAULT '[]',
    text_length INT NOT NULL,
    text_sha256 CHAR(64) NOT NULL,
    CONSTRAINT ck_gevent_direction CHECK (direction IN ('input', 'output')),
    CONSTRAINT ck_gevent_decision CHECK (decision IN ('allow', 'mask', 'block')),
    CONSTRAINT ck_gevent_violations CHECK (JSON_VALID(violations) = 1)
);

CREATE INDEX IF NOT EXISTS ix_gevent_agent_time ON guardrail_event (agent_id, at);
CREATE INDEX IF NOT EXISTS ix_gevent_time ON guardrail_event (at);

CREATE TABLE IF NOT EXISTS eval_trace (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    environment VARCHAR(20) NULL,
    trace_id VARCHAR(100) NOT NULL,
    outcome VARCHAR(10) NOT NULL,
    goal_met BOOLEAN NOT NULL DEFAULT 0,
    cost DECIMAL(18,4) NOT NULL DEFAULT 0,
    total_tokens INT NOT NULL DEFAULT 0,
    total_latency_ms INT NOT NULL DEFAULT 0,
    step_errors INT NOT NULL DEFAULT 0,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT uq_trace_agent UNIQUE (agent_id, trace_id),
    CONSTRAINT fk_trace_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT ck_trace_env CHECK (environment IS NULL OR environment IN ('development', 'staging', 'production')),
    CONSTRAINT ck_trace_outcome CHECK (outcome IN ('success', 'failure', 'partial'))
);

CREATE TABLE IF NOT EXISTS eval_trace_step (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    trace_ref BIGINT NOT NULL,
    step_type VARCHAR(10) NOT NULL,
    name VARCHAR(150) NOT NULL,
    tokens INT NOT NULL DEFAULT 0,
    latency_ms INT NOT NULL DEFAULT 0,
    ok BOOLEAN NOT NULL DEFAULT 1,
    memory_age_days INT NULL,
    CONSTRAINT fk_step_trace FOREIGN KEY (trace_ref) REFERENCES eval_trace (id) ON DELETE CASCADE,
    CONSTRAINT ck_step_type CHECK (step_type IN ('llm', 'tool', 'reasoning', 'memory'))
);

CREATE TABLE IF NOT EXISTS agent_reference_step (
    agent_id INT NOT NULL,
    position INT NOT NULL,
    step_type VARCHAR(10) NOT NULL,
    name VARCHAR(150) NOT NULL,
    CONSTRAINT pk_agent_reference_step PRIMARY KEY (agent_id, position),
    CONSTRAINT fk_refstep_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT ck_refstep_type CHECK (step_type IN ('llm', 'tool', 'reasoning', 'memory'))
);

CREATE TABLE IF NOT EXISTS eval_rule (
    id INT AUTO_INCREMENT PRIMARY KEY,
    type VARCHAR(30) NOT NULL,
    params LONGTEXT NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT uq_eval_rule_type UNIQUE (type),
    CONSTRAINT ck_eval_rule_type CHECK (type IN ('max_tokens_per_trace', 'max_latency_ms', 'max_error_rate',
                                                             'drift_threshold', 'memory_stale_days')),
    CONSTRAINT ck_eval_rule_params CHECK (JSON_VALID(params) = 1)
);

CREATE TABLE IF NOT EXISTS eval_violation (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    rule_type VARCHAR(30) NOT NULL,
    trace_ref BIGINT NULL,
    detail VARCHAR(500) NOT NULL,
    at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3))
);

CREATE TABLE IF NOT EXISTS eval_score (
    id INT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    score TINYINT NOT NULL,
    judge VARCHAR(100) NOT NULL,
    note VARCHAR(500) NULL,
    at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT fk_score_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT ck_score_range CHECK (score BETWEEN 1 AND 5)
);

CREATE INDEX IF NOT EXISTS ix_trace_agent_time ON eval_trace (agent_id, id);

CREATE TABLE IF NOT EXISTS marketplace_listing (
    id INT AUTO_INCREMENT PRIMARY KEY,
    provider VARCHAR(150) NOT NULL,
    name VARCHAR(150) NOT NULL,
    version VARCHAR(50) NULL,
    framework VARCHAR(100) NULL,
    price_model VARCHAR(12) NOT NULL,
    price DECIMAL(18,4) NOT NULL DEFAULT 0,
    currency CHAR(3) NOT NULL DEFAULT 'EUR',
    certified BOOLEAN NOT NULL DEFAULT 0,
    security_review VARCHAR(10) NOT NULL DEFAULT 'pending',
    status VARCHAR(8) NOT NULL DEFAULT 'hidden',
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    updated_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT ck_listing_price_model CHECK (price_model IN ('free', 'subscription', 'per_use')),
    CONSTRAINT ck_listing_price CHECK (price >= 0),
    CONSTRAINT ck_listing_currency CHECK (currency IN ('EUR', 'USD')),
    CONSTRAINT ck_listing_review CHECK (security_review IN ('pending', 'approved', 'rejected')),
    CONSTRAINT ck_listing_status CHECK (status IN ('listed', 'hidden'))
);

CREATE TABLE IF NOT EXISTS marketplace_request (
    id INT AUTO_INCREMENT PRIMARY KEY,
    listing_id INT NOT NULL,
    requested_by VARCHAR(150) NOT NULL,
    justification VARCHAR(500) NULL,
    status VARCHAR(12) NOT NULL DEFAULT 'pending',
    decided_by VARCHAR(150) NULL,
    decision_note VARCHAR(500) NULL,
    agent_id INT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    decided_at DATETIME(3) NULL,
    provisioned_at DATETIME(3) NULL,
    CONSTRAINT fk_request_listing FOREIGN KEY (listing_id) REFERENCES marketplace_listing (id) ON DELETE CASCADE,
    CONSTRAINT ck_request_status CHECK (status IN ('pending', 'approved', 'rejected', 'provisioned'))
);

CREATE INDEX IF NOT EXISTS ix_request_listing ON marketplace_request (listing_id, status);

ALTER TABLE agent ADD COLUMN IF NOT EXISTS framework VARCHAR(100) NULL;

CREATE TABLE IF NOT EXISTS finops_outcome (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    outcome VARCHAR(10) NOT NULL,
    value_eur DECIMAL(18,4) NOT NULL DEFAULT 0,
    period DATE NOT NULL,
    note VARCHAR(500) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT fk_outcome_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT ck_outcome_value CHECK (outcome IN ('success', 'failure', 'partial')),
    CONSTRAINT ck_outcome_value_eur CHECK (value_eur >= 0)
);

CREATE INDEX IF NOT EXISTS ix_outcome_agent_period ON finops_outcome (agent_id, period);

ALTER TABLE agent ADD COLUMN IF NOT EXISTS identity_id CHAR(36) NOT NULL DEFAULT (UUID());

ALTER TABLE agent ADD COLUMN IF NOT EXISTS identity_revoked_at DATETIME(3) NULL;

ALTER TABLE eval_violation ADD COLUMN IF NOT EXISTS params LONGTEXT NOT NULL DEFAULT '{}' CHECK (JSON_VALID(params) = 1);

CREATE TABLE IF NOT EXISTS app_user (
    id INT AUTO_INCREMENT PRIMARY KEY,
    username VARCHAR(50) NOT NULL,
    password_hash VARCHAR(100) NOT NULL,
    role VARCHAR(20) NOT NULL,
    active BOOLEAN NOT NULL DEFAULT 1,
    must_change_password BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    last_login_at DATETIME(3) NULL,
    CONSTRAINT uq_app_user_name UNIQUE (username),
    CONSTRAINT ck_app_user_role CHECK (role IN ('admin', 'manager', 'operator', 'auditor', 'viewer', 'integration'))
);

CREATE TABLE IF NOT EXISTS login_session (
    token_hash CHAR(64) NOT NULL PRIMARY KEY,
    user_id INT NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    expires_at DATETIME(3) NOT NULL,
    CONSTRAINT fk_session_user FOREIGN KEY (user_id) REFERENCES app_user (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS login_attempt (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    username VARCHAR(50) NOT NULL,
    success BOOLEAN NOT NULL,
    at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3))
);

ALTER TABLE login_attempt ADD COLUMN IF NOT EXISTS ip VARCHAR(45) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS ix_attempt_user_time ON login_attempt (username, at);
CREATE INDEX IF NOT EXISTS ix_attempt_ip_time ON login_attempt (ip, at);
CREATE INDEX IF NOT EXISTS ix_attempt_at ON login_attempt (at);
CREATE INDEX IF NOT EXISTS ix_session_expires ON login_session (expires_at);

ALTER TABLE integration_approval ADD COLUMN IF NOT EXISTS tool_id INT NULL;

CREATE TABLE IF NOT EXISTS mcp_tool (
    id INT AUTO_INCREMENT PRIMARY KEY,
    resource_id INT NOT NULL,
    name VARCHAR(150) NOT NULL,
    description VARCHAR(1000) NULL,
    risk VARCHAR(10) NOT NULL DEFAULT 'low',
    requires_approval BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT uq_tool_name UNIQUE (resource_id, name),
    CONSTRAINT fk_tool_resource FOREIGN KEY (resource_id) REFERENCES integration_resource (id) ON DELETE CASCADE,
    CONSTRAINT ck_tool_risk CHECK (risk IN ('low', 'medium', 'high'))
);

CREATE TABLE IF NOT EXISTS tool_access (
    id INT AUTO_INCREMENT PRIMARY KEY,
    agent_id INT NOT NULL,
    tool_id INT NOT NULL,
    requires_approval BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    CONSTRAINT uq_tool_access UNIQUE (agent_id, tool_id),
    CONSTRAINT fk_toolaccess_agent FOREIGN KEY (agent_id) REFERENCES agent (id) ON DELETE CASCADE,
    CONSTRAINT fk_toolaccess_tool FOREIGN KEY (tool_id) REFERENCES mcp_tool (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS gateway_event (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    at DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
    agent_id INT NOT NULL,
    resource_id INT NOT NULL,
    tool_name VARCHAR(150) NULL,
    decision VARCHAR(10) NOT NULL,
    reason VARCHAR(50) NOT NULL,
    args_length INT NOT NULL DEFAULT 0,
    args_sha256 CHAR(64) NULL,
    result_length INT NOT NULL DEFAULT 0,
    result_sha256 CHAR(64) NULL,
    latency_ms INT NOT NULL DEFAULT 0,
    CONSTRAINT ck_gw_decision CHECK (decision IN ('allowed', 'denied', 'blocked', 'error'))
);

-- Blocchi applicativi per la durata di una transazione (sostituiscono sp_getapplock)
CREATE TABLE IF NOT EXISTS app_lock (
    name VARCHAR(100) NOT NULL PRIMARY KEY
);
