// Open-GenCut - Punto 8 della specifica: FinOps e tracciamento degli agenti.
//
// Costi cross-platform (costo d'uso dal monitoraggio + costo fisso mensile), esiti di successo,
// fallimento o parziale con il valore prodotto, e ROI per agente.
//
// Convenzioni:
//   - i costi d'uso (usage_log.cost) e i valori degli esiti sono in EUR;
//   - il costo fisso è cost_monthly per ciascun mese toccato dal periodo; conta solo se la valuta è EUR;
//     gli agenti in USD sono riportati a parte in fixed_cost_other_currency (nessun tasso di cambio);
//   - roi = (valore - costo) / costo, null se il costo è zero;
//   - cost_per_success = costo totale / successi, null se non ci sono successi.
package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

var outcomeValues = map[string]bool{"success": true, "failure": true, "partial": true}

// OutcomeInput è il corpo di POST /api/finops/outcomes.
type OutcomeInput struct {
	AgentID  int     `json:"agent_id"`
	Outcome  string  `json:"outcome"`
	ValueEUR float64 `json:"value_eur"`
	Period   string  `json:"period"`
	Note     string  `json:"note"`
}

// FinOpsView è la vista finanziaria di un agente nel periodo.
type FinOpsView struct {
	AgentID                int                `json:"agent_id"`
	Name                   string             `json:"name"`
	Origin                 string             `json:"origin"`
	Framework              *string            `json:"framework"`
	From                   string             `json:"from"`
	To                     string             `json:"to"`
	Currency               string             `json:"currency"`
	UsageCost              float64            `json:"usage_cost"`
	FixedCost              float64            `json:"fixed_cost"`
	TotalCost              float64            `json:"total_cost"`
	FixedCostOtherCurrency map[string]float64 `json:"fixed_cost_other_currency"`
	Outcomes               map[string]int     `json:"outcomes"`
	ValueEUR               float64            `json:"value_eur"`
	ROI                    *float64           `json:"roi"`
	CostPerSuccess         *float64           `json:"cost_per_success"`
}

// parseDate accetta solo YYYY-MM-DD.
func parseDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// periodRange legge from e to (obbligatori, YYYY-MM-DD, from <= to).
func periodRange(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, bool) {
	qs := r.URL.Query()
	from, err := parseDate(qs.Get("from"))
	if err != nil {
		writeValidation(w, "from è obbligatorio, formato YYYY-MM-DD")
		return from, from, false
	}
	to, err := parseDate(qs.Get("to"))
	if err != nil {
		writeValidation(w, "to è obbligatorio, formato YYYY-MM-DD")
		return from, to, false
	}
	if to.Before(from) {
		writeValidation(w, "to non può precedere from")
		return from, to, false
	}
	return from, to, true
}

// registerFinOps aggiunge le rotte FinOps al mux principale.
func (s *Server) registerFinOps(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/finops/outcomes", s.createOutcome)
	mux.HandleFunc("GET /api/finops/agents/{id}", s.finopsAgent)
	mux.HandleFunc("GET /api/finops/summary", s.finopsSummary)
}

// createOutcome: POST /api/finops/outcomes. Risponde 201.
func (s *Server) createOutcome(w http.ResponseWriter, r *http.Request) {
	var in OutcomeInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.AgentID <= 0 {
		writeValidation(w, "agent_id è obbligatorio")
		return
	}
	if !outcomeValues[in.Outcome] {
		writeValidation(w, "outcome deve essere success, failure o partial")
		return
	}
	if !validMoney(in.ValueEUR) {
		writeValidation(w, "value_eur deve essere maggiore o uguale a 0 nei limiti del database")
		return
	}
	period, err := parseDate(in.Period)
	if err != nil {
		writeValidation(w, "period deve essere YYYY-MM-DD")
		return
	}
	if runeLen(in.Note) > 500 {
		writeValidation(w, "note supera 500 caratteri")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createOutcome begin", err)
		return
	}
	defer tx.Rollback()
	if _, err := lockAgent(r.Context(), tx, in.AgentID); err != nil {
		agentLockError(w, "createOutcome agent", err)
		return
	}
	var oid int64
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.finops_outcome (agent_id, outcome, value_eur, period, note)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4, @p5)`,
		in.AgentID, in.Outcome, in.ValueEUR, period, nullable(in.Note)).Scan(&oid)
	if err != nil {
		writeDBError(w, "createOutcome insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "finops_outcome", in.AgentID,
		auditDetail("outcome_recorded", map[string]any{"outcome": in.Outcome, "period": in.Period})); err != nil {
		serverError(w, "createOutcome audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createOutcome commit", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": oid, "agent_id": in.AgentID, "outcome": in.Outcome,
		"value_eur": in.ValueEUR, "period": in.Period})
}

// finopsFor calcola la vista finanziaria di un agente nel periodo.
func (s *Server) finopsFor(ctx context.Context, agentID int, from, to time.Time) (FinOpsView, error) {
	v := FinOpsView{AgentID: agentID, From: from.Format("2006-01-02"), To: to.Format("2006-01-02"),
		Currency: "EUR", FixedCostOtherCurrency: map[string]float64{},
		Outcomes: map[string]int{"success": 0, "failure": 0, "partial": 0}}

	var currency string
	var monthly float64
	var fw sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT name, origin, framework, currency, cost_monthly FROM dbo.agent WHERE id = @p1`, agentID).
		Scan(&v.Name, &v.Origin, &fw, &currency, &monthly); err != nil {
		return v, err
	}
	v.Framework = ptrOf(fw)

	// Costo d'uso dal monitoraggio nel periodo (estremi inclusi).
	var usage sql.NullFloat64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(cost) FROM dbo.usage_log
		  WHERE agent_id = @p1 AND called_at >= @p2 AND called_at < DATEADD(DAY, 1, @p3)`,
		agentID, from, to).Scan(&usage); err != nil {
		return v, err
	}
	v.UsageCost = usage.Float64

	// Costo fisso: un mese per ciascun mese toccato dal periodo.
	months := (to.Year()*12 + int(to.Month())) - (from.Year()*12 + int(from.Month())) + 1
	if currency == "EUR" {
		v.FixedCost = monthly * float64(months)
	} else {
		v.FixedCostOtherCurrency[currency] = monthly * float64(months)
	}
	v.TotalCost = v.UsageCost + v.FixedCost

	// Esiti nel periodo.
	rows, err := s.db.QueryContext(ctx,
		`SELECT outcome, COUNT(*), SUM(value_eur) FROM dbo.finops_outcome
		  WHERE agent_id = @p1 AND period >= @p2 AND period <= @p3 GROUP BY outcome`,
		agentID, from, to)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var o string
		var n int
		var val sql.NullFloat64
		if err := rows.Scan(&o, &n, &val); err != nil {
			return v, err
		}
		v.Outcomes[o] = n
		v.ValueEUR += val.Float64
	}
	if err := rows.Err(); err != nil {
		return v, err
	}

	if v.TotalCost > 0 {
		roi := (v.ValueEUR - v.TotalCost) / v.TotalCost
		v.ROI = &roi
	}
	if successes := v.Outcomes["success"]; successes > 0 {
		cps := v.TotalCost / float64(successes)
		v.CostPerSuccess = &cps
	}
	return v, nil
}

// finopsAgent: GET /api/finops/agents/{id}?from=&to=
func (s *Server) finopsAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	from, to, ok := periodRange(w, r)
	if !ok {
		return
	}
	v, err := s.finopsFor(r.Context(), id, from, to)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "agente")
		return
	}
	if err != nil {
		serverError(w, "finopsAgent", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// FinOpsGroup è il totale di un gruppo (framework o origine).
type FinOpsGroup struct {
	Key            string         `json:"key"`
	Agents         int            `json:"agents"`
	UsageCost      float64        `json:"usage_cost"`
	FixedCost      float64        `json:"fixed_cost"`
	TotalCost      float64        `json:"total_cost"`
	Outcomes       map[string]int `json:"outcomes"`
	ValueEUR       float64        `json:"value_eur"`
	ROI            *float64       `json:"roi"`
	CostPerSuccess *float64       `json:"cost_per_success"`
}

// finopsSummary: GET /api/finops/summary?from=&to=&group_by=framework|origin
func (s *Server) finopsSummary(w http.ResponseWriter, r *http.Request) {
	from, to, ok := periodRange(w, r)
	if !ok {
		return
	}
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "origin"
	}
	if groupBy != "framework" && groupBy != "origin" {
		writeValidation(w, "group_by deve essere framework o origin")
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT id FROM dbo.agent ORDER BY id`)
	if err != nil {
		serverError(w, "finopsSummary agents", err)
		return
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			serverError(w, "finopsSummary scan", err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()

	groups := map[string]*FinOpsGroup{}
	var order []string
	for _, id := range ids {
		v, err := s.finopsFor(r.Context(), id, from, to)
		if err != nil {
			serverError(w, "finopsSummary view", err)
			return
		}
		key := v.Origin
		if groupBy == "framework" {
			key = "(nessuno)"
			if v.Framework != nil && *v.Framework != "" {
				key = *v.Framework
			}
		}
		g, ok := groups[key]
		if !ok {
			g = &FinOpsGroup{Key: key, Outcomes: map[string]int{"success": 0, "failure": 0, "partial": 0}}
			groups[key] = g
			order = append(order, key)
		}
		g.Agents++
		g.UsageCost += v.UsageCost
		g.FixedCost += v.FixedCost
		g.TotalCost += v.TotalCost
		g.ValueEUR += v.ValueEUR
		for k, n := range v.Outcomes {
			g.Outcomes[k] += n
		}
	}
	out := make([]FinOpsGroup, 0, len(order))
	for _, key := range order {
		g := groups[key]
		if g.TotalCost > 0 {
			roi := (g.ValueEUR - g.TotalCost) / g.TotalCost
			g.ROI = &roi
		}
		if n := g.Outcomes["success"]; n > 0 {
			cps := g.TotalCost / float64(n)
			g.CostPerSuccess = &cps
		}
		out = append(out, *g)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group_by": groupBy,
		"from":     from.Format("2006-01-02"),
		"to":       to.Format("2006-01-02"),
		"groups":   out,
	})
}
