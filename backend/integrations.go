// Open-GenCut v0.1 - Punto 2: controllo accessi degli agenti verso risorse esterne.
//
// Risorse: modelli LLM, API, server MCP, fonti dati (tabella integration_resource).
// Allowlist: quale agente può usare quale risorsa (agent_access).
// Limiti: chiamate al giorno e costo al mese per coppia agente-risorsa (access_limit).
// Controllo: POST /api/integrations/check decide e registra ogni accesso (access_event).
//
// Rotte sotto /api/integrations, registrate da registerIntegrations.
package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

var (
	validResourceKinds    = map[string]bool{"llm_model": true, "api": true, "mcp_server": true, "data_source": true, "ai_gateway": true}
	validResourceStatuses = map[string]bool{"enabled": true, "disabled": true}
)

// ---------------------------------------------------------------------------
// Modelli JSON
// ---------------------------------------------------------------------------

// Resource è una risorsa esterna censita nel registro.
type Resource struct {
	ID          int       `json:"id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Endpoint    *string   `json:"endpoint"`
	Description *string   `json:"description"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ResourceInput è il corpo di POST e PUT delle risorse (sostituzione completa).
type ResourceInput struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Endpoint    string `json:"endpoint"`
	Description string `json:"description"`
	Status      string `json:"status"` // default "enabled"
}

// AllowEntry è una riga dell'allowlist: l'agente può usare la risorsa.
type AllowEntry struct {
	ID               int       `json:"id"`
	AgentID          int       `json:"agent_id"`
	ResourceID       int       `json:"resource_id"`
	RequiresApproval bool      `json:"requires_approval"`
	CreatedAt        time.Time `json:"created_at"`
}

// AllowInput è il corpo di POST /api/integrations/allowlist.
type AllowInput struct {
	AgentID          int  `json:"agent_id"`
	ResourceID       int  `json:"resource_id"`
	RequiresApproval bool `json:"requires_approval"` // default false: azione sensibile, serve approvazione
}

// Limit è il limite di chiamate e costo per una coppia agente-risorsa.
// I campi a NULL non sono limitati.
type Limit struct {
	ID              int      `json:"id"`
	AgentID         int      `json:"agent_id"`
	ResourceID      int      `json:"resource_id"`
	MaxCallsPerDay  *int     `json:"max_calls_per_day"`
	MaxCostPerMonth *float64 `json:"max_cost_per_month"`
	Currency        string   `json:"currency"`
}

// LimitInput è il corpo di POST e PUT dei limiti.
type LimitInput struct {
	AgentID         int      `json:"agent_id"`
	ResourceID      int      `json:"resource_id"`
	MaxCallsPerDay  *int     `json:"max_calls_per_day"`
	MaxCostPerMonth *float64 `json:"max_cost_per_month"`
	Currency        string   `json:"currency"` // default "EUR"
}

// CheckInput è il corpo di POST /api/integrations/check, chiamato dall'agente prima di ogni accesso.
// Cost è il costo stimato della chiamata, facoltativo; se l'accesso è permesso viene registrato.
type CheckInput struct {
	AgentID    int      `json:"agent_id"`
	ResourceID int      `json:"resource_id"`
	Cost       *float64 `json:"cost"`
}

// CheckResult è la risposta del controllo. decision è sempre presente: "allowed" o "denied".
// Un accesso negato è una risposta valida (200), non un errore.
type CheckResult struct {
	Decision   string `json:"decision"`
	Reason     string `json:"reason"`
	AgentID    int    `json:"agent_id"`
	ResourceID int    `json:"resource_id"`
	ApprovalID *int   `json:"approval_id"` // richiesta di approvazione collegata, se presente
}

// AccessEvent è una riga del registro degli accessi.
type AccessEvent struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	AgentID    int       `json:"agent_id"`
	ResourceID int       `json:"resource_id"`
	Decision   string    `json:"decision"`
	Reason     string    `json:"reason"`
	Cost       float64   `json:"cost"`
}

// ---------------------------------------------------------------------------
// Rotte
// ---------------------------------------------------------------------------

// registerIntegrations aggiunge le rotte del punto 2 al mux principale.
func (s *Server) registerIntegrations(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations/resources", s.listResources)
	mux.HandleFunc("POST /api/integrations/resources", s.createResource)
	mux.HandleFunc("GET /api/integrations/resources/{id}", s.getResource)
	mux.HandleFunc("PUT /api/integrations/resources/{id}", s.updateResource)
	mux.HandleFunc("DELETE /api/integrations/resources/{id}", s.deleteResource)

	mux.HandleFunc("GET /api/integrations/allowlist", s.listAllowlist)
	mux.HandleFunc("POST /api/integrations/allowlist", s.createAllowEntry)
	mux.HandleFunc("DELETE /api/integrations/allowlist/{id}", s.deleteAllowEntry)

	mux.HandleFunc("GET /api/integrations/limits", s.listLimits)
	mux.HandleFunc("POST /api/integrations/limits", s.createLimit)
	mux.HandleFunc("PUT /api/integrations/limits/{id}", s.updateLimit)
	mux.HandleFunc("DELETE /api/integrations/limits/{id}", s.deleteLimit)

	mux.HandleFunc("POST /api/integrations/check", s.checkAccess)
	mux.HandleFunc("GET /api/integrations/access-log", s.listAccessLog)
}

// ---------------------------------------------------------------------------
// Risorse
// ---------------------------------------------------------------------------

const resourceColumns = `r.id, r.kind, r.name, r.endpoint, r.description, r.status, r.created_at, r.updated_at`

// listResources: GET /api/integrations/resources?kind=
func (s *Server) listResources(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "" && !validResourceKinds[kind] {
		writeValidation(w, "kind deve essere llm_model, api, mcp_server, data_source o ai_gateway")
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+resourceColumns+` FROM dbo.integration_resource r
		  WHERE (@p1 = N'' OR r.kind = @p1) ORDER BY r.kind, r.name`, kind)
	if err != nil {
		serverError(w, "listResources", err)
		return
	}
	defer rows.Close()

	out := []Resource{}
	for rows.Next() {
		res, err := scanResource(rows)
		if err != nil {
			serverError(w, "listResources scan", err)
			return
		}
		out = append(out, res)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listResources rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createResource: POST /api/integrations/resources. Risponde 201.
func (s *Server) createResource(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeResourceInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createResource begin", err)
		return
	}
	defer tx.Rollback()

	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.integration_resource (kind, name, endpoint, description, status)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4, @p5)`,
		in.Kind, in.Name, nullable(in.Endpoint), nullable(in.Description), in.Status).Scan(&id)
	if err != nil {
		writeDBError(w, "createResource insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "integration_resource", id, auditDetail("resource_saved", map[string]any{"kind": in.Kind, "name": in.Name, "status": in.Status})); err != nil {
		serverError(w, "createResource audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createResource commit", err)
		return
	}
	res, err := s.fetchResource(r.Context(), id)
	if err != nil {
		serverError(w, "createResource fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) getResource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := s.fetchResource(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "risorsa")
		return
	}
	if err != nil {
		serverError(w, "getResource", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// updateResource: PUT /api/integrations/resources/{id}. Sostituzione completa.
func (s *Server) updateResource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeResourceInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateResource begin", err)
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.integration_resource
		    SET kind = @p1, name = @p2, endpoint = @p3, description = @p4,
		        status = @p5, updated_at = SYSUTCDATETIME()
		  WHERE id = @p6`,
		in.Kind, in.Name, nullable(in.Endpoint), nullable(in.Description), in.Status, id)
	if err != nil {
		writeDBError(w, "updateResource update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "risorsa")
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "integration_resource", id, auditDetail("resource_saved", map[string]any{"kind": in.Kind, "name": in.Name, "status": in.Status})); err != nil {
		serverError(w, "updateResource audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateResource commit", err)
		return
	}
	out, err := s.fetchResource(r.Context(), id)
	if err != nil {
		serverError(w, "updateResource fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteResource: DELETE /api/integrations/resources/{id}. Allowlist e limiti collegati
// vengono rimossi in cascata; il registro degli accessi resta. Risponde 204.
func (s *Server) deleteResource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteResource begin", err)
		return
	}
	defer tx.Rollback()

	var name string
	err = tx.QueryRowContext(r.Context(),
		`SELECT name FROM dbo.integration_resource WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "risorsa")
		return
	}
	if err != nil {
		serverError(w, "deleteResource read", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.integration_resource WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteResource delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "integration_resource", id, auditDetail("resource_deleted", map[string]any{"name": name})); err != nil {
		serverError(w, "deleteResource audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteResource commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fetchResource(ctx context.Context, id int) (Resource, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+resourceColumns+` FROM dbo.integration_resource r WHERE r.id = @p1`, id)
	return scanResource(row)
}

func scanResource(sc rowScanner) (Resource, error) {
	var res Resource
	var endpoint, desc sql.NullString
	if err := sc.Scan(&res.ID, &res.Kind, &res.Name, &endpoint, &desc, &res.Status, &res.CreatedAt, &res.UpdatedAt); err != nil {
		return Resource{}, err
	}
	res.Endpoint = ptrOf(endpoint)
	res.Description = ptrOf(desc)
	return res, nil
}

func decodeResourceInput(w http.ResponseWriter, r *http.Request) (ResourceInput, bool) {
	var in ResourceInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || runeLen(in.Name) > 150 {
		writeValidation(w, "name è obbligatorio (massimo 150 caratteri)")
		return in, false
	}
	if !validResourceKinds[in.Kind] {
		writeValidation(w, "kind deve essere llm_model, api, mcp_server, data_source o ai_gateway")
		return in, false
	}
	if in.Status == "" {
		in.Status = "enabled"
	}
	if !validResourceStatuses[in.Status] {
		writeValidation(w, "status deve essere enabled o disabled")
		return in, false
	}
	if runeLen(in.Endpoint) > 300 {
		writeValidation(w, "endpoint supera 300 caratteri")
		return in, false
	}
	if runeLen(in.Description) > 1000 {
		writeValidation(w, "description supera 1000 caratteri")
		return in, false
	}
	return in, true
}

// ---------------------------------------------------------------------------
// Allowlist
// ---------------------------------------------------------------------------

// listAllowlist: GET /api/integrations/allowlist?agent_id=
func (s *Server) listAllowlist(w http.ResponseWriter, r *http.Request) {
	agentID, ok := optionalID(w, r.URL.Query().Get("agent_id"), "agent_id")
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, agent_id, resource_id, requires_approval, created_at FROM dbo.agent_access
		  WHERE (@p1 = 0 OR agent_id = @p1) ORDER BY agent_id, resource_id`, agentID)
	if err != nil {
		serverError(w, "listAllowlist", err)
		return
	}
	defer rows.Close()

	out := []AllowEntry{}
	for rows.Next() {
		var e AllowEntry
		if err := rows.Scan(&e.ID, &e.AgentID, &e.ResourceID, &e.RequiresApproval, &e.CreatedAt); err != nil {
			serverError(w, "listAllowlist scan", err)
			return
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listAllowlist rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createAllowEntry: POST /api/integrations/allowlist. Risponde 201.
// Una coppia già presente dà 409 (conflict).
func (s *Server) createAllowEntry(w http.ResponseWriter, r *http.Request) {
	var in AllowInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.AgentID <= 0 || in.ResourceID <= 0 {
		writeValidation(w, "agent_id e resource_id sono obbligatori")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createAllowEntry begin", err)
		return
	}
	defer tx.Rollback()

	var e AllowEntry
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.agent_access (agent_id, resource_id, requires_approval)
		 OUTPUT inserted.id, inserted.agent_id, inserted.resource_id, inserted.requires_approval, inserted.created_at
		 VALUES (@p1, @p2, @p3)`, in.AgentID, in.ResourceID, in.RequiresApproval).
		Scan(&e.ID, &e.AgentID, &e.ResourceID, &e.RequiresApproval, &e.CreatedAt)
	if err != nil {
		writeDBError(w, "createAllowEntry insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "allowlist", e.ID,
		auditDetail("allowlist_added", map[string]any{"agent_id": in.AgentID, "resource_id": in.ResourceID, "requires_approval": in.RequiresApproval})); err != nil {
		serverError(w, "createAllowEntry audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createAllowEntry commit", err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

// deleteAllowEntry: DELETE /api/integrations/allowlist/{id}. Risponde 204.
func (s *Server) deleteAllowEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteAllowEntry begin", err)
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.agent_access WHERE id = @p1`, id)
	if err != nil {
		serverError(w, "deleteAllowEntry delete", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "voce di allowlist")
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "allowlist", id, auditDetail("allowlist_removed", nil)); err != nil {
		serverError(w, "deleteAllowEntry audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteAllowEntry commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Limiti
// ---------------------------------------------------------------------------

const limitColumns = `l.id, l.agent_id, l.resource_id, l.max_calls_per_day, l.max_cost_per_month, l.currency`

// listLimits: GET /api/integrations/limits?agent_id=
func (s *Server) listLimits(w http.ResponseWriter, r *http.Request) {
	agentID, ok := optionalID(w, r.URL.Query().Get("agent_id"), "agent_id")
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+limitColumns+` FROM dbo.access_limit l
		  WHERE (@p1 = 0 OR l.agent_id = @p1) ORDER BY l.agent_id, l.resource_id`, agentID)
	if err != nil {
		serverError(w, "listLimits", err)
		return
	}
	defer rows.Close()

	out := []Limit{}
	for rows.Next() {
		l, err := scanLimit(rows)
		if err != nil {
			serverError(w, "listLimits scan", err)
			return
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listLimits rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createLimit: POST /api/integrations/limits. Risponde 201.
func (s *Server) createLimit(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeLimitInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createLimit begin", err)
		return
	}
	defer tx.Rollback()

	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.access_limit (agent_id, resource_id, max_calls_per_day, max_cost_per_month, currency)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4, @p5)`,
		in.AgentID, in.ResourceID, intParam(in.MaxCallsPerDay), floatParam(in.MaxCostPerMonth), in.Currency).Scan(&id)
	if err != nil {
		writeLimitDBError(w, "createLimit insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "limit", id,
		auditDetail("limit_saved", map[string]any{"agent_id": in.AgentID, "resource_id": in.ResourceID})); err != nil {
		serverError(w, "createLimit audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createLimit commit", err)
		return
	}
	l, err := s.fetchLimit(r.Context(), id)
	if err != nil {
		serverError(w, "createLimit fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

// updateLimit: PUT /api/integrations/limits/{id}. Sostituzione completa.
func (s *Server) updateLimit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeLimitInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateLimit begin", err)
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.access_limit
		    SET agent_id = @p1, resource_id = @p2, max_calls_per_day = @p3,
		        max_cost_per_month = @p4, currency = @p5
		  WHERE id = @p6`,
		in.AgentID, in.ResourceID, intParam(in.MaxCallsPerDay), floatParam(in.MaxCostPerMonth), in.Currency, id)
	if err != nil {
		writeLimitDBError(w, "updateLimit update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "limite")
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "limit", id,
		auditDetail("limit_saved", map[string]any{"agent_id": in.AgentID, "resource_id": in.ResourceID})); err != nil {
		serverError(w, "updateLimit audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateLimit commit", err)
		return
	}
	l, err := s.fetchLimit(r.Context(), id)
	if err != nil {
		serverError(w, "updateLimit fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// deleteLimit: DELETE /api/integrations/limits/{id}. Risponde 204.
func (s *Server) deleteLimit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteLimit begin", err)
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.access_limit WHERE id = @p1`, id)
	if err != nil {
		serverError(w, "deleteLimit delete", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "limite")
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "limit", id, auditDetail("limit_deleted", nil)); err != nil {
		serverError(w, "deleteLimit audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteLimit commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fetchLimit(ctx context.Context, id int) (Limit, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+limitColumns+` FROM dbo.access_limit l WHERE l.id = @p1`, id)
	return scanLimit(row)
}

func scanLimit(sc rowScanner) (Limit, error) {
	var l Limit
	var calls sql.NullInt64
	var cost sql.NullFloat64
	if err := sc.Scan(&l.ID, &l.AgentID, &l.ResourceID, &calls, &cost, &l.Currency); err != nil {
		return Limit{}, err
	}
	if calls.Valid {
		v := int(calls.Int64)
		l.MaxCallsPerDay = &v
	}
	if cost.Valid {
		v := cost.Float64
		l.MaxCostPerMonth = &v
	}
	return l, nil
}

func decodeLimitInput(w http.ResponseWriter, r *http.Request) (LimitInput, bool) {
	var in LimitInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	if in.AgentID <= 0 || in.ResourceID <= 0 {
		writeValidation(w, "agent_id e resource_id sono obbligatori")
		return in, false
	}
	if in.Currency == "" {
		in.Currency = "EUR"
	}
	if in.Currency != "EUR" && in.Currency != "USD" {
		writeValidation(w, "currency deve essere EUR o USD")
		return in, false
	}
	if in.MaxCallsPerDay != nil && (*in.MaxCallsPerDay <= 0 || *in.MaxCallsPerDay > maxInt32) {
		writeValidation(w, "max_calls_per_day deve essere maggiore di 0")
		return in, false
	}
	if in.MaxCostPerMonth != nil && !validMoney(*in.MaxCostPerMonth) {
		writeValidation(w, "max_cost_per_month deve essere maggiore o uguale a 0")
		return in, false
	}
	return in, true
}

// ---------------------------------------------------------------------------
// Controllo accesso
// ---------------------------------------------------------------------------

// checkAccess: POST /api/integrations/check
//
// Ordine dei controlli (il primo che fallisce nega l'accesso):
//  1. risorsa abilitata            -> resource_disabled
//  2. agente attivo                -> agent_retired (dismesso) o agent_not_active
//  3. presente nell'allowlist      -> not_allowed
//  4. limite di chiamate al giorno -> daily_call_limit
//  5. limite di costo del mese     -> monthly_cost_limit
//
// Ogni decisione viene registrata in access_event. Il costo è contabilizzato solo se l'accesso è permesso.
// I controlli 4 e 5 sono fatti sotto un blocco sulla riga del limite, così due chiamate
// simultanee non superano il limite insieme.
func (s *Server) checkAccess(w http.ResponseWriter, r *http.Request) {
	var in CheckInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.AgentID <= 0 || in.ResourceID <= 0 {
		writeValidation(w, "agent_id e resource_id sono obbligatori")
		return
	}
	cost := 0.0
	if in.Cost != nil {
		if !validMoney(*in.Cost) {
			writeValidation(w, "cost deve essere maggiore o uguale a 0")
			return
		}
		cost = *in.Cost
	}
	res, serr := s.accessCheck(r.Context(), in.AgentID, in.ResourceID, cost)
	if serr != nil {
		writeSvcErr(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// accessCheck è il controllo di accesso riusabile (usato anche da /api/chat/complete).
// Registra la decisione in access_event. Il costo di un accesso negato è zero.
func (s *Server) accessCheck(ctx context.Context, agentID, resourceID int, cost float64) (CheckResult, *svcErr) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckResult{}, internalErr("checkAccess begin", err)
	}
	defer tx.Rollback()

	var agentStatus, resStatus string
	var revoked bool
	err = tx.QueryRowContext(ctx,
		`SELECT status, CASE WHEN identity_revoked_at IS NULL THEN 0 ELSE 1 END FROM dbo.agent WHERE id = @p1`,
		agentID).Scan(&agentStatus, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckResult{}, notFoundErr("agente")
	}
	if err != nil {
		return CheckResult{}, internalErr("checkAccess agent", err)
	}
	err = tx.QueryRowContext(ctx, `SELECT status FROM dbo.integration_resource WHERE id = @p1`, resourceID).Scan(&resStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckResult{}, notFoundErr("risorsa")
	}
	if err != nil {
		return CheckResult{}, internalErr("checkAccess resource", err)
	}

	decision, reason, approvalID := s.decide(ctx, tx, agentID, resourceID, agentStatus, revoked, resStatus, cost)
	if decision == "denied" {
		cost = 0 // un accesso negato non consuma budget
	}
	if err := writeAccessEvent(ctx, tx, agentID, resourceID, decision, reason, cost); err != nil {
		return CheckResult{}, internalErr("checkAccess event", err)
	}
	if err := tx.Commit(); err != nil {
		return CheckResult{}, internalErr("checkAccess commit", err)
	}
	return CheckResult{
		Decision: decision, Reason: reason, AgentID: agentID, ResourceID: resourceID,
		ApprovalID: approvalID,
	}, nil
}

// decide applica le regole di accesso e restituisce decisione e motivo.
// Va chiamata dentro la transazione di checkAccess.
func (s *Server) decide(ctx context.Context, tx *sql.Tx, agentID, resourceID int,
	agentStatus string, revoked bool, resStatus string, cost float64) (string, string, *int) {

	if resStatus != "enabled" {
		return "denied", "resource_disabled", nil
	}
	if revoked {
		// Identità revocata: nega prima di ogni altro controllo (tranne la risorsa disattivata).
		return "denied", "agent_revoked", nil
	}
	if agentStatus == "retired" {
		// Punto 3: un agente dismesso non ha più accessi (motivo distinto da agent_not_active).
		return "denied", "agent_retired", nil
	}
	if agentStatus != "active" {
		return "denied", "agent_not_active", nil
	}

	var requiresApproval bool
	err := tx.QueryRowContext(ctx,
		`SELECT requires_approval FROM dbo.agent_access WHERE agent_id = @p1 AND resource_id = @p2`,
		agentID, resourceID).Scan(&requiresApproval)
	if errors.Is(err, sql.ErrNoRows) {
		return "denied", "not_allowed", nil
	}
	if err != nil {
		// Un errore qui nega per sicurezza; il chiamante lo registra come motivo.
		return "denied", "check_error", nil
	}

	if d, r := limitDecision(ctx, tx, agentID, resourceID, cost); d != "allowed" {
		return d, r, nil
	}
	if requiresApproval {
		// Coppia sensibile: l'accesso passa solo con un'approvazione valida (vedi approvals.go).
		return approvalGate(ctx, tx, agentID, resourceID, 0)
	}
	return "allowed", "ok", nil
}

// listAccessLog: GET /api/integrations/access-log?agent_id=&limit=50 (massimo 500), dal più recente.
func (s *Server) listAccessLog(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	agentID, ok := optionalID(w, qs.Get("agent_id"), "agent_id")
	if !ok {
		return
	}
	limit := 50
	if v := qs.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeValidation(w, "limit deve essere tra 1 e 500")
			return
		}
		limit = n
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT TOP (@p2) id, at, agent_id, resource_id, decision, reason, cost
		   FROM dbo.access_event
		  WHERE (@p1 = 0 OR agent_id = @p1)
		  ORDER BY id DESC`, agentID, limit)
	if err != nil {
		serverError(w, "listAccessLog", err)
		return
	}
	defer rows.Close()

	out := []AccessEvent{}
	for rows.Next() {
		var e AccessEvent
		if err := rows.Scan(&e.ID, &e.At, &e.AgentID, &e.ResourceID, &e.Decision, &e.Reason, &e.Cost); err != nil {
			serverError(w, "listAccessLog scan", err)
			return
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listAccessLog rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Utilità
// ---------------------------------------------------------------------------

// optionalID legge un filtro numerico facoltativo: "" -> 0 (nessun filtro).
func optionalID(w http.ResponseWriter, raw, field string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		writeValidation(w, field+" non valido")
		return 0, false
	}
	return id, true
}

// writeLimitDBError traduce gli errori di scrittura dei limiti. Per i limiti la violazione
// di unicità riguarda la coppia agente-risorsa, quindi il messaggio è specifico.
func writeLimitDBError(w http.ResponseWriter, where string, err error) {
	var me mssql.Error
	var mep *mssql.Error
	var num int32
	switch {
	case errors.As(err, &me):
		num = me.Number
	case errors.As(err, &mep):
		num = mep.Number
	}
	if num == 2627 || num == 2601 {
		writeError(w, http.StatusConflict, "conflict", "coppia agente-risorsa già presente nei limiti")
		return
	}
	writeDBError(w, where, err)
}

// intParam e floatParam restituiscono NULL per i valori assenti.
func intParam(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func floatParam(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// limitDecision applica i limiti di chiamate al giorno e di costo al mese alla coppia agente-risorsa.
// Va chiamata dentro la transazione di decide. Restituisce "allowed" se nessun limite è superato.
func limitDecision(ctx context.Context, tx *sql.Tx, agentID, resourceID int, cost float64) (string, string) {
	// Blocco sulla riga del limite: serializza i controlli sulla stessa coppia.
	var maxCalls sql.NullInt64
	var maxCost sql.NullFloat64
	err := tx.QueryRowContext(ctx,
		`SELECT max_calls_per_day, max_cost_per_month FROM dbo.access_limit WITH (UPDLOCK, HOLDLOCK)
		  WHERE agent_id = @p1 AND resource_id = @p2`, agentID, resourceID).Scan(&maxCalls, &maxCost)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "denied", "check_error"
	}

	if maxCalls.Valid {
		var today int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM dbo.access_event
			  WHERE agent_id = @p1 AND resource_id = @p2 AND decision = 'allowed'
			    AND at >= CAST(SYSUTCDATETIME() AS DATE)`, agentID, resourceID).Scan(&today); err != nil {
			return "denied", "check_error"
		}
		if int64(today) >= maxCalls.Int64 {
			return "denied", "daily_call_limit"
		}
	}

	if maxCost.Valid {
		var spent float64
		if err := tx.QueryRowContext(ctx,
			`SELECT CAST(ISNULL(SUM(cost), 0) AS FLOAT) FROM dbo.access_event
			  WHERE agent_id = @p1 AND resource_id = @p2 AND decision = 'allowed'
			    AND at >= DATEFROMPARTS(YEAR(SYSUTCDATETIME()), MONTH(SYSUTCDATETIME()), 1)`,
			agentID, resourceID).Scan(&spent); err != nil {
			return "denied", "check_error"
		}
		if spent+cost > maxCost.Float64 {
			return "denied", "monthly_cost_limit"
		}
	}
	return "allowed", "ok"
}
