// Open-GenCut v0.1 - Punto 3: ciclo di vita degli agenti.
//
// Stati dell'agente (agent.status): registered -> active <-> suspended, poi retired.
//   - sospensione e riattivazione in qualsiasi momento (tra registered/active/suspended)
//   - dismissione (retired) è definitiva: blocca deploy, promozioni e nuovi accessi
//
// Ambienti: development, staging, production. Ogni ambiente ha una versione corrente.
//   - deploy: porta una versione registrata in un ambiente
//   - promote: copia la versione dell'ambiente precedente nel successivo
//     (development -> staging -> production)
//
// Ogni deploy, promozione e cambio di stato viene registrato in agent_deployment o in audit.
// Monitoraggio: ogni chiamata registrata (esito, latenza, token, costo) finisce in usage_log.
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
	validEnvironments = map[string]bool{"development": true, "staging": true, "production": true}
	// promotionSource indica da quale ambiente si promuove verso quello indicato.
	promotionSource = map[string]string{"staging": "development", "production": "staging"}
)

// ---------------------------------------------------------------------------
// Modelli JSON
// ---------------------------------------------------------------------------

// EnvironmentState è la versione corrente in un ambiente. Null se non ha ancora un deploy.
type EnvironmentState struct {
	VersionLabel string    `json:"version_label"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// LifecycleState è la vista di insieme di un agente nel ciclo di vita.
type LifecycleState struct {
	AgentID      int                          `json:"agent_id"`
	Status       string                       `json:"status"`
	Environments map[string]*EnvironmentState `json:"environments"`
	Versions     []string                     `json:"versions"`
}

// Deployment è una voce del registro di deploy e promozioni.
type Deployment struct {
	ID              int64     `json:"id"`
	AgentID         int       `json:"agent_id"`
	Action          string    `json:"action"` // deploy | promote
	Environment     string    `json:"environment"`
	FromEnvironment *string   `json:"from_environment"`
	VersionLabel    string    `json:"version_label"`
	Actor           string    `json:"actor"`
	Note            *string   `json:"note"`
	At              time.Time `json:"at"`
}

// DeployInput è il corpo di POST /deploy.
type DeployInput struct {
	Environment  string `json:"environment"`
	VersionLabel string `json:"version_label"`
	Note         string `json:"note"`
}

// PromoteInput è il corpo di POST /promote.
type PromoteInput struct {
	ToEnvironment string `json:"to_environment"`
	Note          string `json:"note"`
}

// VersionInput è il corpo di POST /versions.
type VersionInput struct {
	VersionLabel string `json:"version_label"`
}

// NoteInput è il corpo opzionale di suspend, reactivate e retire.
type NoteInput struct {
	Note string `json:"note"`
}

// CallInput è il corpo di POST /calls: il risultato di una chiamata già eseguita dall'agente.
type CallInput struct {
	Environment string  `json:"environment"` // facoltativo
	Success     *bool   `json:"success"`     // default true
	LatencyMS   *int    `json:"latency_ms"`
	Tokens      int     `json:"tokens"`
	Cost        float64 `json:"cost"`
}

// Performance è la risposta di GET /performance.
type Performance struct {
	AgentID      int     `json:"agent_id"`
	Environment  *string `json:"environment"`
	Calls        int     `json:"calls"`
	Errors       int     `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	MaxLatencyMS int     `json:"max_latency_ms"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
}

// ---------------------------------------------------------------------------
// Rotte
// ---------------------------------------------------------------------------

// registerLifecycle aggiunge le rotte del punto 3 al mux principale.
func (s *Server) registerLifecycle(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/lifecycle/agents/{id}", s.getLifecycle)
	mux.HandleFunc("GET /api/lifecycle/agents/{id}/versions", s.listVersions)
	mux.HandleFunc("POST /api/lifecycle/agents/{id}/versions", s.createVersion)
	mux.HandleFunc("POST /api/lifecycle/agents/{id}/deploy", s.deploy)
	mux.HandleFunc("POST /api/lifecycle/agents/{id}/promote", s.promote)
	mux.HandleFunc("GET /api/lifecycle/agents/{id}/deployments", s.listDeployments)
	mux.HandleFunc("POST /api/lifecycle/agents/{id}/suspend", s.suspendAgent)
	mux.HandleFunc("POST /api/lifecycle/agents/{id}/reactivate", s.reactivateAgent)
	mux.HandleFunc("POST /api/lifecycle/agents/{id}/retire", s.retireAgent)
	mux.HandleFunc("POST /api/lifecycle/agents/{id}/calls", s.recordCall)
	mux.HandleFunc("GET /api/lifecycle/agents/{id}/performance", s.performance)
}

// ---------------------------------------------------------------------------
// Stato
// ---------------------------------------------------------------------------

// getLifecycle: GET /api/lifecycle/agents/{id}
func (s *Server) getLifecycle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	st, err := s.fetchLifecycle(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "agente")
		return
	}
	if err != nil {
		serverError(w, "getLifecycle", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// fetchLifecycle legge stato dell'agente, ambienti e versioni.
func (s *Server) fetchLifecycle(ctx context.Context, agentID int) (LifecycleState, error) {
	var st LifecycleState
	st.AgentID = agentID
	err := s.db.QueryRowContext(ctx, `SELECT status FROM dbo.agent WHERE id = @p1`, agentID).Scan(&st.Status)
	if err != nil {
		return st, err
	}

	st.Environments = map[string]*EnvironmentState{"development": nil, "staging": nil, "production": nil}
	rows, err := s.db.QueryContext(ctx,
		`SELECT environment, version_label, updated_at FROM dbo.agent_environment WHERE agent_id = @p1`, agentID)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var env string
		var e EnvironmentState
		if err := rows.Scan(&env, &e.VersionLabel, &e.UpdatedAt); err != nil {
			rows.Close()
			return st, err
		}
		st.Environments[env] = &e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	st.Versions = []string{}
	vrows, err := s.db.QueryContext(ctx,
		`SELECT version_label FROM dbo.agent_version WHERE agent_id = @p1 ORDER BY created_at, id`, agentID)
	if err != nil {
		return st, err
	}
	defer vrows.Close()
	for vrows.Next() {
		var v string
		if err := vrows.Scan(&v); err != nil {
			return st, err
		}
		st.Versions = append(st.Versions, v)
	}
	return st, vrows.Err()
}

// ---------------------------------------------------------------------------
// Versioni
// ---------------------------------------------------------------------------

// listVersions: GET /api/lifecycle/agents/{id}/versions
func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT version_label FROM dbo.agent_version WHERE agent_id = @p1 ORDER BY created_at, id`, id)
	if err != nil {
		serverError(w, "listVersions", err)
		return
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			serverError(w, "listVersions scan", err)
			return
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listVersions rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createVersion: POST /api/lifecycle/agents/{id}/versions. Risponde 201.
func (s *Server) createVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in VersionInput
	if !decodeJSON(w, r, &in) {
		return
	}
	label := strings.TrimSpace(in.VersionLabel)
	if label == "" || runeLen(label) > 50 {
		writeValidation(w, "version_label è obbligatoria (massimo 50 caratteri)")
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createVersion begin", err)
		return
	}
	defer tx.Rollback()

	if _, err := lockAgent(r.Context(), tx, id); err != nil {
		agentLockError(w, "createVersion", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO dbo.agent_version (agent_id, version_label) VALUES (@p1, @p2)`, id, label); err != nil {
		writeLifecycleDBError(w, "createVersion insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "agent_version", id, auditDetail("version_registered", map[string]any{"version": label})); err != nil {
		serverError(w, "createVersion audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createVersion commit", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"agent_id": id, "version_label": label})
}

// ---------------------------------------------------------------------------
// Deploy e promozioni
// ---------------------------------------------------------------------------

// deploy: POST /api/lifecycle/agents/{id}/deploy
// Porta una versione già registrata in un ambiente. Risponde 201 con la voce del registro.
func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in DeployInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if !validEnvironments[in.Environment] {
		writeValidation(w, "environment deve essere development, staging o production")
		return
	}
	in.VersionLabel = strings.TrimSpace(in.VersionLabel)
	if in.VersionLabel == "" {
		writeValidation(w, "version_label è obbligatoria")
		return
	}
	if len(in.Note) > 500 {
		writeValidation(w, "note supera 500 caratteri")
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deploy begin", err)
		return
	}
	defer tx.Rollback()

	status, err := lockAgent(r.Context(), tx, id)
	if err != nil {
		agentLockError(w, "deploy", err)
		return
	}
	if status == "retired" {
		writeInvalidState(w, "agente dismesso: nessun deploy possibile")
		return
	}
	if status == "suspended" {
		writeInvalidState(w, "agente sospeso: nessuna nuova versione possibile")
		return
	}
	var exists int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM dbo.agent_version WHERE agent_id = @p1 AND version_label = @p2`,
		id, in.VersionLabel).Scan(&exists); err != nil {
		serverError(w, "deploy version check", err)
		return
	}
	if exists == 0 {
		writeValidation(w, "versione non registrata per questo agente")
		return
	}

	if reason, err := requireEvaluation(r.Context(), tx, id, in.Environment, in.VersionLabel); err != nil {
		serverError(w, "deploy gate", err)
		return
	} else if reason != "" {
		writeInvalidState(w, reason)
		return
	}

	d, err := recordDeployment(r.Context(), tx, id, "deploy", in.Environment, nil, in.VersionLabel, in.Note)
	if err != nil {
		serverError(w, "deploy record", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deploy commit", err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// promote: POST /api/lifecycle/agents/{id}/promote
// Copia la versione corrente dell'ambiente precedente nell'ambiente indicato.
// development -> staging, staging -> production. Risponde 201 con la voce del registro.
func (s *Server) promote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in PromoteInput
	if !decodeJSON(w, r, &in) {
		return
	}
	src, ok := promotionSource[in.ToEnvironment]
	if !ok {
		writeValidation(w, "to_environment deve essere staging o production")
		return
	}
	if len(in.Note) > 500 {
		writeValidation(w, "note supera 500 caratteri")
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "promote begin", err)
		return
	}
	defer tx.Rollback()

	status, err := lockAgent(r.Context(), tx, id)
	if err != nil {
		agentLockError(w, "promote", err)
		return
	}
	if status == "retired" {
		writeInvalidState(w, "agente dismesso: nessuna promozione possibile")
		return
	}
	if status == "suspended" {
		writeInvalidState(w, "agente sospeso: nessuna nuova versione possibile")
		return
	}

	var version string
	err = tx.QueryRowContext(r.Context(),
		`SELECT version_label FROM dbo.agent_environment WHERE agent_id = @p1 AND environment = @p2`,
		id, src).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		writeInvalidState(w, "nessuna versione in "+src+": impossibile promuovere verso "+in.ToEnvironment)
		return
	}
	if err != nil {
		serverError(w, "promote source", err)
		return
	}

	if reason, err := requireEvaluation(r.Context(), tx, id, in.ToEnvironment, version); err != nil {
		serverError(w, "promote gate", err)
		return
	} else if reason != "" {
		writeInvalidState(w, reason)
		return
	}

	d, err := recordDeployment(r.Context(), tx, id, "promote", in.ToEnvironment, &src, version, in.Note)
	if err != nil {
		serverError(w, "promote record", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "promote commit", err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// recordDeployment registra il deploy o la promozione, aggiorna la versione corrente
// dell'ambiente e scrive l'audit. Va chiamata dentro una transazione con l'agente bloccato.
func recordDeployment(ctx context.Context, tx *sql.Tx, agentID int, action, env string,
	from *string, version, note string) (Deployment, error) {

	var d Deployment
	var fromEnv sql.NullString
	var noteVal sql.NullString
	if from != nil {
		fromEnv = sql.NullString{String: *from, Valid: true}
	}
	if note != "" {
		noteVal = sql.NullString{String: note, Valid: true}
	}

	err := tx.QueryRowContext(ctx,
		`INSERT INTO dbo.agent_deployment (agent_id, action, environment, from_environment, version_label, note, actor)
		 OUTPUT inserted.id, inserted.agent_id, inserted.action, inserted.environment,
		        inserted.from_environment, inserted.version_label, inserted.actor, inserted.note, inserted.at
		 VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7)`,
		agentID, action, env, fromEnv, version, noteVal, actorFrom(ctx)).
		Scan(&d.ID, &d.AgentID, &d.Action, &d.Environment, scanNullStr(&d.FromEnvironment),
			&d.VersionLabel, &d.Actor, scanNullStr(&d.Note), &d.At)
	if err != nil {
		return d, err
	}

	// Versione corrente dell'ambiente: inserimento o aggiornamento.
	if _, err := tx.ExecContext(ctx,
		`MERGE dbo.agent_environment AS t
		 USING (SELECT @p1 AS agent_id, @p2 AS environment) AS s
		    ON t.agent_id = s.agent_id AND t.environment = s.environment
		 WHEN MATCHED THEN UPDATE SET version_label = @p3, updated_at = SYSUTCDATETIME()
		 WHEN NOT MATCHED THEN INSERT (agent_id, environment, version_label) VALUES (@p1, @p2, @p3);`,
		agentID, env, version); err != nil {
		return d, err
	}

	key, params := "deployment", map[string]any{"version": version, "environment": env}
	if from != nil {
		key, params = "promotion", map[string]any{"version": version, "from": *from, "to": env}
	}
	if err := writeAudit(ctx, tx, action, "agent_deployment", agentID, auditDetail(key, params)); err != nil {
		return d, err
	}
	return d, nil
}

// listDeployments: GET /api/lifecycle/agents/{id}/deployments?limit=50 (massimo 500), dal più recente.
func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
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
		`SELECT TOP (@p2) id, agent_id, action, environment, from_environment, version_label, actor, note, at
		   FROM dbo.agent_deployment WHERE agent_id = @p1 ORDER BY id DESC`, id, limit)
	if err != nil {
		serverError(w, "listDeployments", err)
		return
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.AgentID, &d.Action, &d.Environment, scanNullStr(&d.FromEnvironment),
			&d.VersionLabel, &d.Actor, scanNullStr(&d.Note), &d.At); err != nil {
			serverError(w, "listDeployments scan", err)
			return
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listDeployments rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Sospensione, riattivazione, dismissione
// ---------------------------------------------------------------------------

// suspendAgent: POST /api/lifecycle/agents/{id}/suspend. Da registered o active.
func (s *Server) suspendAgent(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "suspend", []string{"registered", "active"}, "suspended")
}

// reactivateAgent: POST /api/lifecycle/agents/{id}/reactivate. Solo da suspended.
func (s *Server) reactivateAgent(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "reactivate", []string{"suspended"}, "active")
}

// retireAgent: POST /api/lifecycle/agents/{id}/retire. Definitiva.
// Dopo la dismissione l'agente non può più essere promosso, deployato o riattivato,
// e /api/integrations/check nega i suoi accessi (motivo agent_retired).
func (s *Server) retireAgent(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "retire", []string{"registered", "active", "suspended"}, "retired")
}

// transition applica una transizione di stato consentita e la registra in audit.
func (s *Server) transition(w http.ResponseWriter, r *http.Request, action string, from []string, to string) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in NoteInput
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	if len(in.Note) > 500 {
		writeValidation(w, "note supera 500 caratteri")
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, action+" begin", err)
		return
	}
	defer tx.Rollback()

	current, err := lockAgent(r.Context(), tx, id)
	if err != nil {
		agentLockError(w, action, err)
		return
	}
	allowed := false
	for _, f := range from {
		if current == f {
			allowed = true
			break
		}
	}
	if !allowed {
		writeInvalidState(w, "transizione non consentita: "+action+" da "+current)
		return
	}

	if _, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.agent SET status = @p1, updated_at = SYSUTCDATETIME() WHERE id = @p2`, to, id); err != nil {
		serverError(w, action+" update", err)
		return
	}
	params := map[string]any{"from": current, "to": to}
	if in.Note != "" {
		params["note"] = in.Note
	}
	if err := writeAudit(r.Context(), tx, action, "agent", id, auditDetail("status_change", params)); err != nil {
		serverError(w, action+" audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, action+" commit", err)
		return
	}
	st, err := s.fetchLifecycle(r.Context(), id)
	if err != nil {
		serverError(w, action+" fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ---------------------------------------------------------------------------
// Monitoraggio
// ---------------------------------------------------------------------------

// recordCall: POST /api/lifecycle/agents/{id}/calls. Registra una chiamata già eseguita. Risponde 201.
func (s *Server) recordCall(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in CallInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Environment != "" && !validEnvironments[in.Environment] {
		writeValidation(w, "environment deve essere development, staging o production")
		return
	}
	if in.Tokens < 0 || in.Tokens > maxInt32 {
		writeValidation(w, "tokens deve essere maggiore o uguale a 0")
		return
	}
	if in.LatencyMS != nil && (*in.LatencyMS < 0 || *in.LatencyMS > maxInt32) {
		writeValidation(w, "latency_ms deve essere maggiore o uguale a 0")
		return
	}
	if !validMoney(in.Cost) {
		writeValidation(w, "cost deve essere maggiore o uguale a 0")
		return
	}
	success := true
	if in.Success != nil {
		success = *in.Success
	}

	var exists int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM dbo.agent WHERE id = @p1`, id).Scan(&exists); err != nil {
		serverError(w, "recordCall agent", err)
		return
	}
	if exists == 0 {
		writeNotFound(w, "agente")
		return
	}

	var latency any
	if in.LatencyMS != nil {
		latency = *in.LatencyMS
	}
	if _, err := s.db.ExecContext(r.Context(),
		`INSERT INTO dbo.usage_log (agent_id, environment, tokens, cost, success, latency_ms)
		 VALUES (@p1, @p2, @p3, @p4, @p5, @p6)`,
		id, nullable(in.Environment), in.Tokens, in.Cost, success, latency); err != nil {
		serverError(w, "recordCall insert", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"agent_id": id, "recorded": true})
}

// performance: GET /api/lifecycle/agents/{id}/performance?environment=&from=&to=
// from e to sono giorni YYYY-MM-DD (estremi inclusi).
func (s *Server) performance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	qs := r.URL.Query()
	env := qs.Get("environment")
	if env != "" && !validEnvironments[env] {
		writeValidation(w, "environment deve essere development, staging o production")
		return
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

	var p Performance
	p.AgentID = id
	if env != "" {
		p.Environment = &env
	}
	var errs sql.NullInt64
	var avg sql.NullFloat64
	var maxLat sql.NullInt64
	var tokens sql.NullInt64
	var cost sql.NullFloat64
	err = s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*),
		        SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END),
		        AVG(CAST(latency_ms AS FLOAT)),
		        MAX(latency_ms),
		        SUM(CAST(tokens AS BIGINT)),
		        SUM(cost)
		   FROM dbo.usage_log
		  WHERE agent_id = @p1
		    AND (@p2 = N'' OR environment = @p2)
		    AND (@p3 IS NULL OR called_at >= CAST(@p3 AS DATE))
		    AND (@p4 IS NULL OR called_at < DATEADD(DAY, 1, CAST(@p4 AS DATE)))`,
		id, env, from, to).Scan(&p.Calls, &errs, &avg, &maxLat, &tokens, &cost)
	if err != nil {
		serverError(w, "performance", err)
		return
	}
	p.Errors = int(errs.Int64)
	if p.Calls > 0 {
		p.ErrorRate = float64(p.Errors) / float64(p.Calls)
	}
	p.AvgLatencyMS = avg.Float64
	p.MaxLatencyMS = int(maxLat.Int64)
	p.Tokens = tokens.Int64
	p.Cost = cost.Float64
	writeJSON(w, http.StatusOK, p)
}

// ---------------------------------------------------------------------------
// Utilità
// ---------------------------------------------------------------------------

// lockAgent legge lo stato dell'agente con blocco di aggiornamento, per serializzare
// le scritture del ciclo di vita sullo stesso agente.
func lockAgent(ctx context.Context, tx *sql.Tx, id int) (string, error) {
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM dbo.agent WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&status)
	return status, err
}

func agentLockError(w http.ResponseWriter, where string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "agente")
		return
	}
	serverError(w, where, err)
}

// writeInvalidState risponde 409 con codice invalid_state.
func writeInvalidState(w http.ResponseWriter, message string) {
	writeError(w, http.StatusConflict, "invalid_state", message)
}

// writeLifecycleDBError gestisce la violazione di unicità delle versioni.
func writeLifecycleDBError(w http.ResponseWriter, where string, err error) {
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
		writeError(w, http.StatusConflict, "conflict", "versione già registrata per questo agente")
		return
	}
	writeDBError(w, where, err)
}

// scanNullStr è un adattatore: legge una colonna nullable direttamente in *string.
func scanNullStr(dst **string) *nullStrScanner {
	return &nullStrScanner{dst: dst}
}

type nullStrScanner struct {
	dst **string
}

func (n *nullStrScanner) Scan(src any) error {
	var ns sql.NullString
	if err := ns.Scan(src); err != nil {
		return err
	}
	*n.dst = ptrOf(ns)
	return nil
}
