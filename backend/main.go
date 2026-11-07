// Open-GenCut v0.1 - API REST del Registro unificato (punto 1).
//
// Implementa il contratto docs/API.txt:
//   - agenti (CRUD, filtri q/origin/status/owner)
//   - configurazioni (CRUD, chiave unica, scope global|agent)
//   - metriche (summary, usage) e audit (ogni scrittura produce una riga)
//   - salute (GET /api/health)
//
// Errori: {"error": "codice", "message": "testo"}.
//
// Variabili d'ambiente:
//
//	MSSQL_HOST      host SQL Server      (default: mon_mssql_express)
//	MSSQL_PORT      porta                (default: 1433)
//	MSSQL_DATABASE  database             (default: geniuscut)
//	MSSQL_USER      utente               (default: sa)
//	MSSQL_PASSWORD  password             (obbligatoria, nessun default)
//	HTTP_ADDR       indirizzo di ascolto (default: :8080)
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// Valori ammessi, come da CHECK di db/schema.sql e docs/API.txt.
var (
	validOrigins  = map[string]bool{"internal": true, "marketplace": true, "third_party": true}
	validStatuses = map[string]bool{"registered": true, "active": true, "suspended": true, "retired": true}
	validCurrency = map[string]bool{"EUR": true, "USD": true}
	validScopes   = map[string]bool{"global": true, "agent": true}
)

// ---------------------------------------------------------------------------
// Modelli JSON
// ---------------------------------------------------------------------------

// Agent è la rappresentazione JSON di un agente (docs/API.txt, "Campi agente").
type Agent struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Origin      string  `json:"origin"`
	Vendor      *string `json:"vendor"`
	Version     *string `json:"version"`
	Owner       *string `json:"owner"`
	Status      string  `json:"status"`
	Model       *string `json:"model"`
	Endpoint    *string `json:"endpoint"`
	CostMonthly float64 `json:"cost_monthly"`
	Currency    string  `json:"currency"`
	// R4: dichiarazione di trasparenza.
	InteractsWithPeople bool    `json:"interacts_with_people"`
	InteractionLanguage *string `json:"interaction_language"`
	// Punto 8: framework dichiarato e identità per agente (UUID, revoca).
	Framework         *string    `json:"framework"`
	IdentityID        string     `json:"identity_id"`
	IdentityRevokedAt *time.Time `json:"identity_revoked_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// AgentInput è il corpo di POST e PUT /api/agents. La PUT sostituisce l'intero agente.
// Le stringhe vuote sui campi opzionali diventano NULL.
type AgentInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Origin      string   `json:"origin"`
	Vendor      string   `json:"vendor"`
	Version     string   `json:"version"`
	Owner       string   `json:"owner"`
	Status      string   `json:"status"` // default "registered"
	Model       string   `json:"model"`
	Endpoint    string   `json:"endpoint"`
	CostMonthly *float64 `json:"cost_monthly"` // default 0
	Currency    string   `json:"currency"`     // default "EUR"
	// R4: l'agente interagisce con persone? Se sì, indicare la lingua (obbligatoria in quel caso).
	InteractsWithPeople bool   `json:"interacts_with_people"` // default false
	InteractionLanguage string `json:"interaction_language"`  // es. "it", "en-GB"
	Framework           string `json:"framework"`             // testo libero: LangGraph, CrewAI, AutoGen, ...
}

// Configuration è la rappresentazione JSON di una configurazione.
type Configuration struct {
	ID          int       `json:"id"`
	Key         string    `json:"key"`
	Value       string    `json:"value"`
	Scope       string    `json:"scope"`
	AgentID     *int      `json:"agent_id"`
	Description *string   `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ConfigurationInput è il corpo di POST e PUT /api/configurations.
type ConfigurationInput struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Scope       string `json:"scope"` // obbligatorio
	AgentID     *int   `json:"agent_id"`
	Description string `json:"description"`
}

// AuditEntry è una riga del registro di audit.
type AuditEntry struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"`
	Entity   string    `json:"entity"`
	EntityID *int      `json:"entity_id"`
	Detail   *string   `json:"detail"`
}

// ---------------------------------------------------------------------------
// Server e routing
// ---------------------------------------------------------------------------

// Server contiene le dipendenze dei gestori HTTP.
type Server struct {
	db *sql.DB
}

func main() {
	dsn, err := buildDSN()
	if err != nil {
		log.Fatalf("configurazione database: %v", err)
	}

	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		log.Fatalf("apertura database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	// Il container SQL Server può impiegare qualche secondo ad avviarsi.
	if err := waitForDB(db, 60*time.Second); err != nil {
		log.Fatalf("database non raggiungibile: %v", err)
	}

	if err := ensureAdmin(context.Background(), db); err != nil {
		log.Fatalf("utente iniziale: %v", err)
	}
	srv := &Server{db: db}
	addr := getenv("HTTP_ADDR", ":8080")
	log.Printf("Open-GenCut API v0.1 in ascolto su %s", addr)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatalf("server HTTP: %v", err)
	}
}

// routes registra le rotte con il ServeMux di Go 1.22+ (metodo + wildcard).
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)

	mux.HandleFunc("GET /api/agents", s.listAgents)
	mux.HandleFunc("POST /api/agents", s.createAgent)
	mux.HandleFunc("GET /api/agents/{id}", s.getAgent)
	mux.HandleFunc("PUT /api/agents/{id}", s.updateAgent)
	mux.HandleFunc("DELETE /api/agents/{id}", s.deleteAgent)

	mux.HandleFunc("GET /api/configurations", s.listConfigurations)
	mux.HandleFunc("POST /api/configurations", s.createConfiguration)
	mux.HandleFunc("PUT /api/configurations/{id}", s.updateConfiguration)
	mux.HandleFunc("DELETE /api/configurations/{id}", s.deleteConfiguration)

	mux.HandleFunc("GET /api/metrics/summary", s.metricsSummary)
	mux.HandleFunc("GET /api/metrics/usage", s.metricsUsage)
	mux.HandleFunc("GET /api/audit", s.listAudit)
	mux.HandleFunc("GET /api/audit/verify", s.verifyAudit)

	s.registerIntegrations(mux)     // punto 2: /api/integrations/...
	s.registerLifecycle(mux)        // punto 3: /api/lifecycle/...
	s.registerEvaluations(mux)      // punto 4 R3: valutazioni prima di staging e produzione
	s.registerApprovals(mux)        // punto 4 R1: approvazione umana per azioni sensibili
	s.registerCompliance(mux)       // punto 4 della specifica: conformità
	s.registerComplianceImport(mux) // aggiornamento dei quadri da bundle
	s.registerGuardrails(mux)       // punto 5 della specifica: guardrail
	s.registerEvaluation(mux)       // punto 6 della specifica: valutazione
	s.registerMarketplace(mux)      // punto 7 della specifica: marketplace
	s.registerIdentity(mux)         // identità per agente con revoca
	s.registerFinOps(mux)           // punto 8 della specifica: FinOps
	s.registerChat(mux)             // chat verso OpenAI e Anthropic attraverso il backend
	s.registerMCP(mux)              // server MCP: strumenti, sincronizzazione, permessi per strumento
	s.registerGateway(mux)          // punto di accesso unificato: /api/gateway/call
	s.registerInactive(mux)         // agenti senza attività
	s.registerOTLP(mux)             // ingestione OpenTelemetry in JSON
	s.registerDiscovery(mux)        // scoperta: import di manifesti agente
	s.registerDeviations(mux)       // deviazioni dei ragionamenti dalla sequenza di riferimento
	s.registerJudge(mux)            // giudizio automatico con un modello, tramite il proxy della chat
	s.registerAuth(mux)             // sicurezza: sessioni, utenti, ruoli

	// GUI statica: tutto ciò che non è /api viene servito dalla cartella WEB_DIR
	// (index.html di default). Stessa origine, quindi la GUI usa percorsi relativi.
	mux.Handle("/", http.FileServer(http.Dir(getenv("WEB_DIR", "/web"))))

	return logRequests(s.secure(mux))
}

// ---------------------------------------------------------------------------
// Salute
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		log.Printf("health: database non raggiungibile: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "db": "error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "db": "ok"})
}

// ---------------------------------------------------------------------------
// Agenti
// ---------------------------------------------------------------------------

const agentColumns = `a.id, a.name, a.description, a.origin, a.vendor, a.version, a.owner,
	a.status, a.model, a.endpoint, a.cost_monthly, a.currency, a.created_at, a.updated_at,
	a.interacts_with_people, a.interaction_language, a.framework,
	CAST(a.identity_id AS NVARCHAR(36)), a.identity_revoked_at`

// listAgents: GET /api/agents?q=&origin=&status=&owner=
// q cerca in nome, descrizione, fornitore, modello e proprietario.
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	q := strings.TrimSpace(qs.Get("q"))
	origin := qs.Get("origin")
	status := qs.Get("status")
	owner := strings.TrimSpace(qs.Get("owner"))

	if origin != "" && !validOrigins[origin] {
		writeError(w, http.StatusBadRequest, "validation_error", "origin deve essere internal, marketplace o third_party")
		return
	}
	if status != "" && !validStatuses[status] {
		writeError(w, http.StatusBadRequest, "validation_error", "status deve essere registered, active, suspended o retired")
		return
	}

	// ESCAPE '\' rende letterali i caratteri % e _ digitati dall'utente.
	like := "%" + likeEscape(q) + "%"
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+agentColumns+` FROM dbo.agent a
		  WHERE (@p1 = N'' OR a.name LIKE @p2 ESCAPE '\' OR a.description LIKE @p2 ESCAPE '\'
		         OR a.vendor LIKE @p2 ESCAPE '\' OR a.model LIKE @p2 ESCAPE '\' OR a.owner LIKE @p2 ESCAPE '\')
		    AND (@p3 = N'' OR a.origin = @p3)
		    AND (@p4 = N'' OR a.status = @p4)
		    AND (@p5 = N'' OR a.owner = @p5)
		  ORDER BY a.id`,
		q, like, origin, status, owner)
	if err != nil {
		serverError(w, "listAgents", err)
		return
	}
	defer rows.Close()

	agents := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			serverError(w, "listAgents scan", err)
			return
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listAgents rows", err)
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

// createAgent: POST /api/agents. Risponde 201 con l'agente creato.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeAgentInput(w, r)
	if !ok {
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createAgent begin", err)
		return
	}
	defer tx.Rollback() // senza effetto dopo il Commit

	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.agent (name, description, origin, vendor, version, owner, status,
		                        model, endpoint, cost_monthly, currency,
		                        interacts_with_people, interaction_language, framework)
		 OUTPUT inserted.id
		 VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10, @p11, @p12, @p13, @p14)`,
		in.Name, nullable(in.Description), in.Origin, nullable(in.Vendor), nullable(in.Version),
		nullable(in.Owner), in.Status, nullable(in.Model), nullable(in.Endpoint),
		in.costOrZero(), in.Currency, in.InteractsWithPeople, nullable(in.InteractionLanguage),
		nullable(in.Framework),
	).Scan(&id)
	if err != nil {
		writeDBError(w, "createAgent insert", err)
		return
	}

	if err := writeAudit(r.Context(), tx, "create", "agent", id, auditDetail("agent_created", map[string]any{"name": in.Name})); err != nil {
		serverError(w, "createAgent audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createAgent commit", err)
		return
	}

	a, err := s.fetchAgent(r.Context(), id)
	if err != nil {
		serverError(w, "createAgent fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// getAgent: GET /api/agents/{id}
func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, err := s.fetchAgent(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "agente")
		return
	}
	if err != nil {
		serverError(w, "getAgent", err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// updateAgent: PUT /api/agents/{id}. Sostituzione completa; l'audit riporta i cambi di stato.
func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeAgentInput(w, r)
	if !ok {
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateAgent begin", err)
		return
	}
	defer tx.Rollback()

	// Lettura con blocco di aggiornamento: stato precedente per l'audit.
	var oldStatus string
	err = tx.QueryRowContext(r.Context(),
		`SELECT status FROM dbo.agent WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&oldStatus)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "agente")
		return
	}
	if err != nil {
		serverError(w, "updateAgent read", err)
		return
	}
	// La dismissione è definitiva: nemmeno la PUT può riportare un agente dismesso a un altro stato.
	if oldStatus == "retired" && in.Status != "retired" {
		writeInvalidState(w, "agente dismesso: lo stato non può più cambiare")
		return
	}

	_, err = tx.ExecContext(r.Context(),
		`UPDATE dbo.agent
		    SET name = @p1, description = @p2, origin = @p3, vendor = @p4, version = @p5,
		        owner = @p6, status = @p7, model = @p8, endpoint = @p9,
		        cost_monthly = @p10, currency = @p11, interacts_with_people = @p12,
		        interaction_language = @p13, framework = @p14, updated_at = SYSUTCDATETIME()
		  WHERE id = @p15`,
		in.Name, nullable(in.Description), in.Origin, nullable(in.Vendor), nullable(in.Version),
		nullable(in.Owner), in.Status, nullable(in.Model), nullable(in.Endpoint),
		in.costOrZero(), in.Currency, in.InteractsWithPeople, nullable(in.InteractionLanguage),
		nullable(in.Framework), id)
	if err != nil {
		writeDBError(w, "updateAgent update", err)
		return
	}

	key, params := "agent_updated", map[string]any{"name": in.Name}
	if oldStatus != in.Status {
		key, params = "status_change", map[string]any{"name": in.Name, "from": oldStatus, "to": in.Status}
	}
	if err := writeAudit(r.Context(), tx, "update", "agent", id, auditDetail(key, params)); err != nil {
		serverError(w, "updateAgent audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateAgent commit", err)
		return
	}

	a, err := s.fetchAgent(r.Context(), id)
	if err != nil {
		serverError(w, "updateAgent fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// deleteAgent: DELETE /api/agents/{id}. Configurazioni e log di utilizzo
// collegati vengono rimossi in cascata. Risponde 204.
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteAgent begin", err)
		return
	}
	defer tx.Rollback()

	var name string
	err = tx.QueryRowContext(r.Context(),
		`SELECT name FROM dbo.agent WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "agente")
		return
	}
	if err != nil {
		serverError(w, "deleteAgent read", err)
		return
	}

	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.agent WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteAgent delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "agent", id, auditDetail("agent_deleted", map[string]any{"name": name})); err != nil {
		serverError(w, "deleteAgent audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteAgent commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fetchAgent(ctx context.Context, id int) (Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM dbo.agent a WHERE a.id = @p1`, id)
	return scanAgent(row)
}

// rowScanner è comune a *sql.Row e *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAgent(sc rowScanner) (Agent, error) {
	var a Agent
	var desc, vendor, version, owner, model, endpoint, lang, framework sql.NullString
	var revokedAt sql.NullTime
	if err := sc.Scan(&a.ID, &a.Name, &desc, &a.Origin, &vendor, &version, &owner,
		&a.Status, &model, &endpoint, &a.CostMonthly, &a.Currency, &a.CreatedAt, &a.UpdatedAt,
		&a.InteractsWithPeople, &lang, &framework, &a.IdentityID, &revokedAt); err != nil {
		return Agent{}, err
	}
	a.InteractionLanguage = ptrOf(lang)
	a.Framework = ptrOf(framework)
	if revokedAt.Valid {
		t := revokedAt.Time
		a.IdentityRevokedAt = &t
	}
	a.Description = ptrOf(desc)
	a.Vendor = ptrOf(vendor)
	a.Version = ptrOf(version)
	a.Owner = ptrOf(owner)
	a.Model = ptrOf(model)
	a.Endpoint = ptrOf(endpoint)
	return a, nil
}

// decodeAgentInput legge e valida il corpo di POST/PUT agenti.
func decodeAgentInput(w http.ResponseWriter, r *http.Request) (AgentInput, bool) {
	var in AgentInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	if msg := validateAgentInput(&in); msg != "" {
		writeValidation(w, msg)
		return in, false
	}
	return in, true
}

// validateAgentInput normalizza e controlla un agente (creazione, PUT e import). Restituisce il messaggio d'errore o "".
func validateAgentInput(in *AgentInput) string {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || runeLen(in.Name) > 150 {
		return "name è obbligatorio (massimo 150 caratteri)"
	}
	if !validOrigins[in.Origin] {
		return "origin deve essere internal, marketplace o third_party"
	}
	if in.Status == "" {
		in.Status = "registered"
	}
	if !validStatuses[in.Status] {
		return "status deve essere registered, active, suspended o retired"
	}
	if in.Currency == "" {
		in.Currency = "EUR"
	}
	if !validCurrency[in.Currency] {
		return "currency deve essere EUR o USD"
	}
	if in.CostMonthly != nil && !validMoney(*in.CostMonthly) {
		return "cost_monthly deve essere un numero maggiore o uguale a 0"
	}
	// R4: lingua dichiarata. Obbligatoria se l'agente interagisce con persone.
	if in.InteractionLanguage != "" {
		if runeLen(in.InteractionLanguage) > 35 || !languageTag.MatchString(in.InteractionLanguage) {
			return "interaction_language deve essere un tag di lingua, es. it o en-GB"
		}
	}
	if in.InteractsWithPeople && in.InteractionLanguage == "" {
		return "interaction_language è obbligatoria se l'agente interagisce con persone"
	}
	// Limiti di lunghezza dei campi opzionali (come da colonne in db/schema.sql).
	limits := []struct {
		field, value string
		max          int
	}{
		{"description", in.Description, 1000},
		{"vendor", in.Vendor, 150},
		{"version", in.Version, 50},
		{"owner", in.Owner, 150},
		{"model", in.Model, 150},
		{"endpoint", in.Endpoint, 300},
		{"framework", in.Framework, 100},
	}
	for _, l := range limits {
		if runeLen(l.value) > l.max {
			return l.field + " supera " + strconv.Itoa(l.max) + " caratteri"
		}
	}
	return ""
}

// costOrZero restituisce cost_monthly oppure 0 se non fornito.
func (in AgentInput) costOrZero() float64 {
	if in.CostMonthly == nil {
		return 0
	}
	return *in.CostMonthly
}

// ---------------------------------------------------------------------------
// Configurazioni
// ---------------------------------------------------------------------------

const configurationColumns = `c.id, c.[key], c.value, c.scope, c.agent_id, c.description, c.created_at, c.updated_at`

// listConfigurations: GET /api/configurations
func (s *Server) listConfigurations(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+configurationColumns+` FROM dbo.configuration c ORDER BY c.[key]`)
	if err != nil {
		serverError(w, "listConfigurations", err)
		return
	}
	defer rows.Close()

	configs := []Configuration{}
	for rows.Next() {
		c, err := scanConfiguration(rows)
		if err != nil {
			serverError(w, "listConfigurations scan", err)
			return
		}
		configs = append(configs, c)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listConfigurations rows", err)
		return
	}
	writeJSON(w, http.StatusOK, configs)
}

// createConfiguration: POST /api/configurations. Risponde 201.
func (s *Server) createConfiguration(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeConfigurationInput(w, r)
	if !ok {
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createConfiguration begin", err)
		return
	}
	defer tx.Rollback()

	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.configuration ([key], value, scope, agent_id, description)
		 OUTPUT inserted.id
		 VALUES (@p1, @p2, @p3, @p4, @p5)`,
		in.Key, in.Value, in.Scope, agentParam(in.AgentID), nullable(in.Description),
	).Scan(&id)
	if err != nil {
		writeDBError(w, "createConfiguration insert", err)
		return
	}

	if err := writeAudit(r.Context(), tx, "create", "configuration", id, auditDetail("configuration_created", map[string]any{"key": in.Key})); err != nil {
		serverError(w, "createConfiguration audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createConfiguration commit", err)
		return
	}

	c, err := s.fetchConfiguration(r.Context(), id)
	if err != nil {
		serverError(w, "createConfiguration fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// updateConfiguration: PUT /api/configurations/{id}. Sostituzione completa.
func (s *Server) updateConfiguration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeConfigurationInput(w, r)
	if !ok {
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateConfiguration begin", err)
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.configuration
		    SET [key] = @p1, value = @p2, scope = @p3, agent_id = @p4,
		        description = @p5, updated_at = SYSUTCDATETIME()
		  WHERE id = @p6`,
		in.Key, in.Value, in.Scope, agentParam(in.AgentID), nullable(in.Description), id)
	if err != nil {
		writeDBError(w, "updateConfiguration update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "configurazione")
		return
	}

	if err := writeAudit(r.Context(), tx, "update", "configuration", id, auditDetail("configuration_updated", map[string]any{"key": in.Key})); err != nil {
		serverError(w, "updateConfiguration audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateConfiguration commit", err)
		return
	}

	c, err := s.fetchConfiguration(r.Context(), id)
	if err != nil {
		serverError(w, "updateConfiguration fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// deleteConfiguration: DELETE /api/configurations/{id}. Risponde 204.
func (s *Server) deleteConfiguration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteConfiguration begin", err)
		return
	}
	defer tx.Rollback()

	var key string
	err = tx.QueryRowContext(r.Context(),
		`SELECT [key] FROM dbo.configuration WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "configurazione")
		return
	}
	if err != nil {
		serverError(w, "deleteConfiguration read", err)
		return
	}

	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.configuration WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteConfiguration delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "configuration", id, auditDetail("configuration_deleted", map[string]any{"key": key})); err != nil {
		serverError(w, "deleteConfiguration audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteConfiguration commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fetchConfiguration(ctx context.Context, id int) (Configuration, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+configurationColumns+` FROM dbo.configuration c WHERE c.id = @p1`, id)
	return scanConfiguration(row)
}

func scanConfiguration(sc rowScanner) (Configuration, error) {
	var c Configuration
	var agentID sql.NullInt64
	var desc sql.NullString
	if err := sc.Scan(&c.ID, &c.Key, &c.Value, &c.Scope, &agentID, &desc, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Configuration{}, err
	}
	if agentID.Valid {
		v := int(agentID.Int64)
		c.AgentID = &v
	}
	c.Description = ptrOf(desc)
	return c, nil
}

// decodeConfigurationInput legge e valida il corpo di POST/PUT configurazioni.
func decodeConfigurationInput(w http.ResponseWriter, r *http.Request) (ConfigurationInput, bool) {
	var in ConfigurationInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" || runeLen(in.Key) > 150 {
		writeValidation(w, "key è obbligatoria (massimo 150 caratteri)")
		return in, false
	}
	if !validScopes[in.Scope] {
		writeValidation(w, "scope deve essere global o agent")
		return in, false
	}
	// Coerenza scope / agent_id (anche garantita dal CHECK nel database).
	if in.Scope == "agent" && (in.AgentID == nil || *in.AgentID <= 0) {
		writeValidation(w, "agent_id è obbligatorio quando scope è agent")
		return in, false
	}
	if in.Scope == "global" && in.AgentID != nil {
		writeValidation(w, "agent_id deve essere vuoto quando scope è global")
		return in, false
	}
	if runeLen(in.Description) > 1000 {
		writeValidation(w, "description supera 1000 caratteri")
		return in, false
	}
	return in, true
}

// ---------------------------------------------------------------------------
// Metriche
// ---------------------------------------------------------------------------

// MetricsSummary è la risposta di GET /api/metrics/summary.
//
// Decisione: cost_monthly_total somma solo gli agenti in EUR
// e currency vale "EUR". Gli agenti in USD non si sommano senza un tasso di cambio:
// il loro totale è in cost_monthly_other.
type MetricsSummary struct {
	AgentsTotal      int                `json:"agents_total"`
	ByOrigin         map[string]int     `json:"by_origin"`
	ByStatus         map[string]int     `json:"by_status"`
	CostMonthlyTotal float64            `json:"cost_monthly_total"`
	Currency         string             `json:"currency"`
	CostMonthlyOther map[string]float64 `json:"cost_monthly_other"`
}

// metricsSummary: GET /api/metrics/summary
func (s *Server) metricsSummary(w http.ResponseWriter, r *http.Request) {
	// Tutte le chiavi previste sono sempre presenti, anche con valore 0.
	m := MetricsSummary{
		ByOrigin:         map[string]int{"internal": 0, "marketplace": 0, "third_party": 0},
		ByStatus:         map[string]int{"registered": 0, "active": 0, "suspended": 0, "retired": 0},
		CostMonthlyOther: map[string]float64{"USD": 0},
		Currency:         "EUR",
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT origin, status, currency, COUNT(*) AS n, SUM(cost_monthly) AS cost
		   FROM dbo.agent GROUP BY origin, status, currency`)
	if err != nil {
		serverError(w, "metricsSummary", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var origin, status, currency string
		var n int
		var cost float64
		if err := rows.Scan(&origin, &status, &currency, &n, &cost); err != nil {
			serverError(w, "metricsSummary scan", err)
			return
		}
		m.AgentsTotal += n
		m.ByOrigin[origin] += n
		m.ByStatus[status] += n
		if currency == "EUR" {
			m.CostMonthlyTotal += cost
		} else {
			m.CostMonthlyOther[currency] += cost
		}
	}
	if err := rows.Err(); err != nil {
		serverError(w, "metricsSummary rows", err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// UsageRow è una riga di GET /api/metrics/usage: aggregato per agente e per giorno.
type UsageRow struct {
	AgentID int     `json:"agent_id"`
	Day     string  `json:"day"` // YYYY-MM-DD, UTC
	Calls   int     `json:"calls"`
	Tokens  int64   `json:"tokens"`
	Cost    float64 `json:"cost"`
}

// metricsUsage: GET /api/metrics/usage?agent_id=&from=&to=
// from e to sono giorni (YYYY-MM-DD) oppure RFC 3339; l'intervallo è incluso.
func (s *Server) metricsUsage(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()

	agentID := 0
	if v := qs.Get("agent_id"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil || id <= 0 {
			writeValidation(w, "agent_id non valido")
			return
		}
		agentID = id
	}
	from, err := parseDay(qs.Get("from"))
	if err != nil {
		writeValidation(w, "from non valido: usare YYYY-MM-DD")
		return
	}
	to, err := parseDay(qs.Get("to"))
	if err != nil {
		writeValidation(w, "to non valido: usare YYYY-MM-DD")
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT agent_id, CONVERT(CHAR(10), called_at, 23) AS day,
		        COUNT(*) AS calls, SUM(CAST(tokens AS BIGINT)) AS tokens, SUM(cost) AS cost
		   FROM dbo.usage_log
		  WHERE (@p1 = 0 OR agent_id = @p1)
		    AND (@p2 IS NULL OR called_at >= CAST(@p2 AS DATE))
		    AND (@p3 IS NULL OR called_at < DATEADD(DAY, 1, CAST(@p3 AS DATE)))
		  GROUP BY agent_id, CONVERT(CHAR(10), called_at, 23)
		  ORDER BY day, agent_id`,
		agentID, from, to)
	if err != nil {
		serverError(w, "metricsUsage", err)
		return
	}
	defer rows.Close()

	out := []UsageRow{}
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.AgentID, &u.Day, &u.Calls, &u.Tokens, &u.Cost); err != nil {
			serverError(w, "metricsUsage scan", err)
			return
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "metricsUsage rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// listAudit: GET /api/audit?limit=50 (default 50, massimo 500), dal più recente.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeValidation(w, "limit deve essere tra 1 e 500")
			return
		}
		limit = n
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT TOP (@p1) id, at, actor, action, entity, entity_id, detail
		   FROM dbo.audit ORDER BY id DESC`, limit)
	if err != nil {
		serverError(w, "listAudit", err)
		return
	}
	defer rows.Close()

	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var entityID sql.NullInt64
		var detail sql.NullString
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Entity, &entityID, &detail); err != nil {
			serverError(w, "listAudit scan", err)
			return
		}
		if entityID.Valid {
			v := int(entityID.Int64)
			e.EntityID = &v
		}
		e.Detail = ptrOf(detail)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listAudit rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Utilità HTTP
// ---------------------------------------------------------------------------

// decodeJSON legge il corpo come JSON: limite 1 MB, campi sconosciuti rifiutati.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "JSON non valido: "+err.Error())
		return false
	}
	// Il corpo deve contenere un solo oggetto: dati dopo la chiusura sono un errore.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "JSON non valido: dati dopo l'oggetto")
		return false
	}
	return true
}

// pathID legge il parametro {id} dal percorso.
func pathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "id non valido")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("scrittura risposta: %v", err)
	}
}

// apiError è la forma uniforme degli errori (docs/API.txt): {"error": codice, "message": testo}.
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: code, Message: message})
}

func writeValidation(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, "validation_error", message)
}

func writeNotFound(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotFound, "not_found", what+" non trovato")
}

// serverError registra il dettaglio nel log e restituisce un messaggio generico.
func serverError(w http.ResponseWriter, where string, err error) {
	log.Printf("errore %s: %v", where, err)
	writeError(w, http.StatusInternalServerError, "internal_error", "errore interno")
}

// writeDBError traduce gli errori SQL Server più comuni in codici HTTP.
//
//	2627 / 2601 violazione di unicità          -> 409 conflict
//	547 / 515   chiave esterna, CHECK, NULL     -> 400 validation_error
func writeDBError(w http.ResponseWriter, where string, err error) {
	var me mssql.Error
	var mep *mssql.Error
	var num int32
	switch {
	case errors.As(err, &me):
		num = me.Number
	case errors.As(err, &mep):
		num = mep.Number
	}
	switch num {
	case 2627, 2601:
		writeError(w, http.StatusConflict, "conflict", "valore già presente (chiave duplicata)")
	case 547, 515:
		writeValidation(w, "dati non validi per i vincoli del database")
	default:
		serverError(w, where, err)
	}
}

// ---------------------------------------------------------------------------
// Utilità database e stringhe
// ---------------------------------------------------------------------------

// nullable restituisce NULL per la stringa vuota, altrimenti la stringa.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// languageTag accetta tag di lingua semplici: "it", "en", "en-GB", "zh-Hant-TW".
var languageTag = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8})*$`)

// agentParam restituisce NULL per un agent_id assente, altrimenti l'id.
func agentParam(id *int) any {
	if id == nil {
		return nil
	}
	return *id
}

func ptrOf(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

// likeEscape rende letterali i caratteri speciali di LIKE (usare con ESCAPE '\').
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `[`, `\[`)
	return r.Replace(s)
}

// parseDay accetta YYYY-MM-DD o RFC 3339. Stringa vuota = nessun filtro.
func parseDay(s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, err
	}
	return t.UTC(), nil
}

// buildDSN costruisce la stringa di connessione dalle variabili d'ambiente.
func buildDSN() (string, error) {
	pwd := os.Getenv("MSSQL_PASSWORD")
	if pwd == "" {
		return "", errors.New("MSSQL_PASSWORD non impostata")
	}
	u := &url.URL{
		Scheme: "sqlserver",
		User:   url.UserPassword(getenv("MSSQL_USER", "sa"), pwd),
		Host:   net.JoinHostPort(getenv("MSSQL_HOST", "mon_mssql_express"), getenv("MSSQL_PORT", "1433")),
	}
	q := u.Query()
	q.Set("database", getenv("MSSQL_DATABASE", "geniuscut"))
	q.Set("encrypt", "disable") // laboratorio: certificato non gestito
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// waitForDB ritenta il ping finché il database risponde o scade il timeout.
func waitForDB(db *sql.DB, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := db.PingContext(ctx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ping fallito dopo %s: %w", timeout, err)
		}
		log.Printf("database non ancora pronto, nuovo tentativo: %v", err)
		time.Sleep(2 * time.Second)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// logRequests registra metodo, percorso e durata di ogni richiesta.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
