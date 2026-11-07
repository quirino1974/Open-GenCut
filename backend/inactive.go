// Open-GenCut - Agenti senza attività (punto 2 della specifica, docs/API.txt).
//
// Un agente è attivo se ha almeno un evento negli ultimi N giorni: chiamata di monitoraggio
// (usage_log), decisione di accesso (access_event, che include il gateway), traccia di valutazione.
package main

import (
	"net/http"
	"strconv"
	"time"
)

// InactiveAgent è un agente attivo senza attività recente.
type InactiveAgent struct {
	ID             int        `json:"id"`
	Name           string     `json:"name"`
	Origin         string     `json:"origin"`
	Status         string     `json:"status"`
	LastActivityAt *time.Time `json:"last_activity_at"` // null se non ha mai avuto attività
}

// registerInactive aggiunge la rotta di rilevamento al mux principale.
func (s *Server) registerInactive(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents/inactive", s.listInactiveAgents)
}

// listInactiveAgents: GET /api/agents/inactive?days=N (predefinito 30, da 1 a 3650).
func (s *Server) listInactiveAgents(w http.ResponseWriter, r *http.Request) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3650 {
			writeValidation(w, "days deve essere un intero tra 1 e 3650")
			return
		}
		days = n
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT a.id, a.name, a.origin, a.status, last.last_at
		   FROM dbo.agent a
		   OUTER APPLY (
		       SELECT MAX(x.t) AS last_at FROM (
		           SELECT MAX(called_at) AS t FROM dbo.usage_log WHERE agent_id = a.id
		           UNION ALL
		           SELECT MAX(at) FROM dbo.access_event WHERE agent_id = a.id
		           UNION ALL
		           SELECT MAX(created_at) FROM dbo.eval_trace WHERE agent_id = a.id
		       ) x
		   ) last
		  WHERE a.status = 'active'
		    AND (last.last_at IS NULL OR last.last_at < DATEADD(DAY, -@p1, SYSUTCDATETIME()))
		  ORDER BY a.id`, days)
	if err != nil {
		serverError(w, "listInactiveAgents", err)
		return
	}
	defer rows.Close()
	out := []InactiveAgent{}
	for rows.Next() {
		var a InactiveAgent
		var last *time.Time
		if err := rows.Scan(&a.ID, &a.Name, &a.Origin, &a.Status, &last); err != nil {
			serverError(w, "listInactiveAgents scan", err)
			return
		}
		a.LastActivityAt = last
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listInactiveAgents rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
