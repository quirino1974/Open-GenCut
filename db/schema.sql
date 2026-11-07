-- ============================================================================
-- Open-GenCut v0.1 - Registro unificato degli agenti (punto 1)
-- Database: geniuscut su SQL Server 2022 (container mon_mssql_express)
-- Allineato al contratto docs/API.txt. Script idempotente.
-- Autore: Quirino Scaramastra
-- ============================================================================

IF DB_ID(N'geniuscut') IS NULL
    CREATE DATABASE geniuscut;
GO

USE geniuscut;
GO

-- ----------------------------------------------------------------------------
-- Agenti
--   origin: internal | marketplace | third_party
--   status: registered | active | suspended | retired
--   currency: EUR | USD
-- ----------------------------------------------------------------------------
IF OBJECT_ID(N'dbo.agent', N'U') IS NULL
CREATE TABLE dbo.agent (
    id            INT            IDENTITY(1,1) PRIMARY KEY,
    name          NVARCHAR(150)  NOT NULL,
    description   NVARCHAR(1000) NULL,
    origin        VARCHAR(20)    NOT NULL
                  CONSTRAINT ck_agent_origin CHECK (origin IN ('internal', 'marketplace', 'third_party')),
    vendor        NVARCHAR(150)  NULL,
    version       NVARCHAR(50)   NULL,
    owner         NVARCHAR(150)  NULL,
    status        VARCHAR(20)    NOT NULL DEFAULT 'registered'
                  CONSTRAINT ck_agent_status CHECK (status IN ('registered', 'active', 'suspended', 'retired')),
    model         NVARCHAR(150)  NULL,
    endpoint      NVARCHAR(300)  NULL,
    cost_monthly  DECIMAL(18,4)  NOT NULL DEFAULT 0
                  CONSTRAINT ck_agent_cost CHECK (cost_monthly >= 0),
    currency      CHAR(3)        NOT NULL DEFAULT 'EUR'
                  CONSTRAINT ck_agent_currency CHECK (currency IN ('EUR', 'USD')),
    created_at    DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    updated_at    DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME()
);
GO

-- ----------------------------------------------------------------------------
-- Configurazioni
--   scope: global (agent_id NULL) | agent (agent_id obbligatorio)
--   key: unica nell'intero registro
-- ----------------------------------------------------------------------------
IF OBJECT_ID(N'dbo.configuration', N'U') IS NULL
CREATE TABLE dbo.configuration (
    id            INT            IDENTITY(1,1) PRIMARY KEY,
    [key]         NVARCHAR(150)  NOT NULL CONSTRAINT uq_configuration_key UNIQUE,
    value         NVARCHAR(MAX)  NOT NULL DEFAULT N'',
    scope         VARCHAR(10)    NOT NULL
                  CONSTRAINT ck_configuration_scope CHECK (scope IN ('global', 'agent')),
    agent_id      INT            NULL
                  CONSTRAINT fk_configuration_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    description   NVARCHAR(1000) NULL,
    created_at    DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    updated_at    DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT ck_configuration_scope_agent CHECK (
        (scope = 'global' AND agent_id IS NULL) OR
        (scope = 'agent'  AND agent_id IS NOT NULL)
    )
);
GO

-- ----------------------------------------------------------------------------
-- Audit: una riga per ogni scrittura (contratto: "Ogni scrittura produce una riga in audit")
-- ----------------------------------------------------------------------------
IF OBJECT_ID(N'dbo.audit', N'U') IS NULL
CREATE TABLE dbo.audit (
    id            BIGINT         IDENTITY(1,1) PRIMARY KEY,
    at            DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    actor         NVARCHAR(150)  NOT NULL DEFAULT N'api',
    action        VARCHAR(20)    NOT NULL,          -- create | update | delete
    entity        VARCHAR(30)    NOT NULL,          -- agent | configuration
    entity_id     INT            NULL,              -- id dell'entità; nessuna FK, la riga resta anche dopo la cancellazione
    detail        NVARCHAR(1000) NULL
);
GO

-- ----------------------------------------------------------------------------
-- Log di utilizzo: una riga per invocazione dell'agente (alimenta /api/metrics/usage)
-- ----------------------------------------------------------------------------
IF OBJECT_ID(N'dbo.usage_log', N'U') IS NULL
CREATE TABLE dbo.usage_log (
    id            BIGINT         IDENTITY(1,1) PRIMARY KEY,
    agent_id      INT            NOT NULL
                  CONSTRAINT fk_usage_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    called_at     DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    tokens        INT            NOT NULL DEFAULT 0,
    cost          DECIMAL(18,4)  NOT NULL DEFAULT 0
);
GO

-- ----------------------------------------------------------------------------
-- Indici
-- ----------------------------------------------------------------------------
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_agent_origin_status' AND object_id = OBJECT_ID(N'dbo.agent'))
    CREATE INDEX ix_agent_origin_status ON dbo.agent (origin, status);
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_agent_owner' AND object_id = OBJECT_ID(N'dbo.agent'))
    CREATE INDEX ix_agent_owner ON dbo.agent (owner);
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_configuration_agent' AND object_id = OBJECT_ID(N'dbo.configuration'))
    CREATE INDEX ix_configuration_agent ON dbo.configuration (agent_id);
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_audit_at' AND object_id = OBJECT_ID(N'dbo.audit'))
    CREATE INDEX ix_audit_at ON dbo.audit (at);
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_usage_agent_time' AND object_id = OBJECT_ID(N'dbo.usage_log'))
    CREATE INDEX ix_usage_agent_time ON dbo.usage_log (agent_id, called_at);
GO

-- ============================================================================
-- Punto 2 - Controllo accessi degli agenti verso risorse esterne
--   (modelli LLM, API, server MCP, fonti dati)
-- Tabelle aggiuntive: non modificano quelle del punto 1.
-- ============================================================================

-- Risorse esterne censite nel registro
--   kind: llm_model | api | mcp_server | data_source
--   status: enabled | disabled
IF OBJECT_ID(N'dbo.integration_resource', N'U') IS NULL
CREATE TABLE dbo.integration_resource (
    id            INT            IDENTITY(1,1) PRIMARY KEY,
    kind          VARCHAR(20)    NOT NULL
                  CONSTRAINT ck_resource_kind CHECK (kind IN ('llm_model', 'api', 'mcp_server', 'data_source')),
    name          NVARCHAR(150)  NOT NULL,
    endpoint      NVARCHAR(300)  NULL,
    description   NVARCHAR(1000) NULL,
    status        VARCHAR(10)    NOT NULL DEFAULT 'enabled'
                  CONSTRAINT ck_resource_status CHECK (status IN ('enabled', 'disabled')),
    created_at    DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    updated_at    DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT uq_resource_kind_name UNIQUE (kind, name)
);
GO

-- Allowlist: quale agente può usare quale risorsa (una riga = permesso)
IF OBJECT_ID(N'dbo.agent_access', N'U') IS NULL
CREATE TABLE dbo.agent_access (
    id            INT            IDENTITY(1,1) PRIMARY KEY,
    agent_id      INT            NOT NULL
                  CONSTRAINT fk_access_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    resource_id   INT            NOT NULL
                  CONSTRAINT fk_access_resource REFERENCES dbo.integration_resource (id) ON DELETE CASCADE,
    created_at    DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT uq_agent_access UNIQUE (agent_id, resource_id)
);
GO

-- Limiti per coppia agente-risorsa (NULL = nessun limite su quella voce)
IF OBJECT_ID(N'dbo.access_limit', N'U') IS NULL
CREATE TABLE dbo.access_limit (
    id                 INT           IDENTITY(1,1) PRIMARY KEY,
    agent_id           INT           NOT NULL
                       CONSTRAINT fk_limit_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    resource_id        INT           NOT NULL
                       CONSTRAINT fk_limit_resource REFERENCES dbo.integration_resource (id) ON DELETE CASCADE,
    max_calls_per_day  INT           NULL
                       CONSTRAINT ck_limit_calls CHECK (max_calls_per_day IS NULL OR max_calls_per_day > 0),
    max_cost_per_month DECIMAL(18,4) NULL
                       CONSTRAINT ck_limit_cost CHECK (max_cost_per_month IS NULL OR max_cost_per_month >= 0),
    currency           CHAR(3)       NOT NULL DEFAULT 'EUR'
                       CONSTRAINT ck_limit_currency CHECK (currency IN ('EUR', 'USD')),
    CONSTRAINT uq_limit_pair UNIQUE (agent_id, resource_id)
);
GO

-- Registro degli accessi (decisioni allowed / denied).
-- Nessuna FK: la storia degli accessi resta anche dopo la cancellazione di agente o risorsa.
IF OBJECT_ID(N'dbo.access_event', N'U') IS NULL
CREATE TABLE dbo.access_event (
    id            BIGINT         IDENTITY(1,1) PRIMARY KEY,
    at            DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    agent_id      INT            NOT NULL,
    resource_id   INT            NOT NULL,
    decision      VARCHAR(10)    NOT NULL
                  CONSTRAINT ck_event_decision CHECK (decision IN ('allowed', 'denied')),
    reason        VARCHAR(50)    NOT NULL,
    cost          DECIMAL(18,4)  NOT NULL DEFAULT 0
);
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_access_event_pair' AND object_id = OBJECT_ID(N'dbo.access_event'))
    CREATE INDEX ix_access_event_pair ON dbo.access_event (agent_id, resource_id, at);
GO

-- ============================================================================
-- Punto 3 - Ciclo di vita: versioni, ambienti, deploy, promozioni, monitoraggio
-- Modifiche additive: colonne nuove su usage_log (con default) e tabelle nuove.
-- ============================================================================

-- Monitoraggio: esito, latenza e ambiente di ogni chiamata registrata
IF COL_LENGTH(N'dbo.usage_log', N'success') IS NULL
    ALTER TABLE dbo.usage_log ADD success BIT NOT NULL CONSTRAINT df_usage_success DEFAULT 1;
GO
IF COL_LENGTH(N'dbo.usage_log', N'latency_ms') IS NULL
    ALTER TABLE dbo.usage_log ADD latency_ms INT NULL;
GO
IF COL_LENGTH(N'dbo.usage_log', N'environment') IS NULL
    ALTER TABLE dbo.usage_log ADD environment VARCHAR(20) NULL
        CONSTRAINT ck_usage_environment CHECK (environment IS NULL OR environment IN ('development', 'staging', 'production'));
GO

-- Versioni registrate per agente
IF OBJECT_ID(N'dbo.agent_version', N'U') IS NULL
CREATE TABLE dbo.agent_version (
    id             INT            IDENTITY(1,1) PRIMARY KEY,
    agent_id       INT            NOT NULL
                   CONSTRAINT fk_version_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    version_label  NVARCHAR(50)   NOT NULL,
    created_at     DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT uq_agent_version UNIQUE (agent_id, version_label)
);
GO

-- Versione attualmente in esecuzione in ciascun ambiente (una riga per agente e ambiente)
IF OBJECT_ID(N'dbo.agent_environment', N'U') IS NULL
CREATE TABLE dbo.agent_environment (
    agent_id       INT            NOT NULL
                   CONSTRAINT fk_env_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    environment    VARCHAR(20)    NOT NULL
                   CONSTRAINT ck_env_name CHECK (environment IN ('development', 'staging', 'production')),
    version_label  NVARCHAR(50)   NOT NULL,
    updated_at     DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_agent_environment PRIMARY KEY (agent_id, environment)
);
GO

-- Registro di deploy e promozioni (storia, mai modificata).
-- Nessuna FK su version_label: la riga resta leggibile anche se la versione viene rimossa.
IF OBJECT_ID(N'dbo.agent_deployment', N'U') IS NULL
CREATE TABLE dbo.agent_deployment (
    id               BIGINT         IDENTITY(1,1) PRIMARY KEY,
    agent_id         INT            NOT NULL
                     CONSTRAINT fk_deploy_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    action           VARCHAR(10)    NOT NULL
                     CONSTRAINT ck_deploy_action CHECK (action IN ('deploy', 'promote')),
    environment      VARCHAR(20)    NOT NULL
                     CONSTRAINT ck_deploy_env CHECK (environment IN ('development', 'staging', 'production')),
    from_environment VARCHAR(20)    NULL,
    version_label    NVARCHAR(50)   NOT NULL,
    actor            NVARCHAR(150)  NOT NULL DEFAULT N'api',
    note             NVARCHAR(500)  NULL,
    at               DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME()
);
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_usage_agent_env_time' AND object_id = OBJECT_ID(N'dbo.usage_log'))
    CREATE INDEX ix_usage_agent_env_time ON dbo.usage_log (agent_id, environment, called_at);
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_deployment_agent_time' AND object_id = OBJECT_ID(N'dbo.agent_deployment'))
    CREATE INDEX ix_deployment_agent_time ON dbo.agent_deployment (agent_id, at);
GO

-- ============================================================================
-- Punto 4 - R2: audit a catena con hash (prev_hash -> hash)
-- Le righe precedenti alla catena restano senza hash (legacy) e vengono contate a parte.
-- ============================================================================
IF COL_LENGTH(N'dbo.audit', N'hash') IS NULL
    ALTER TABLE dbo.audit ADD prev_hash BINARY(32) NULL, hash BINARY(32) NULL;
GO
IF COL_LENGTH(N'dbo.access_event', N'hash') IS NULL
    ALTER TABLE dbo.access_event ADD prev_hash BINARY(32) NULL, hash BINARY(32) NULL;
GO

-- ============================================================================
-- Punto 4 - R3: valutazioni prima di staging e produzione
-- ============================================================================
IF OBJECT_ID(N'dbo.agent_evaluation', N'U') IS NULL
CREATE TABLE dbo.agent_evaluation (
    id             INT            IDENTITY(1,1) PRIMARY KEY,
    agent_id       INT            NOT NULL
                   CONSTRAINT fk_eval_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    version_label  NVARCHAR(50)   NOT NULL,
    result         VARCHAR(10)    NOT NULL
                   CONSTRAINT ck_eval_result CHECK (result IN ('passed', 'failed')),
    evaluator      NVARCHAR(150)  NOT NULL DEFAULT N'api',
    note           NVARCHAR(500)  NULL,
    at             DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME()
);
GO
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_eval_agent' AND object_id = OBJECT_ID(N'dbo.agent_evaluation'))
    CREATE INDEX ix_eval_agent ON dbo.agent_evaluation (agent_id, id);
GO

-- ============================================================================
-- Punto 4 - R1: approvazione umana per azioni sensibili
-- Una coppia agente-risorsa marcata requires_approval richiede un'approvazione
-- registrata prima di ogni accesso. L'approvazione vale per un solo accesso.
-- ============================================================================
IF COL_LENGTH(N'dbo.agent_access', N'requires_approval') IS NULL
    ALTER TABLE dbo.agent_access ADD requires_approval BIT NOT NULL
        CONSTRAINT df_access_requires_approval DEFAULT 0;
GO

IF OBJECT_ID(N'dbo.integration_approval', N'U') IS NULL
CREATE TABLE dbo.integration_approval (
    id             INT            IDENTITY(1,1) PRIMARY KEY,
    agent_id       INT            NOT NULL
                   CONSTRAINT fk_approval_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    resource_id    INT            NOT NULL
                   CONSTRAINT fk_approval_resource REFERENCES dbo.integration_resource (id) ON DELETE CASCADE,
    status         VARCHAR(10)    NOT NULL DEFAULT 'pending'
                   CONSTRAINT ck_approval_status CHECK (status IN ('pending', 'approved', 'rejected', 'used')),
    requested_by   NVARCHAR(150)  NOT NULL DEFAULT N'api',
    justification  NVARCHAR(500)  NULL,
    decided_by     NVARCHAR(150)  NULL,
    decision_note  NVARCHAR(500)  NULL,
    created_at     DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    decided_at     DATETIME2(3)   NULL,
    used_at        DATETIME2(3)   NULL
);
GO
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_approval_pair' AND object_id = OBJECT_ID(N'dbo.integration_approval'))
    CREATE INDEX ix_approval_pair ON dbo.integration_approval (agent_id, resource_id, status);
GO

-- ============================================================================
-- Punto 4 - R4: dichiarazione di trasparenza sull'agente
-- interacts_with_people: l'agente interagisce con persone
-- interaction_language:  lingua in cui interagisce (tag tipo "it", "en-GB"), obbligatoria se interagisce
-- ============================================================================
IF COL_LENGTH(N'dbo.agent', N'interacts_with_people') IS NULL
    ALTER TABLE dbo.agent ADD interacts_with_people BIT NOT NULL
        CONSTRAINT df_agent_interacts DEFAULT 0;
GO
IF COL_LENGTH(N'dbo.agent', N'interaction_language') IS NULL
    ALTER TABLE dbo.agent ADD interaction_language VARCHAR(35) NULL;
GO

-- ============================================================================
-- Punto 4 - Conformità (specifica del progetto)
-- ============================================================================

-- Nuovo tipo di risorsa: gateway AI
IF NOT EXISTS (SELECT 1 FROM sys.check_constraints
               WHERE name = N'ck_resource_kind' AND definition LIKE N'%ai_gateway%')
BEGIN
    IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_resource_kind')
        ALTER TABLE dbo.integration_resource DROP CONSTRAINT ck_resource_kind;
    ALTER TABLE dbo.integration_resource ADD CONSTRAINT ck_resource_kind
        CHECK (kind IN ('llm_model', 'api', 'mcp_server', 'data_source', 'ai_gateway'));
END
GO

-- Misure di sicurezza dei dati per risorsa (una riga per risorsa; senza riga valgono i default)
IF OBJECT_ID(N'dbo.resource_security', N'U') IS NULL
CREATE TABLE dbo.resource_security (
    resource_id          INT          NOT NULL PRIMARY KEY
                         CONSTRAINT fk_security_resource REFERENCES dbo.integration_resource (id) ON DELETE CASCADE,
    classification       VARCHAR(12)  NOT NULL DEFAULT 'internal'
                         CONSTRAINT ck_security_class CHECK (classification IN ('public', 'internal', 'confidential', 'restricted')),
    encrypted_at_rest    BIT          NOT NULL DEFAULT 0,
    encrypted_in_transit BIT          NOT NULL DEFAULT 0,
    pii_allowed          BIT          NOT NULL DEFAULT 0,
    retention_days       INT          NULL
                         CONSTRAINT ck_security_retention CHECK (retention_days IS NULL OR retention_days >= 0),
    updated_at           DATETIME2(3) NOT NULL DEFAULT SYSUTCDATETIME()
);
GO

-- Politiche di governance
IF OBJECT_ID(N'dbo.compliance_policy', N'U') IS NULL
CREATE TABLE dbo.compliance_policy (
    id           INT           IDENTITY(1,1) PRIMARY KEY,
    name         NVARCHAR(150) NOT NULL CONSTRAINT uq_policy_name UNIQUE,
    description  NVARCHAR(1000) NULL,
    rule_type    VARCHAR(40)   NOT NULL
                 CONSTRAINT ck_policy_type CHECK (rule_type IN ('transparency_required', 'evaluation_before_production',
                                                               'max_classification', 'pii_requires_approval')),
    params       NVARCHAR(MAX) NOT NULL DEFAULT N'{}'
                 CONSTRAINT ck_policy_params CHECK (ISJSON(params) = 1),
    enabled      BIT           NOT NULL DEFAULT 1,
    created_at   DATETIME2(3)  NOT NULL DEFAULT SYSUTCDATETIME(),
    updated_at   DATETIME2(3)  NOT NULL DEFAULT SYSUTCDATETIME()
);
GO

-- Librerie di framework e controlli
IF OBJECT_ID(N'dbo.compliance_framework', N'U') IS NULL
CREATE TABLE dbo.compliance_framework (
    id               INT           IDENTITY(1,1) PRIMARY KEY,
    code             VARCHAR(40)   NOT NULL CONSTRAINT uq_framework_code UNIQUE,
    name             NVARCHAR(150) NOT NULL,
    version          NVARCHAR(50)  NULL,
    source_url       NVARCHAR(300) NULL,
    validation_note  NVARCHAR(300) NULL,   -- es. "da validare dal responsabile conformità"
    updated_at       DATETIME2(3)  NOT NULL DEFAULT SYSUTCDATETIME()
);
GO

IF OBJECT_ID(N'dbo.compliance_control', N'U') IS NULL
CREATE TABLE dbo.compliance_control (
    id            INT            IDENTITY(1,1) PRIMARY KEY,
    framework_id  INT            NOT NULL
                  CONSTRAINT fk_control_framework REFERENCES dbo.compliance_framework (id) ON DELETE CASCADE,
    code          VARCHAR(40)    NOT NULL,
    title         NVARCHAR(200)  NOT NULL,
    description   NVARCHAR(1000) NULL,
    CONSTRAINT uq_control_code UNIQUE (framework_id, code)
);
GO

-- Valutazione dei controlli per agente
IF OBJECT_ID(N'dbo.agent_control_assessment', N'U') IS NULL
CREATE TABLE dbo.agent_control_assessment (
    agent_id      INT            NOT NULL
                  CONSTRAINT fk_assess_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    control_id    INT            NOT NULL
                  CONSTRAINT fk_assess_control REFERENCES dbo.compliance_control (id) ON DELETE CASCADE,
    status        VARCHAR(16)    NOT NULL
                  CONSTRAINT ck_assess_status CHECK (status IN ('compliant', 'non_compliant', 'not_assessed', 'not_applicable')),
    evidence_note NVARCHAR(1000) NULL,
    updated_at    DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_agent_control_assessment PRIMARY KEY (agent_id, control_id)
);
GO

-- Dati iniziali: da validare dal responsabile conformità.
-- Titoli di sintesi, non il testo normativo: il responsabile conformità li completa e li verifica.
MERGE dbo.compliance_framework AS t
USING (VALUES
    ('GDPR', N'Regolamento generale sulla protezione dei dati (UE 2016/679)', N'2016/679', N'https://eur-lex.europa.eu/eli/reg/2016/679/oj', N'da validare dal responsabile conformità'),
    ('AI_ACT', N'Regolamento sull''intelligenza artificiale (UE 2024/1689)', N'2024/1689', N'https://eur-lex.europa.eu/eli/reg/2024/1689/oj', N'da validare dal responsabile conformità'),
    ('ISO_27001', N'ISO/IEC 27001 - sicurezza delle informazioni', N'2022', N'https://www.iso.org/standard/27001', N'da validare dal responsabile conformità'),
    ('ISO_42001', N'ISO/IEC 42001 - sistemi di gestione dell''IA', N'2023', N'https://www.iso.org/standard/42001', N'da validare dal responsabile conformità')
) AS s (code, name, version, source_url, validation_note)
ON t.code = s.code
WHEN NOT MATCHED THEN INSERT (code, name, version, source_url, validation_note)
     VALUES (s.code, s.name, s.version, s.source_url, s.validation_note);
GO

MERGE dbo.compliance_control AS t
USING (
    SELECT f.id AS framework_id, c.code, c.title, c.description
      FROM (VALUES
        ('GDPR', 'ART5', N'Principi del trattamento', N'[da validare] Liceità, minimizzazione e limitazione della conservazione dei dati personali.'),
        ('GDPR', 'ART25', N'Protezione dei dati fin dalla progettazione', N'[da validare] Misure tecniche e organizzative adeguate fin dalla progettazione.'),
        ('GDPR', 'ART32', N'Sicurezza del trattamento', N'[da validare] Cifratura e misure di sicurezza proporzionate al rischio.'),
        ('AI_ACT', 'ART50', N'Trasparenza nell''interazione con le persone', N'[da validare] Informare le persone che interagiscono con un sistema di IA.'),
        ('AI_ACT', 'ART9', N'Sistema di gestione dei rischi', N'[da validare] Identificazione e gestione dei rischi lungo il ciclo di vita.'),
        ('AI_ACT', 'ART12', N'Registrazione degli eventi', N'[da validare] Conservazione di registri adeguati per la tracciabilità.'),
        ('ISO_27001', 'A.5.12', N'Classificazione delle informazioni', N'[da validare] Classificare le informazioni in base al livello di riservatezza.'),
        ('ISO_27001', 'A.8.24', N'Uso della crittografia', N'[da validare] Regole per l''uso della crittografia a riposo e in transito.'),
        ('ISO_42001', '6.1', N'Azioni per affrontare rischi e opportunità', N'[da validare] Valutazione dei rischi dei sistemi di IA.'),
        ('ISO_42001', '8.4', N'Valutazione dell''impatto dei sistemi di IA', N'[da validare] Valutazione dell''impatto sugli utenti e sulle persone.')
      ) AS c (fcode, code, title, description)
      JOIN dbo.compliance_framework f ON f.code = c.fcode
) AS s
ON t.framework_id = s.framework_id AND t.code = s.code
WHEN NOT MATCHED THEN INSERT (framework_id, code, title, description)
     VALUES (s.framework_id, s.code, s.title, s.description);
GO

-- ============================================================================
-- Punto 5 - Guardrail (specifica del progetto)
-- ============================================================================
IF OBJECT_ID(N'dbo.guardrail', N'U') IS NULL
CREATE TABLE dbo.guardrail (
    id           INT           IDENTITY(1,1) PRIMARY KEY,
    name         NVARCHAR(150) NOT NULL,
    type         VARCHAR(30)   NOT NULL
                 CONSTRAINT ck_guardrail_type CHECK (type IN ('pii_mask', 'pii_block', 'keyword_block',
                                                             'max_length', 'rate_limit_per_minute', 'prompt_injection')),
    params       NVARCHAR(MAX) NOT NULL DEFAULT N'{}'
                 CONSTRAINT ck_guardrail_params CHECK (ISJSON(params) = 1),
    scope        VARCHAR(10)   NOT NULL
                 CONSTRAINT ck_guardrail_scope CHECK (scope IN ('global', 'agent', 'resource')),
    agent_id     INT           NULL
                 CONSTRAINT fk_guardrail_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    resource_id  INT           NULL
                 CONSTRAINT fk_guardrail_resource REFERENCES dbo.integration_resource (id) ON DELETE CASCADE,
    enabled      BIT           NOT NULL DEFAULT 1,
    created_at   DATETIME2(3)  NOT NULL DEFAULT SYSUTCDATETIME(),
    updated_at   DATETIME2(3)  NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT ck_guardrail_scope_target CHECK (
        (scope = 'global'   AND agent_id IS NULL     AND resource_id IS NULL) OR
        (scope = 'agent'    AND agent_id IS NOT NULL AND resource_id IS NULL) OR
        (scope = 'resource' AND agent_id IS NULL     AND resource_id IS NOT NULL))
);
GO

-- Registro delle valutazioni: NESSUN testo. Solo lunghezza e hash SHA-256.
-- Nessuna FK: la storia resta anche dopo la cancellazione di agente o risorsa.
IF OBJECT_ID(N'dbo.guardrail_event', N'U') IS NULL
CREATE TABLE dbo.guardrail_event (
    id            BIGINT        IDENTITY(1,1) PRIMARY KEY,
    at            DATETIME2(3)  NOT NULL DEFAULT SYSUTCDATETIME(),
    agent_id      INT           NOT NULL,
    resource_id   INT           NULL,
    direction     VARCHAR(6)    NOT NULL CONSTRAINT ck_gevent_direction CHECK (direction IN ('input', 'output')),
    decision      VARCHAR(6)    NOT NULL CONSTRAINT ck_gevent_decision CHECK (decision IN ('allow', 'mask', 'block')),
    violations    NVARCHAR(MAX) NOT NULL DEFAULT N'[]' CONSTRAINT ck_gevent_violations CHECK (ISJSON(violations) = 1),
    text_length   INT           NOT NULL,
    text_sha256   CHAR(64)      NOT NULL
);
GO
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_gevent_agent_time' AND object_id = OBJECT_ID(N'dbo.guardrail_event'))
    CREATE INDEX ix_gevent_agent_time ON dbo.guardrail_event (agent_id, at);
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_gevent_time' AND object_id = OBJECT_ID(N'dbo.guardrail_event'))
    CREATE INDEX ix_gevent_time ON dbo.guardrail_event (at);
GO

-- ============================================================================
-- Punto 6 - Valutazione degli agenti
-- Tracce con passi: il testo dei passi non viene salvato (solo nome, tipo, token, latenza, esito).
-- ============================================================================
IF OBJECT_ID(N'dbo.eval_trace', N'U') IS NULL
CREATE TABLE dbo.eval_trace (
    id                BIGINT         IDENTITY(1,1) PRIMARY KEY,
    agent_id          INT            NOT NULL
                      CONSTRAINT fk_trace_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    environment       VARCHAR(20)    NULL
                      CONSTRAINT ck_trace_env CHECK (environment IS NULL OR environment IN ('development', 'staging', 'production')),
    trace_id          NVARCHAR(100)  NOT NULL,
    outcome           VARCHAR(10)    NOT NULL
                      CONSTRAINT ck_trace_outcome CHECK (outcome IN ('success', 'failure', 'partial')),
    goal_met          BIT            NOT NULL DEFAULT 0,
    cost              DECIMAL(18,4)  NOT NULL DEFAULT 0,
    total_tokens      INT            NOT NULL DEFAULT 0,
    total_latency_ms  INT            NOT NULL DEFAULT 0,
    step_errors       INT            NOT NULL DEFAULT 0,
    created_at        DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT uq_trace_agent UNIQUE (agent_id, trace_id)
);
GO
IF OBJECT_ID(N'dbo.eval_trace_step', N'U') IS NULL
CREATE TABLE dbo.eval_trace_step (
    id               BIGINT        IDENTITY(1,1) PRIMARY KEY,
    trace_ref        BIGINT        NOT NULL
                     CONSTRAINT fk_step_trace REFERENCES dbo.eval_trace (id) ON DELETE CASCADE,
    step_type        VARCHAR(10)   NOT NULL
                     CONSTRAINT ck_step_type CHECK (step_type IN ('llm', 'tool', 'reasoning', 'memory')),
    name             NVARCHAR(150) NOT NULL,
    tokens           INT           NOT NULL DEFAULT 0,
    latency_ms       INT           NOT NULL DEFAULT 0,
    ok               BIT           NOT NULL DEFAULT 1,
    memory_age_days  INT           NULL
);
GO
-- Sequenza di riferimento per agente (per le deviazioni dei ragionamenti)
IF OBJECT_ID(N'dbo.agent_reference_step', N'U') IS NULL
CREATE TABLE dbo.agent_reference_step (
    agent_id     INT           NOT NULL
                 CONSTRAINT fk_refstep_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    position     INT           NOT NULL,
    step_type    VARCHAR(10)   NOT NULL
                 CONSTRAINT ck_refstep_type CHECK (step_type IN ('llm', 'tool', 'reasoning', 'memory')),
    name         NVARCHAR(150) NOT NULL,
    CONSTRAINT pk_agent_reference_step PRIMARY KEY (agent_id, position)
);
GO

-- Regole di valutazione: una per tipo
IF OBJECT_ID(N'dbo.eval_rule', N'U') IS NULL
CREATE TABLE dbo.eval_rule (
    id          INT            IDENTITY(1,1) PRIMARY KEY,
    type        VARCHAR(30)    NOT NULL CONSTRAINT uq_eval_rule_type UNIQUE
                CONSTRAINT ck_eval_rule_type CHECK (type IN ('max_tokens_per_trace', 'max_latency_ms', 'max_error_rate',
                                                             'drift_threshold', 'memory_stale_days')),
    params      NVARCHAR(MAX)  NOT NULL DEFAULT N'{}' CONSTRAINT ck_eval_rule_params CHECK (ISJSON(params) = 1),
    enabled     BIT            NOT NULL DEFAULT 1,
    created_at  DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    updated_at  DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME()
);
GO
-- Violazioni: storia, senza FK (restano anche dopo la cancellazione)
IF OBJECT_ID(N'dbo.eval_violation', N'U') IS NULL
CREATE TABLE dbo.eval_violation (
    id          BIGINT         IDENTITY(1,1) PRIMARY KEY,
    agent_id    INT            NOT NULL,
    rule_type   VARCHAR(30)    NOT NULL,
    trace_ref   BIGINT         NULL,
    detail      NVARCHAR(500)  NOT NULL,
    at          DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME()
);
GO
-- Punteggi di giudizio (giudice esterno o manuale)
IF OBJECT_ID(N'dbo.eval_score', N'U') IS NULL
CREATE TABLE dbo.eval_score (
    id        INT            IDENTITY(1,1) PRIMARY KEY,
    agent_id  INT            NOT NULL
              CONSTRAINT fk_score_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    score     TINYINT        NOT NULL CONSTRAINT ck_score_range CHECK (score BETWEEN 1 AND 5),
    judge     NVARCHAR(100)  NOT NULL,
    note      NVARCHAR(500)  NULL,
    at        DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME()
);
GO
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_trace_agent_time' AND object_id = OBJECT_ID(N'dbo.eval_trace'))
    CREATE INDEX ix_trace_agent_time ON dbo.eval_trace (agent_id, id);
GO

-- ============================================================================
-- Punto 7 - Marketplace degli agenti
-- ============================================================================
IF OBJECT_ID(N'dbo.marketplace_listing', N'U') IS NULL
CREATE TABLE dbo.marketplace_listing (
    id               INT            IDENTITY(1,1) PRIMARY KEY,
    provider         NVARCHAR(150)  NOT NULL,
    name             NVARCHAR(150)  NOT NULL,
    version          NVARCHAR(50)   NULL,
    framework        NVARCHAR(100)  NULL,
    price_model      VARCHAR(12)    NOT NULL
                     CONSTRAINT ck_listing_price_model CHECK (price_model IN ('free', 'subscription', 'per_use')),
    price            DECIMAL(18,4)  NOT NULL DEFAULT 0
                     CONSTRAINT ck_listing_price CHECK (price >= 0),
    currency         CHAR(3)        NOT NULL DEFAULT 'EUR'
                     CONSTRAINT ck_listing_currency CHECK (currency IN ('EUR', 'USD')),
    certified        BIT            NOT NULL DEFAULT 0,
    security_review  VARCHAR(10)    NOT NULL DEFAULT 'pending'
                     CONSTRAINT ck_listing_review CHECK (security_review IN ('pending', 'approved', 'rejected')),
    status           VARCHAR(8)     NOT NULL DEFAULT 'hidden'
                     CONSTRAINT ck_listing_status CHECK (status IN ('listed', 'hidden')),
    created_at       DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    updated_at       DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME()
);
GO
IF OBJECT_ID(N'dbo.marketplace_request', N'U') IS NULL
CREATE TABLE dbo.marketplace_request (
    id             INT            IDENTITY(1,1) PRIMARY KEY,
    listing_id     INT            NOT NULL
                   CONSTRAINT fk_request_listing REFERENCES dbo.marketplace_listing (id) ON DELETE CASCADE,
    requested_by   NVARCHAR(150)  NOT NULL,
    justification  NVARCHAR(500)  NULL,
    status         VARCHAR(12)    NOT NULL DEFAULT 'pending'
                   CONSTRAINT ck_request_status CHECK (status IN ('pending', 'approved', 'rejected', 'provisioned')),
    decided_by     NVARCHAR(150)  NULL,
    decision_note  NVARCHAR(500)  NULL,
    agent_id       INT            NULL,   -- agente creato dal provisioning (nessuna FK: la storia resta)
    created_at     DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    decided_at     DATETIME2(3)   NULL,
    provisioned_at DATETIME2(3)   NULL
);
GO
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_request_listing' AND object_id = OBJECT_ID(N'dbo.marketplace_request'))
    CREATE INDEX ix_request_listing ON dbo.marketplace_request (listing_id, status);
GO

-- Campo framework sull'agente (testo libero, es. LangGraph, CrewAI, AutoGen, Google ADK, AWS AgentCore, Microsoft Foundry, altro)
IF COL_LENGTH(N'dbo.agent', N'framework') IS NULL
    ALTER TABLE dbo.agent ADD framework NVARCHAR(100) NULL;
GO

-- ============================================================================
-- Punto 8 - FinOps e tracciamento degli agenti
-- ============================================================================
IF OBJECT_ID(N'dbo.finops_outcome', N'U') IS NULL
CREATE TABLE dbo.finops_outcome (
    id          BIGINT         IDENTITY(1,1) PRIMARY KEY,
    agent_id    INT            NOT NULL
                CONSTRAINT fk_outcome_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    outcome     VARCHAR(10)    NOT NULL
                CONSTRAINT ck_outcome_value CHECK (outcome IN ('success', 'failure', 'partial')),
    value_eur   DECIMAL(18,4)  NOT NULL DEFAULT 0
                CONSTRAINT ck_outcome_value_eur CHECK (value_eur >= 0),
    period      DATE           NOT NULL,
    note        NVARCHAR(500)  NULL,
    created_at  DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME()
);
GO
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_outcome_agent_period' AND object_id = OBJECT_ID(N'dbo.finops_outcome'))
    CREATE INDEX ix_outcome_agent_period ON dbo.finops_outcome (agent_id, period);
GO

-- Identità per agente: UUID alla creazione, revoca registrata nel tempo
IF COL_LENGTH(N'dbo.agent', N'identity_id') IS NULL
    ALTER TABLE dbo.agent ADD identity_id UNIQUEIDENTIFIER NOT NULL
        CONSTRAINT df_agent_identity DEFAULT NEWID();
GO
IF COL_LENGTH(N'dbo.agent', N'identity_revoked_at') IS NULL
    ALTER TABLE dbo.agent ADD identity_revoked_at DATETIME2(3) NULL;
GO

-- Punto 6: parametri stabili delle violazioni (il campo detail resta in italiano)
IF COL_LENGTH(N'dbo.eval_violation', N'params') IS NULL
    ALTER TABLE dbo.eval_violation ADD params NVARCHAR(MAX) NOT NULL
        CONSTRAINT df_violation_params DEFAULT N'{}'
        CONSTRAINT ck_violation_params CHECK (ISJSON(params) = 1);
GO

-- ============================================================================
-- Sicurezza: utenti, sessioni, tentativi di accesso (docs/SICUREZZA.txt)
-- La password è salvata solo come hash bcrypt. Il token di sessione è salvato solo come SHA-256.
-- ============================================================================
IF OBJECT_ID(N'dbo.app_user', N'U') IS NULL
CREATE TABLE dbo.app_user (
    id                   INT           IDENTITY(1,1) PRIMARY KEY,
    username             NVARCHAR(50)  NOT NULL CONSTRAINT uq_app_user_name UNIQUE,
    password_hash        VARCHAR(100)  NOT NULL,
    role                 VARCHAR(20)   NOT NULL
                         CONSTRAINT ck_app_user_role CHECK (role IN ('admin', 'manager', 'operator', 'auditor', 'viewer', 'integration')),
    active               BIT           NOT NULL DEFAULT 1,
    must_change_password BIT           NOT NULL DEFAULT 1,
    created_at           DATETIME2(3)  NOT NULL DEFAULT SYSUTCDATETIME(),
    last_login_at        DATETIME2(3)  NULL
);
GO
IF OBJECT_ID(N'dbo.login_session', N'U') IS NULL
CREATE TABLE dbo.login_session (
    token_hash   CHAR(64)      NOT NULL PRIMARY KEY,
    user_id      INT           NOT NULL
                 CONSTRAINT fk_session_user REFERENCES dbo.app_user (id) ON DELETE CASCADE,
    created_at   DATETIME2(3)  NOT NULL DEFAULT SYSUTCDATETIME(),
    expires_at   DATETIME2(3)  NOT NULL
);
GO
IF OBJECT_ID(N'dbo.login_attempt', N'U') IS NULL
CREATE TABLE dbo.login_attempt (
    id        BIGINT         IDENTITY(1,1) PRIMARY KEY,
    username  NVARCHAR(50)   NOT NULL,
    success   BIT            NOT NULL,
    at        DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME()
);
GO
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'ix_attempt_user_time' AND object_id = OBJECT_ID(N'dbo.login_attempt'))
    CREATE INDEX ix_attempt_user_time ON dbo.login_attempt (username, at);
GO

-- ============================================================================
-- Server MCP e accesso unificato (docs/MCP.txt)
-- ============================================================================
IF COL_LENGTH(N'dbo.integration_approval', N'tool_id') IS NULL
    ALTER TABLE dbo.integration_approval ADD tool_id INT NULL;
GO

IF OBJECT_ID(N'dbo.mcp_tool', N'U') IS NULL
CREATE TABLE dbo.mcp_tool (
    id                INT            IDENTITY(1,1) PRIMARY KEY,
    resource_id       INT            NOT NULL
                      CONSTRAINT fk_tool_resource REFERENCES dbo.integration_resource (id) ON DELETE CASCADE,
    name              NVARCHAR(150)  NOT NULL,
    description       NVARCHAR(1000) NULL,
    risk              VARCHAR(10)    NOT NULL DEFAULT 'low'
                      CONSTRAINT ck_tool_risk CHECK (risk IN ('low', 'medium', 'high')),
    requires_approval BIT            NOT NULL DEFAULT 0,
    created_at        DATETIME2(3)   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT uq_tool_name UNIQUE (resource_id, name)
);
GO

IF OBJECT_ID(N'dbo.tool_access', N'U') IS NULL
CREATE TABLE dbo.tool_access (
    id                INT          IDENTITY(1,1) PRIMARY KEY,
    agent_id          INT          NOT NULL
                      CONSTRAINT fk_toolaccess_agent REFERENCES dbo.agent (id) ON DELETE CASCADE,
    tool_id           INT          NOT NULL
                      CONSTRAINT fk_toolaccess_tool REFERENCES dbo.mcp_tool (id) ON DELETE CASCADE,
    requires_approval BIT          NOT NULL DEFAULT 0,
    created_at        DATETIME2(3) NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT uq_tool_access UNIQUE (agent_id, tool_id)
);
GO

-- Registro del gateway: senza testo, solo lunghezze e SHA-256 di argomenti e risultato
IF OBJECT_ID(N'dbo.gateway_event', N'U') IS NULL
CREATE TABLE dbo.gateway_event (
    id              BIGINT        IDENTITY(1,1) PRIMARY KEY,
    at              DATETIME2(3)  NOT NULL DEFAULT SYSUTCDATETIME(),
    agent_id        INT           NOT NULL,
    resource_id     INT           NOT NULL,
    tool_name       NVARCHAR(150) NULL,
    decision        VARCHAR(10)   NOT NULL CONSTRAINT ck_gw_decision CHECK (decision IN ('allowed', 'denied', 'blocked', 'error')),
    reason          VARCHAR(50)   NOT NULL,
    args_length     INT           NOT NULL DEFAULT 0,
    args_sha256     CHAR(64)      NULL,
    result_length   INT           NOT NULL DEFAULT 0,
    result_sha256   CHAR(64)      NULL,
    latency_ms      INT           NOT NULL DEFAULT 0
);
GO
