// Open-GenCut - Punto 6 della specifica: valutazione degli agenti.
//
// Tracce di esecuzione con passi (llm, tool, reasoning, memory) in stile OpenTelemetry:
// esito, durata, token. Il testo dei passi non viene mai salvato.
// Regole di valutazione continua: le violazioni vengono calcolate all'ingestione della traccia.
// Punteggi di giudizio di un giudice esterno (manuale o modello esterno): la piattaforma non esegue giudizi.
//
// Limite dichiarato: l'ingestione nel formato OTLP non è inclusa.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

var (
	traceOutcomes = map[string]bool{"success": true, "failure": true, "partial": true}
	stepTypes     = map[string]bool{"llm": true, "tool": true, "reasoning": true, "memory": true}
	evalRuleTypes = map[string]bool{"max_tokens_per_trace": true, "max_latency_ms": true, "max_error_rate": true,
		"drift_threshold": true, "memory_stale_days": true}
)

const (
	driftWindow      = 10  // tracce per finestra nel confronto di drift
	defaultStaleDays = 30  // giorni oltre i quali la memoria è stale, se non c'è una regola
	defaultDrift     = 0.2 // soglia di drift, se non c'è una regola
)

// ---------------------------------------------------------------------------
// Modelli
// ---------------------------------------------------------------------------

// TraceStepInput è un passo di una traccia.
type TraceStepInput struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	Tokens        int    `json:"tokens"`
	LatencyMS     int    `json:"latency_ms"`
	OK            *bool  `json:"ok"` // default true
	MemoryAgeDays *int   `json:"memory_age_days"`
}

// TraceInput è il corpo di POST /api/evaluation/traces.
type TraceInput struct {
	AgentID     int              `json:"agent_id"`
	Environment string           `json:"environment"`
	TraceID     string           `json:"trace_id"`
	Outcome     string           `json:"outcome"`
	GoalMet     bool             `json:"goal_met"`
	Cost        float64          `json:"cost"`
	Steps       []TraceStepInput `json:"steps"`
}

// Trace è una traccia registrata (senza testo dei passi).
type Trace struct {
	ID             int64     `json:"id"`
	AgentID        int       `json:"agent_id"`
	Environment    *string   `json:"environment"`
	TraceID        string    `json:"trace_id"`
	Outcome        string    `json:"outcome"`
	GoalMet        bool      `json:"goal_met"`
	Cost           float64   `json:"cost"`
	TotalTokens    int       `json:"total_tokens"`
	TotalLatencyMS int       `json:"total_latency_ms"`
	StepErrors     int       `json:"step_errors"`
	Steps          int       `json:"steps"`
	CreatedAt      time.Time `json:"created_at"`
}

// EvalRule è una regola di valutazione continua.
type EvalRule struct {
	ID      int             `json:"id"`
	Type    string          `json:"type"`
	Params  json.RawMessage `json:"params"`
	Enabled bool            `json:"enabled"`
}

// EvalRuleInput è il corpo di POST e PUT delle regole.
type EvalRuleInput struct {
	Type    string          `json:"type"`
	Params  json.RawMessage `json:"params"`
	Enabled *bool           `json:"enabled"`
}

// ScoreInput è il corpo di POST /api/evaluation/agents/{id}/scores.
type ScoreInput struct {
	Score int    `json:"score"`
	Judge string `json:"judge"`
	Note  string `json:"note"`
}

// EvalViolation è una violazione di regola.
type EvalViolation struct {
	ID       int64           `json:"id"`
	AgentID  int             `json:"agent_id"`
	RuleType string          `json:"rule_type"`
	TraceRef *int64          `json:"trace_ref"`
	Detail   string          `json:"detail"`
	Params   json.RawMessage `json:"params"`
	At       time.Time       `json:"at"`
}

// EvalSummary è il riepilogo di valutazione di un agente.
type EvalSummary struct {
	AgentID           int      `json:"agent_id"`
	Traces            int      `json:"traces"`
	SuccessRate       float64  `json:"success_rate"`
	GoalRate          float64  `json:"goal_rate"`
	AvgLatencyMS      float64  `json:"avg_latency_ms"`
	AvgTokensPerTrace float64  `json:"avg_tokens_per_trace"`
	ErrorRate         float64  `json:"error_rate"`
	RuleViolations    int      `json:"rule_violations"`
	AvgScore          *float64 `json:"avg_score"`
	Scores            int      `json:"scores"`
	DriftFlag         bool     `json:"drift_flag"`
	DriftDelta        float64  `json:"drift_delta"`
	StaleMemorySteps  int      `json:"stale_memory_steps"`
}

// ---------------------------------------------------------------------------
// Rotte
// ---------------------------------------------------------------------------

// registerEvaluation aggiunge le rotte di valutazione al mux principale.
func (s *Server) registerEvaluation(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/evaluation/traces", s.createTrace)
	mux.HandleFunc("GET /api/evaluation/traces", s.listTraces)
	mux.HandleFunc("GET /api/evaluation/rules", s.listEvalRules)
	mux.HandleFunc("POST /api/evaluation/rules", s.createEvalRule)
	mux.HandleFunc("GET /api/evaluation/rules/{id}", s.getEvalRule)
	mux.HandleFunc("PUT /api/evaluation/rules/{id}", s.updateEvalRule)
	mux.HandleFunc("DELETE /api/evaluation/rules/{id}", s.deleteEvalRule)
	mux.HandleFunc("POST /api/evaluation/agents/{id}/scores", s.createScore)
	mux.HandleFunc("GET /api/evaluation/agents/{id}/summary", s.evalSummary)
	mux.HandleFunc("GET /api/evaluation/agents/{id}/violations", s.listViolations)
}

// ---------------------------------------------------------------------------
// Tracce
// ---------------------------------------------------------------------------

// validateTrace controlla la traccia in ingresso.
func validateTrace(in *TraceInput) string {
	in.TraceID = strings.TrimSpace(in.TraceID)
	if in.AgentID <= 0 {
		return "agent_id è obbligatorio"
	}
	if in.TraceID == "" || runeLen(in.TraceID) > 100 {
		return "trace_id è obbligatorio (massimo 100 caratteri)"
	}
	if !traceOutcomes[in.Outcome] {
		return "outcome deve essere success, failure o partial"
	}
	if in.Environment != "" && !validEnvironments[in.Environment] {
		return "environment non valido"
	}
	if !validMoney(in.Cost) {
		return "cost deve essere maggiore o uguale a 0 nei limiti del database"
	}
	if len(in.Steps) > 500 {
		return "una traccia non può avere più di 500 passi"
	}
	for i, st := range in.Steps {
		if !stepTypes[st.Type] {
			return fmt.Sprintf("passo %d: type deve essere llm, tool, reasoning o memory", i+1)
		}
		if runeLen(st.Name) == 0 || runeLen(st.Name) > 150 {
			return fmt.Sprintf("passo %d: name è obbligatorio (massimo 150 caratteri)", i+1)
		}
		if st.Tokens < 0 || st.Tokens > maxInt32 || st.LatencyMS < 0 || st.LatencyMS > maxInt32 {
			return fmt.Sprintf("passo %d: tokens o latency_ms fuori dai limiti", i+1)
		}
		if st.MemoryAgeDays != nil && (*st.MemoryAgeDays < 0 || *st.MemoryAgeDays > 36500) {
			return fmt.Sprintf("passo %d: memory_age_days fuori dai limiti", i+1)
		}
	}
	return ""
}

// createTrace: POST /api/evaluation/traces. Risponde 201. Calcola le violazioni delle regole attive.
func (s *Server) createTrace(w http.ResponseWriter, r *http.Request) {
	var in TraceInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if msg := validateTrace(&in); msg != "" {
		writeValidation(w, msg)
		return
	}
	t, serr := s.recordTrace(r.Context(), in)
	if serr != nil {
		writeSvcErr(w, serr)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// recordTrace registra una traccia con i suoi passi, calcola le violazioni e scrive l'audit.
// È riusata dall'handler HTTP e dall'ingestione OTLP. Il corpo deve essere già validato.
func (s *Server) recordTrace(ctx context.Context, in TraceInput) (Trace, *svcErr) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Trace{}, internalErr("createTrace begin", err)
	}
	defer tx.Rollback()

	if _, err := lockAgent(ctx, tx, in.AgentID); err != nil {
		return Trace{}, agentErr("createTrace agent", err)
	}

	// Totali della traccia.
	var tokens, latency, errs int
	for _, st := range in.Steps {
		tokens += st.Tokens
		latency += st.LatencyMS
		if st.OK != nil && !*st.OK {
			errs++ // passo non riuscito
		}
	}
	env := sql.NullString{}
	if in.Environment != "" {
		env = sql.NullString{String: in.Environment, Valid: true}
	}
	var traceRef int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO dbo.eval_trace (agent_id, environment, trace_id, outcome, goal_met, cost,
		                            total_tokens, total_latency_ms, step_errors)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9)`,
		in.AgentID, env, in.TraceID, in.Outcome, in.GoalMet, in.Cost, tokens, latency, errs).Scan(&traceRef)
	if err != nil {
		return Trace{}, dbErr("createTrace insert", err)
	}
	for _, st := range in.Steps {
		ok := true
		if st.OK != nil {
			ok = *st.OK
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO dbo.eval_trace_step (trace_ref, step_type, name, tokens, latency_ms, ok, memory_age_days)
			 VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7)`,
			traceRef, st.Type, st.Name, st.Tokens, st.LatencyMS, ok, intParam(st.MemoryAgeDays)); err != nil {
			return Trace{}, internalErr("createTrace step", err)
		}
	}

	// Violazioni: regole attive, valutate sulla traccia appena inserita.
	if err := s.checkRules(ctx, tx, in.AgentID, traceRef); err != nil {
		return Trace{}, internalErr("createTrace rules", err)
	}

	if err := writeAudit(ctx, tx, "create", "eval_trace", in.AgentID,
		auditDetail("trace_recorded", map[string]any{"trace_id": in.TraceID, "outcome": in.Outcome})); err != nil {
		return Trace{}, internalErr("createTrace audit", err)
	}
	if err := tx.Commit(); err != nil {
		return Trace{}, internalErr("createTrace commit", err)
	}
	t, err := s.fetchTrace(ctx, traceRef)
	if err != nil {
		return Trace{}, internalErr("createTrace fetch", err)
	}
	return t, nil
}

// dbErr e agentErr traducono gli errori di scrittura in errori di servizio.
func dbErr(where string, err error) *svcErr {
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
		return &svcErr{status: http.StatusConflict, code: "conflict", msg: "valore già presente (chiave duplicata)"}
	case 547, 515:
		return &svcErr{status: http.StatusBadRequest, code: "validation_error", msg: "dati non validi per i vincoli del database"}
	}
	return internalErr(where, err)
}

func agentErr(where string, err error) *svcErr {
	if errors.Is(err, sql.ErrNoRows) {
		return notFoundErr("agente")
	}
	return internalErr(where, err)
}

// checkRules valuta le regole attive sulla traccia appena inserita e registra le violazioni.
func (s *Server) checkRules(ctx context.Context, tx *sql.Tx, agentID int, traceRef int64) error {
	rules, err := loadEnabledRules(ctx, tx)
	if err != nil {
		return err
	}
	var tokens, latency int
	if err := tx.QueryRowContext(ctx,
		`SELECT total_tokens, total_latency_ms FROM dbo.eval_trace WHERE id = @p1`, traceRef).Scan(&tokens, &latency); err != nil {
		return err
	}
	for _, rule := range rules {
		var p map[string]json.RawMessage
		_ = json.Unmarshal(rule.Params, &p)
		var detail string
		var vparams map[string]any
		switch rule.Type {
		case "max_tokens_per_trace":
			var max int
			_ = json.Unmarshal(p["max"], &max)
			if max > 0 && tokens > max {
				detail = fmt.Sprintf("token %d oltre il massimo %d", tokens, max)
				vparams = map[string]any{"value": tokens, "max": max}
			}
		case "max_latency_ms":
			var max int
			_ = json.Unmarshal(p["max"], &max)
			if max > 0 && latency > max {
				detail = fmt.Sprintf("latenza %d ms oltre il massimo %d ms", latency, max)
				vparams = map[string]any{"value": latency, "max": max}
			}
		case "memory_stale_days":
			var maxDays int
			if err := json.Unmarshal(p["max_days"], &maxDays); err != nil {
				maxDays = defaultStaleDays
			}
			var stale int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM dbo.eval_trace_step WHERE trace_ref = @p1 AND step_type = 'memory'
				    AND memory_age_days > @p2`, traceRef, maxDays).Scan(&stale); err != nil {
				return err
			}
			if stale > 0 {
				detail = fmt.Sprintf("%d passi di memoria più vecchi di %d giorni", stale, maxDays)
				vparams = map[string]any{"count": stale, "max_days": maxDays}
			}
		case "max_error_rate":
			var max float64
			_ = json.Unmarshal(p["max"], &max)
			rate, n, err := recentFailureRate(ctx, tx, agentID)
			if err != nil {
				return err
			}
			if n > 0 && rate > max {
				detail = fmt.Sprintf("tasso di errore %.2f oltre il massimo %.2f (ultime %d tracce)", rate, max, n)
				vparams = map[string]any{"rate": rate, "max": max}
			}
		case "drift_threshold":
			var maxDelta float64
			if err := json.Unmarshal(p["max_delta"], &maxDelta); err != nil {
				maxDelta = defaultDrift
			}
			delta, ok, err := driftDelta(ctx, tx, agentID)
			if err != nil {
				return err
			}
			if ok && math.Abs(delta) > maxDelta {
				detail = fmt.Sprintf("drift del tasso di successo %.2f oltre la soglia %.2f", delta, maxDelta)
				vparams = map[string]any{"delta": delta, "max_delta": maxDelta}
			}
		}
		if detail != "" {
			pj, _ := json.Marshal(vparams)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO dbo.eval_violation (agent_id, rule_type, trace_ref, detail, params) VALUES (@p1, @p2, @p3, @p4, @p5)`,
				agentID, rule.Type, traceRef, detail, string(pj)); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadEnabledRules legge le regole attive.
func loadEnabledRules(ctx context.Context, tx *sql.Tx) ([]EvalRule, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, type, params, enabled FROM dbo.eval_rule WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvalRule
	for rows.Next() {
		var rl EvalRule
		var params string
		if err := rows.Scan(&rl.ID, &rl.Type, &params, &rl.Enabled); err != nil {
			return nil, err
		}
		rl.Params = json.RawMessage(params)
		out = append(out, rl)
	}
	return out, rows.Err()
}

// recentFailureRate calcola il tasso di fallimento delle ultime driftWindow tracce.
func recentFailureRate(ctx context.Context, tx *sql.Tx, agentID int) (float64, int, error) {
	var fails, n int
	err := tx.QueryRowContext(ctx,
		`SELECT SUM(CASE WHEN outcome = 'failure' THEN 1 ELSE 0 END), COUNT(*)
		   FROM (SELECT TOP (@p2) outcome FROM dbo.eval_trace WHERE agent_id = @p1 ORDER BY id DESC) t`,
		agentID, driftWindow).Scan(&fails, &n)
	if err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, nil
	}
	return float64(fails) / float64(n), n, nil
}

// driftDelta confronta il tasso di successo delle ultime driftWindow tracce con le driftWindow precedenti.
// ok è falso se non ci sono abbastanza tracce per il confronto.
func driftDelta(ctx context.Context, tx *sql.Tx, agentID int) (float64, bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT outcome FROM dbo.eval_trace WHERE agent_id = @p1 ORDER BY id DESC`, agentID)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var outcomes []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return 0, false, err
		}
		outcomes = append(outcomes, o)
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	if len(outcomes) < 2*driftWindow {
		return 0, false, nil
	}
	recent := successRate(outcomes[:driftWindow])
	previous := successRate(outcomes[driftWindow : 2*driftWindow])
	return recent - previous, true, nil
}

func successRate(outcomes []string) float64 {
	if len(outcomes) == 0 {
		return 0
	}
	ok := 0
	for _, o := range outcomes {
		if o == "success" {
			ok++
		}
	}
	return float64(ok) / float64(len(outcomes))
}

// fetchTrace legge una traccia con il numero di passi.
func (s *Server) fetchTrace(ctx context.Context, id int64) (Trace, error) {
	var t Trace
	var env sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT t.id, t.agent_id, t.environment, t.trace_id, t.outcome, t.goal_met, t.cost,
		        t.total_tokens, t.total_latency_ms, t.step_errors, t.created_at,
		        (SELECT COUNT(*) FROM dbo.eval_trace_step s WHERE s.trace_ref = t.id)
		   FROM dbo.eval_trace t WHERE t.id = @p1`, id).
		Scan(&t.ID, &t.AgentID, &env, &t.TraceID, &t.Outcome, &t.GoalMet, &t.Cost,
			&t.TotalTokens, &t.TotalLatencyMS, &t.StepErrors, &t.CreatedAt, &t.Steps)
	t.Environment = ptrOf(env)
	return t, err
}

// listTraces: GET /api/evaluation/traces?agent_id=&limit=50 (massimo 500), dal più recente.
func (s *Server) listTraces(w http.ResponseWriter, r *http.Request) {
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
		`SELECT TOP (@p2) t.id FROM dbo.eval_trace t WHERE (@p1 = 0 OR t.agent_id = @p1) ORDER BY t.id DESC`,
		agentID, limit)
	if err != nil {
		serverError(w, "listTraces", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			serverError(w, "listTraces scan", err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []Trace{}
	for _, id := range ids {
		t, err := s.fetchTrace(r.Context(), id)
		if err != nil {
			serverError(w, "listTraces fetch", err)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Regole
// ---------------------------------------------------------------------------

// validateEvalRule controlla il tipo e i parametri della regola.
func validateEvalRule(typ string, raw json.RawMessage) (json.RawMessage, string) {
	if !evalRuleTypes[typ] {
		return nil, "type non valido"
	}
	params, err := normalizeObject(raw)
	if err != nil {
		return nil, "params deve essere un oggetto JSON"
	}
	var p map[string]json.RawMessage
	_ = json.Unmarshal(params, &p)
	switch typ {
	case "max_tokens_per_trace", "max_latency_ms":
		var v int
		if err := json.Unmarshal(p["max"], &v); err != nil || v < 1 || v > maxInt32 {
			return nil, typ + ` richiede params {"max": intero positivo}`
		}
	case "max_error_rate":
		var v float64
		if err := json.Unmarshal(p["max"], &v); err != nil || v < 0 || v > 1 {
			return nil, `max_error_rate richiede params {"max": valore tra 0 e 1}`
		}
	case "drift_threshold":
		var v float64
		if err := json.Unmarshal(p["max_delta"], &v); err != nil || v < 0 || v > 1 {
			return nil, `drift_threshold richiede params {"max_delta": valore tra 0 e 1}`
		}
	case "memory_stale_days":
		var v int
		if err := json.Unmarshal(p["max_days"], &v); err != nil || v < 0 || v > 36500 {
			return nil, `memory_stale_days richiede params {"max_days": intero tra 0 e 36500}`
		}
	}
	return params, ""
}

// EvalRule: scansione unificata.
func scanEvalRule(sc rowScanner) (EvalRule, error) {
	var rl EvalRule
	var params string
	if err := sc.Scan(&rl.ID, &rl.Type, &params, &rl.Enabled); err != nil {
		return EvalRule{}, err
	}
	rl.Params = json.RawMessage(params)
	return rl, nil
}

func (s *Server) fetchEvalRule(ctx context.Context, id int) (EvalRule, error) {
	return scanEvalRule(s.db.QueryRowContext(ctx,
		`SELECT id, type, params, enabled FROM dbo.eval_rule WHERE id = @p1`, id))
}

// listEvalRules: GET /api/evaluation/rules
func (s *Server) listEvalRules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, type, params, enabled FROM dbo.eval_rule ORDER BY id`)
	if err != nil {
		serverError(w, "listEvalRules", err)
		return
	}
	defer rows.Close()
	out := []EvalRule{}
	for rows.Next() {
		rl, err := scanEvalRule(rows)
		if err != nil {
			serverError(w, "listEvalRules scan", err)
			return
		}
		out = append(out, rl)
	}
	writeJSON(w, http.StatusOK, out)
}

// getEvalRule: GET /api/evaluation/rules/{id}
func (s *Server) getEvalRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rl, err := s.fetchEvalRule(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "regola")
		return
	}
	if err != nil {
		serverError(w, "getEvalRule", err)
		return
	}
	writeJSON(w, http.StatusOK, rl)
}

// createEvalRule: POST /api/evaluation/rules. Risponde 201. Un tipo di regola compare una sola volta.
func (s *Server) createEvalRule(w http.ResponseWriter, r *http.Request) {
	var in EvalRuleInput
	if !decodeJSON(w, r, &in) {
		return
	}
	params, msg := validateEvalRule(in.Type, in.Params)
	if msg != "" {
		writeValidation(w, msg)
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createEvalRule begin", err)
		return
	}
	defer tx.Rollback()
	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.eval_rule (type, params, enabled) OUTPUT inserted.id VALUES (@p1, @p2, @p3)`,
		in.Type, string(params), enabled).Scan(&id)
	if err != nil {
		writeDBError(w, "createEvalRule insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "eval_rule", id,
		auditDetail("eval_rule_saved", map[string]any{"rule_type": in.Type})); err != nil {
		serverError(w, "createEvalRule audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createEvalRule commit", err)
		return
	}
	rl, err := s.fetchEvalRule(r.Context(), id)
	if err != nil {
		serverError(w, "createEvalRule fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, rl)
}

// updateEvalRule: PUT /api/evaluation/rules/{id}. Sostituzione completa.
func (s *Server) updateEvalRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in EvalRuleInput
	if !decodeJSON(w, r, &in) {
		return
	}
	params, msg := validateEvalRule(in.Type, in.Params)
	if msg != "" {
		writeValidation(w, msg)
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateEvalRule begin", err)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.eval_rule SET type = @p1, params = @p2, enabled = @p3, updated_at = SYSUTCDATETIME() WHERE id = @p4`,
		in.Type, string(params), enabled, id)
	if err != nil {
		writeDBError(w, "updateEvalRule update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "regola")
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "eval_rule", id,
		auditDetail("eval_rule_saved", map[string]any{"rule_type": in.Type})); err != nil {
		serverError(w, "updateEvalRule audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateEvalRule commit", err)
		return
	}
	rl, err := s.fetchEvalRule(r.Context(), id)
	if err != nil {
		serverError(w, "updateEvalRule fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, rl)
}

// deleteEvalRule: DELETE /api/evaluation/rules/{id}. Risponde 204.
func (s *Server) deleteEvalRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteEvalRule begin", err)
		return
	}
	defer tx.Rollback()
	var typ string
	err = tx.QueryRowContext(r.Context(),
		`SELECT type FROM dbo.eval_rule WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&typ)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "regola")
		return
	}
	if err != nil {
		serverError(w, "deleteEvalRule read", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.eval_rule WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteEvalRule delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "eval_rule", id,
		auditDetail("eval_rule_deleted", map[string]any{"rule_type": typ})); err != nil {
		serverError(w, "deleteEvalRule audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteEvalRule commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Punteggi, riepilogo, violazioni
// ---------------------------------------------------------------------------

// createScore: POST /api/evaluation/agents/{id}/scores. Risponde 201.
func (s *Server) createScore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in ScoreInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Judge = strings.TrimSpace(in.Judge)
	if in.Score < 1 || in.Score > 5 {
		writeValidation(w, "score deve essere tra 1 e 5")
		return
	}
	if in.Judge == "" || runeLen(in.Judge) > 100 {
		writeValidation(w, "judge è obbligatorio (massimo 100 caratteri)")
		return
	}
	if runeLen(in.Note) > 500 {
		writeValidation(w, "note supera 500 caratteri")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createScore begin", err)
		return
	}
	defer tx.Rollback()
	if _, err := lockAgent(r.Context(), tx, id); err != nil {
		agentLockError(w, "createScore agent", err)
		return
	}
	var sid int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.eval_score (agent_id, score, judge, note) OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4)`,
		id, in.Score, in.Judge, nullable(in.Note)).Scan(&sid)
	if err != nil {
		serverError(w, "createScore insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "eval_score", id,
		auditDetail("score_recorded", map[string]any{"score": in.Score, "judge": in.Judge})); err != nil {
		serverError(w, "createScore audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createScore commit", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": sid, "agent_id": id, "score": in.Score, "judge": in.Judge})
}

// evalSummary: GET /api/evaluation/agents/{id}/summary
func (s *Server) evalSummary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dbo.agent WHERE id = @p1`, id).Scan(&n); err != nil {
		serverError(w, "evalSummary agent", err)
		return
	}
	if n == 0 {
		writeNotFound(w, "agente")
		return
	}

	sum := EvalSummary{AgentID: id}
	var succ, goal, fails sql.NullInt64
	var avgLat, avgTok sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        SUM(CASE WHEN outcome = 'success' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN goal_met = 1 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN outcome = 'failure' THEN 1 ELSE 0 END),
		        AVG(CAST(total_latency_ms AS FLOAT)),
		        AVG(CAST(total_tokens AS FLOAT))
		   FROM dbo.eval_trace WHERE agent_id = @p1`, id).
		Scan(&sum.Traces, &succ, &goal, &fails, &avgLat, &avgTok)
	if err != nil {
		serverError(w, "evalSummary traces", err)
		return
	}
	if sum.Traces > 0 {
		sum.SuccessRate = float64(succ.Int64) / float64(sum.Traces)
		sum.GoalRate = float64(goal.Int64) / float64(sum.Traces)
		sum.ErrorRate = float64(fails.Int64) / float64(sum.Traces)
		sum.AvgLatencyMS = avgLat.Float64
		sum.AvgTokensPerTrace = avgTok.Float64
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dbo.eval_violation WHERE agent_id = @p1`, id).Scan(&sum.RuleViolations); err != nil {
		serverError(w, "evalSummary violations", err)
		return
	}

	var avg sql.NullFloat64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), AVG(CAST(score AS FLOAT)) FROM dbo.eval_score WHERE agent_id = @p1`, id).
		Scan(&sum.Scores, &avg); err != nil {
		serverError(w, "evalSummary scores", err)
		return
	}
	if avg.Valid {
		v := avg.Float64
		sum.AvgScore = &v
	}

	// Drift: confronto delle ultime driftWindow tracce con le precedenti; soglia dalla regola o default.
	rows, err := s.db.QueryContext(ctx,
		`SELECT outcome FROM dbo.eval_trace WHERE agent_id = @p1 ORDER BY id DESC`, id)
	if err != nil {
		serverError(w, "evalSummary drift", err)
		return
	}
	var outcomes []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			rows.Close()
			serverError(w, "evalSummary drift scan", err)
			return
		}
		outcomes = append(outcomes, o)
	}
	rows.Close()
	threshold := defaultDrift
	if rl, ok, err := s.ruleOfType(ctx, "drift_threshold"); err != nil {
		serverError(w, "evalSummary rule", err)
		return
	} else if ok {
		var p struct {
			MaxDelta float64 `json:"max_delta"`
		}
		_ = json.Unmarshal(rl.Params, &p)
		threshold = p.MaxDelta
	}
	if len(outcomes) >= 2*driftWindow {
		sum.DriftDelta = successRate(outcomes[:driftWindow]) - successRate(outcomes[driftWindow:2*driftWindow])
		sum.DriftFlag = math.Abs(sum.DriftDelta) > threshold
	}

	// Passi di memoria stale: più vecchi della soglia della regola (o del default).
	staleDays := defaultStaleDays
	if rl, ok, err := s.ruleOfType(ctx, "memory_stale_days"); err != nil {
		serverError(w, "evalSummary stale rule", err)
		return
	} else if ok {
		var p struct {
			MaxDays int `json:"max_days"`
		}
		_ = json.Unmarshal(rl.Params, &p)
		staleDays = p.MaxDays
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dbo.eval_trace_step st
		   JOIN dbo.eval_trace t ON t.id = st.trace_ref
		  WHERE t.agent_id = @p1 AND st.step_type = 'memory' AND st.memory_age_days > @p2`,
		id, staleDays).Scan(&sum.StaleMemorySteps); err != nil {
		serverError(w, "evalSummary stale", err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// ruleOfType restituisce la regola attiva di un tipo, se esiste.
func (s *Server) ruleOfType(ctx context.Context, typ string) (EvalRule, bool, error) {
	rl, err := scanEvalRule(s.db.QueryRowContext(ctx,
		`SELECT id, type, params, enabled FROM dbo.eval_rule WHERE type = @p1 AND enabled = 1`, typ))
	if errors.Is(err, sql.ErrNoRows) {
		return EvalRule{}, false, nil
	}
	return rl, err == nil, err
}

// listViolations: GET /api/evaluation/agents/{id}/violations?limit=50 (massimo 500), dal più recente.
func (s *Server) listViolations(w http.ResponseWriter, r *http.Request) {
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
		`SELECT TOP (@p2) id, agent_id, rule_type, trace_ref, detail, params, at
		   FROM dbo.eval_violation WHERE agent_id = @p1 ORDER BY id DESC`, id, limit)
	if err != nil {
		serverError(w, "listViolations", err)
		return
	}
	defer rows.Close()
	out := []EvalViolation{}
	for rows.Next() {
		var v EvalViolation
		var tref sql.NullInt64
		var params string
		if err := rows.Scan(&v.ID, &v.AgentID, &v.RuleType, &tref, &v.Detail, &params, &v.At); err != nil {
			serverError(w, "listViolations scan", err)
			return
		}
		if tref.Valid {
			x := tref.Int64
			v.TraceRef = &x
		}
		v.Params = json.RawMessage(params)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listViolations rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
