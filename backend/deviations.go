// Open-GenCut - Deviazioni dei ragionamenti dalla sequenza di riferimento.
//
// Per ogni agente si definisce una sequenza di riferimento (passi attesi, per tipo e nome):
//
//	GET /api/evaluation/agents/{id}/reference   leggi la sequenza
//	PUT /api/evaluation/agents/{id}/reference   sostituisci la sequenza: {"steps":[{"type","name"}]}
//
// POST /api/evaluation/deviations confronta i passi di una traccia (eval_trace.id, campo trace_ref)
// con la sequenza di riferimento del suo agente. Usa la sottosequenza comune più lunga (LCS):
// i passi della traccia fuori da essa sono "unexpected_step", quelli di riferimento mancanti
// sono "missing_step" (uno spostamento di ordine compare come coppia di questi due).
// I passi di tipo tool il cui nome non è tra gli strumenti in allowlist dell'agente sono
// "tool_not_allowed". Il risultato non viene salvato: è calcolato al momento.
package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

const (
	refMaxSteps   = 200
	traceMaxSteps = 5000
)

// ReferenceStep è un passo della sequenza di riferimento.
type ReferenceStep struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// ReferenceInput è il corpo di PUT /api/evaluation/agents/{id}/reference.
type ReferenceInput struct {
	Steps []ReferenceStep `json:"steps"`
}

// Deviation è una differenza tra la traccia e la sequenza di riferimento.
type Deviation struct {
	Kind          string `json:"kind"` // unexpected_step | missing_step | tool_not_allowed
	TracePosition int    `json:"trace_position,omitempty"`
	RefPosition   int    `json:"reference_position,omitempty"`
	Type          string `json:"type"`
	Name          string `json:"name"`
}

// DeviationReport è la risposta di POST /api/evaluation/deviations.
type DeviationReport struct {
	AgentID        int         `json:"agent_id"`
	TraceRef       int64       `json:"trace_ref"`
	Compliant      bool        `json:"compliant"`
	TraceSteps     int         `json:"trace_steps"`
	ReferenceSteps int         `json:"reference_steps"`
	Deviations     []Deviation `json:"deviations"`
}

func (s *Server) registerDeviations(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/evaluation/agents/{id}/reference", s.getReference)
	mux.HandleFunc("PUT /api/evaluation/agents/{id}/reference", s.putReference)
	mux.HandleFunc("POST /api/evaluation/deviations", s.postDeviations)
}

// loadReference legge la sequenza di riferimento di un agente, in ordine.
func (s *Server) loadReference(ctx context.Context, agentID int) ([]ReferenceStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT step_type, name FROM dbo.agent_reference_step WHERE agent_id = @p1 ORDER BY position`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReferenceStep{}
	for rows.Next() {
		var st ReferenceStep
		if err := rows.Scan(&st.Type, &st.Name); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// getReference: GET /api/evaluation/agents/{id}/reference
func (s *Server) getReference(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	steps, err := s.loadReference(r.Context(), id)
	if err != nil {
		serverError(w, "getReference", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": id, "steps": steps})
}

// putReference: PUT /api/evaluation/agents/{id}/reference. Sostituisce l'intera sequenza.
func (s *Server) putReference(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in ReferenceInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.Steps) == 0 || len(in.Steps) > refMaxSteps {
		writeValidation(w, "steps deve contenere da 1 a "+strconv.Itoa(refMaxSteps)+" passi")
		return
	}
	for i, st := range in.Steps {
		in.Steps[i].Name = strings.TrimSpace(st.Name)
		if !stepTypes[st.Type] {
			writeValidation(w, "tipo di passo non valido: deve essere llm, tool, reasoning o memory")
			return
		}
		if in.Steps[i].Name == "" || runeLen(in.Steps[i].Name) > 150 {
			writeValidation(w, "ogni passo richiede name (massimo 150 caratteri)")
			return
		}
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "putReference begin", err)
		return
	}
	defer tx.Rollback()
	if _, err := lockAgent(ctx, tx, id); err != nil {
		agentLockError(w, "putReference agent", err)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM dbo.agent_reference_step WHERE agent_id = @p1`, id); err != nil {
		serverError(w, "putReference delete", err)
		return
	}
	for i, st := range in.Steps {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO dbo.agent_reference_step (agent_id, position, step_type, name) VALUES (@p1, @p2, @p3, @p4)`,
			id, i+1, st.Type, st.Name); err != nil {
			serverError(w, "putReference insert", err)
			return
		}
	}
	if err := writeAudit(ctx, tx, "update", "agent", id,
		auditDetail("reference_saved", map[string]any{"steps": len(in.Steps)})); err != nil {
		serverError(w, "putReference audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "putReference commit", err)
		return
	}
	steps, err := s.loadReference(ctx, id)
	if err != nil {
		serverError(w, "putReference load", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": id, "steps": steps})
}

// traceStepRow è un passo salvato di una traccia (nessun testo: solo tipo e nome).
type traceStepRow struct {
	Type string
	Name string
}

// loadTraceSteps restituisce agente e passi di una traccia di valutazione, in ordine.
func (s *Server) loadTraceSteps(ctx context.Context, traceRef int64) (int, []traceStepRow, error) {
	var agentID int
	err := s.db.QueryRowContext(ctx, `SELECT agent_id FROM dbo.eval_trace WHERE id = @p1`, traceRef).Scan(&agentID)
	if err != nil {
		return 0, nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT step_type, name FROM dbo.eval_trace_step WHERE trace_ref = @p1 ORDER BY id`, traceRef)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	steps := []traceStepRow{}
	for rows.Next() {
		var st traceStepRow
		if err := rows.Scan(&st.Type, &st.Name); err != nil {
			return 0, nil, err
		}
		steps = append(steps, st)
	}
	return agentID, steps, rows.Err()
}

// lcsMatch restituisce, per i passi della traccia e del riferimento, se sono nella sottosequenza comune più lunga.
func lcsMatch(trace []traceStepRow, ref []ReferenceStep) ([]bool, []bool) {
	n, m := len(trace), len(ref)
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if trace[i].Type == ref[j].Type && trace[i].Name == ref[j].Name {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	matchT := make([]bool, n)
	matchR := make([]bool, m)
	i, j := 0, 0
	for i < n && j < m {
		if trace[i].Type == ref[j].Type && trace[i].Name == ref[j].Name && dp[i][j] == dp[i+1][j+1]+1 {
			matchT[i], matchR[j] = true, true
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			i++
		} else {
			j++
		}
	}
	return matchT, matchR
}

// postDeviations: POST /api/evaluation/deviations  {"trace_ref": N}
func (s *Server) postDeviations(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TraceRef int64 `json:"trace_ref"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.TraceRef <= 0 {
		writeValidation(w, "trace_ref è obbligatorio")
		return
	}
	ctx := r.Context()
	agentID, trace, err := s.loadTraceSteps(ctx, in.TraceRef)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "traccia")
		return
	}
	if err != nil {
		serverError(w, "postDeviations trace", err)
		return
	}
	if len(trace) > traceMaxSteps {
		writeValidation(w, "la traccia supera "+strconv.Itoa(traceMaxSteps)+" passi")
		return
	}
	ref, err := s.loadReference(ctx, agentID)
	if err != nil {
		serverError(w, "postDeviations reference", err)
		return
	}
	if len(ref) == 0 {
		writeError(w, http.StatusConflict, "no_reference_sequence", "l'agente non ha una sequenza di riferimento")
		return
	}

	matchT, matchR := lcsMatch(trace, ref)
	report := DeviationReport{AgentID: agentID, TraceRef: in.TraceRef, TraceSteps: len(trace),
		ReferenceSteps: len(ref), Deviations: []Deviation{}}
	for i, st := range trace {
		if !matchT[i] {
			report.Deviations = append(report.Deviations,
				Deviation{Kind: "unexpected_step", TracePosition: i + 1, Type: st.Type, Name: st.Name})
		}
	}
	for j, st := range ref {
		if !matchR[j] {
			report.Deviations = append(report.Deviations,
				Deviation{Kind: "missing_step", RefPosition: j + 1, Type: st.Type, Name: st.Name})
		}
	}

	// Strumenti consentiti: nomi degli strumenti in allowlist per l'agente.
	allowed, err := s.allowedToolNames(ctx, agentID)
	if err != nil {
		serverError(w, "postDeviations allowlist", err)
		return
	}
	for i, st := range trace {
		if st.Type == "tool" && !allowed[st.Name] {
			report.Deviations = append(report.Deviations,
				Deviation{Kind: "tool_not_allowed", TracePosition: i + 1, Type: st.Type, Name: st.Name})
		}
	}
	report.Compliant = len(report.Deviations) == 0
	writeJSON(w, http.StatusOK, report)
}

// allowedToolNames restituisce i nomi degli strumenti che l'agente può usare.
func (s *Server) allowedToolNames(ctx context.Context, agentID int) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT t.name FROM dbo.tool_access ta JOIN dbo.mcp_tool t ON t.id = ta.tool_id WHERE ta.agent_id = @p1`,
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}
