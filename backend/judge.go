// Open-GenCut - Giudizio automatico con un modello (POST /api/evaluation/agents/{id}/judge).
//
// Una traccia viene valutata da un modello attraverso il proxy della chat (stessi controlli:
// accesso dell'agente alla risorsa, guardrail sull'ingresso e sull'uscita, monitoraggio).
// Il modello riceve solo i dati strutturali della traccia (esito, obiettivo, passi con tipo,
// nome, esito, latenza e token) e la rubrica. Risponde con un punteggio intero da 1 a 5.
// Il punteggio viene registrato con giudice "llm:<modello>". La chiave API serve solo alla
// chiamata e non viene salvata, né registrata, né restituita.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const (
	judgeMaxSteps    = 200 // passi descritti nel messaggio al giudice
	judgeMaxRubric   = 2000
	judgeScorePrefix = "llm:"
)

// JudgeInput è il corpo di POST /api/evaluation/agents/{id}/judge.
type JudgeInput struct {
	TraceRef    int64  `json:"trace_ref"`
	Provider    string `json:"provider"`
	APIKey      string `json:"api_key"`
	Model       string `json:"model"`
	Rubric      string `json:"rubric"`
	ResourceID  int    `json:"resource_id"`
	Environment string `json:"environment"`
}

// scoreReply accetta solo un numero da 1 a 5, da solo o come {"score": n}.
var scoreReply = regexp.MustCompile(`^\s*(?:\{\s*"score"\s*:\s*([1-5])\s*\}|([1-5]))\s*$`)

func (s *Server) registerJudge(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/evaluation/agents/{id}/judge", s.judgeTrace)
}

// traceSummary descrive la traccia al giudice con i soli dati strutturali (nessun testo).
func traceSummary(outcome string, goalMet bool, cost float64, steps []traceStepRow, stepDetail []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Traccia da valutare.\nEsito: %s\nObiettivo raggiunto: %s\nCosto: %.4f\nPassi (%d):\n",
		outcome, map[bool]string{true: "sì", false: "no"}[goalMet], cost, len(steps))
	for i, d := range stepDetail {
		fmt.Fprintf(&b, "%d. %s\n", i+1, d)
	}
	if len(steps) > len(stepDetail) {
		fmt.Fprintf(&b, "... altri %d passi non descritti\n", len(steps)-len(stepDetail))
	}
	return b.String()
}

// judgeTrace: POST /api/evaluation/agents/{id}/judge. Risponde 201 con il punteggio registrato,
// oppure 200 con scored=false se la chiamata è negata o bloccata dai guardrail.
func (s *Server) judgeTrace(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in JudgeInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Rubric = strings.TrimSpace(in.Rubric)
	if in.TraceRef <= 0 {
		writeValidation(w, "trace_ref è obbligatorio")
		return
	}
	if in.Rubric == "" || runeLen(in.Rubric) > judgeMaxRubric {
		writeValidation(w, "rubric è obbligatoria (massimo "+strconv.Itoa(judgeMaxRubric)+" caratteri)")
		return
	}
	if len(in.Model) > 96 {
		writeValidation(w, "model supera 96 caratteri")
		return
	}

	ctx := r.Context()
	agentID, steps, err := s.loadTraceSteps(ctx, in.TraceRef)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && agentID != id) {
		writeNotFound(w, "traccia di questo agente")
		return
	}
	if err != nil {
		serverError(w, "judge trace", err)
		return
	}
	var outcome string
	var goalMet bool
	var cost float64
	if err := s.db.QueryRowContext(ctx,
		`SELECT outcome, goal_met, cost FROM dbo.eval_trace WHERE id = @p1`, in.TraceRef).
		Scan(&outcome, &goalMet, &cost); err != nil {
		serverError(w, "judge trace fields", err)
		return
	}
	details := []string{}
	for i, st := range steps {
		if i >= judgeMaxSteps {
			break
		}
		details = append(details, fmt.Sprintf("tipo=%s nome=%s", st.Type, st.Name))
	}
	summary := traceSummary(outcome, goalMet, cost, steps, details)

	chat := ChatInput{
		Provider:    in.Provider,
		APIKey:      in.APIKey,
		Model:       in.Model,
		System:      "Sei un giudice che valuta la qualità di un agente. Assegna un punteggio intero da 1 a 5 secondo la rubrica. Rispondi solo con il numero, oppure con {\"score\": n}.\nRubrica:\n" + in.Rubric,
		Messages:    []ChatMessage{{Role: "user", Content: summary}},
		AgentID:     id,
		ResourceID:  in.ResourceID,
		Environment: in.Environment,
	}
	if msg := validateChat(&chat); msg != "" {
		writeValidation(w, msg)
		return
	}

	res, serr := s.runChat(ctx, chat)
	if serr != nil {
		writeSvcErr(w, serr)
		return
	}
	if res.Decision != "allowed" {
		writeJSON(w, http.StatusOK, map[string]any{"scored": false, "decision": res.Decision, "reason": res.Reason})
		return
	}

	m := scoreReply.FindStringSubmatch(res.Text)
	if m == nil {
		writeError(w, http.StatusBadGateway, "judge_invalid_reply", "il giudice non ha restituito un punteggio da 1 a 5")
		return
	}
	score, _ := strconv.Atoi(m[1] + m[2])
	judge := judgeScorePrefix + chat.Model

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "judge begin", err)
		return
	}
	defer tx.Rollback()
	if _, err := lockAgent(ctx, tx, id); err != nil {
		agentLockError(w, "judge agent", err)
		return
	}
	var sid int
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO dbo.eval_score (agent_id, score, judge, note) OUTPUT inserted.id VALUES (@p1, @p2, @p3, NULL)`,
		id, score, judge).Scan(&sid); err != nil {
		serverError(w, "judge insert", err)
		return
	}
	if err := writeAudit(ctx, tx, "create", "eval_score", id,
		auditDetail("score_recorded", map[string]any{"score": score, "judge": judge, "trace_ref": in.TraceRef})); err != nil {
		serverError(w, "judge audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "judge commit", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"scored": true, "id": sid, "agent_id": id, "trace_ref": in.TraceRef,
		"score": score, "judge": judge, "decision": res.Decision,
	})
}
