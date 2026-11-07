// Open-GenCut v0.1 - Punto 4, R3: gate di valutazione prima di staging e produzione.
//
// Un agente può entrare in staging o production (deploy o promozione) solo se
// l'ULTIMA valutazione registrata per l'agente è "passed" e riguarda la stessa versione.
// Una valutazione "failed" blocca il passaggio finché non ne arriva una superata.
// Lo sviluppo (development) non è soggetto al gate.
//
// Rotte: POST e GET /api/lifecycle/agents/{id}/evaluations.
package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// gatedEnvironments sono gli ambienti che richiedono una valutazione superata.
var gatedEnvironments = map[string]bool{"staging": true, "production": true}

// Evaluation è una valutazione registrata.
type Evaluation struct {
	ID           int       `json:"id"`
	AgentID      int       `json:"agent_id"`
	VersionLabel string    `json:"version_label"`
	Result       string    `json:"result"` // passed | failed
	Evaluator    string    `json:"evaluator"`
	Note         *string   `json:"note"`
	At           time.Time `json:"at"`
}

// EvaluationInput è il corpo di POST /evaluations.
type EvaluationInput struct {
	VersionLabel string `json:"version_label"`
	Result       string `json:"result"`
	Note         string `json:"note"`
}

// registerEvaluations aggiunge le rotte di valutazione al mux principale.
func (s *Server) registerEvaluations(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/lifecycle/agents/{id}/evaluations", s.listEvaluations)
	mux.HandleFunc("POST /api/lifecycle/agents/{id}/evaluations", s.createEvaluation)
}

// createEvaluation: POST /api/lifecycle/agents/{id}/evaluations. Risponde 201.
func (s *Server) createEvaluation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in EvaluationInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.VersionLabel = strings.TrimSpace(in.VersionLabel)
	if in.VersionLabel == "" {
		writeValidation(w, "version_label è obbligatoria")
		return
	}
	if in.Result != "passed" && in.Result != "failed" {
		writeValidation(w, "result deve essere passed o failed")
		return
	}
	if runeLen(in.Note) > 500 {
		writeValidation(w, "note supera 500 caratteri")
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createEvaluation begin", err)
		return
	}
	defer tx.Rollback()

	status, err := lockAgent(r.Context(), tx, id)
	if err != nil {
		agentLockError(w, "createEvaluation", err)
		return
	}
	if status == "retired" {
		writeInvalidState(w, "agente dismesso: nessuna valutazione possibile")
		return
	}

	var exists int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM dbo.agent_version WHERE agent_id = @p1 AND version_label = @p2`,
		id, in.VersionLabel).Scan(&exists); err != nil {
		serverError(w, "createEvaluation version", err)
		return
	}
	if exists == 0 {
		writeValidation(w, "versione non registrata per questo agente")
		return
	}

	var e Evaluation
	var note sql.NullString
	if in.Note != "" {
		note = sql.NullString{String: in.Note, Valid: true}
	}
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.agent_evaluation (agent_id, version_label, result, note, evaluator)
		 OUTPUT inserted.id, inserted.agent_id, inserted.version_label, inserted.result,
		        inserted.evaluator, inserted.note, inserted.at
		 VALUES (@p1, @p2, @p3, @p4, @p5)`,
		id, in.VersionLabel, in.Result, note, actorFrom(r.Context())).
		Scan(&e.ID, &e.AgentID, &e.VersionLabel, &e.Result, &e.Evaluator, scanNullStr(&e.Note), &e.At)
	if err != nil {
		serverError(w, "createEvaluation insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "agent_evaluation", id,
		auditDetail("evaluation_recorded", map[string]any{"version": in.VersionLabel, "result": in.Result})); err != nil {
		serverError(w, "createEvaluation audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createEvaluation commit", err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

// listEvaluations: GET /api/lifecycle/agents/{id}/evaluations?limit=50 (massimo 500), dal più recente.
func (s *Server) listEvaluations(w http.ResponseWriter, r *http.Request) {
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
		`SELECT TOP (@p2) id, agent_id, version_label, result, evaluator, note, at
		   FROM dbo.agent_evaluation WHERE agent_id = @p1 ORDER BY id DESC`, id, limit)
	if err != nil {
		serverError(w, "listEvaluations", err)
		return
	}
	defer rows.Close()
	out := []Evaluation{}
	for rows.Next() {
		var e Evaluation
		if err := rows.Scan(&e.ID, &e.AgentID, &e.VersionLabel, &e.Result, &e.Evaluator,
			scanNullStr(&e.Note), &e.At); err != nil {
			serverError(w, "listEvaluations scan", err)
			return
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listEvaluations rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// requireEvaluation applica il gate. Restituisce "" se il passaggio è consentito,
// altrimenti il motivo del blocco. Va chiamata dentro la transazione con l'agente bloccato.
func requireEvaluation(ctx context.Context, tx *sql.Tx, agentID int, env, version string) (string, error) {
	if !gatedEnvironments[env] {
		return "", nil
	}
	var result, evalVersion string
	err := tx.QueryRowContext(ctx,
		`SELECT TOP (1) result, version_label FROM dbo.agent_evaluation
		  WHERE agent_id = @p1 ORDER BY id DESC`, agentID).Scan(&result, &evalVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return "nessuna valutazione registrata: impossibile andare in " + env, nil
	}
	if err != nil {
		return "", err
	}
	if result != "passed" {
		return "ultima valutazione non superata: impossibile andare in " + env, nil
	}
	if evalVersion != version {
		return "ultima valutazione su una versione diversa (" + evalVersion + "): impossibile andare in " + env, nil
	}
	return "", nil
}
