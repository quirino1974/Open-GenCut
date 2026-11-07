// Open-GenCut - Scoperta: import di manifesti agente (POST /api/agents/import).
//
// Il corpo è un elenco JSON di manifesti (stessi campi di POST /api/agents: name, origin, owner,
// framework, version, endpoint, ...). Gli agenti mancanti vengono creati; i duplicati per nome
// (già presenti o ripetuti nello stesso elenco) vengono saltati; gli elementi non validi sono
// segnalati senza bloccare gli altri. Ogni agente creato ha una voce di audit.
package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	importMaxManifests = 500
	importMaxBody      = 1 << 20 // 1 MB
)

// ImportItem è l'esito di un singolo manifesto nell'import.
type ImportItem struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Result  string `json:"result"` // created | skipped | error
	ID      *int   `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
}

// ImportResponse è la risposta di POST /api/agents/import.
type ImportResponse struct {
	Created int          `json:"created"`
	Skipped int          `json:"skipped"`
	Errors  int          `json:"errors"`
	Items   []ImportItem `json:"items"`
}

func (s *Server) registerDiscovery(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/agents/import", s.importAgents)
}

// decodeManifests legge l'elenco dei manifesti. Campi sconosciuti e dati dopo il JSON sono rifiutati.
func decodeManifests(w http.ResponseWriter, r *http.Request) ([]AgentInput, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, importMaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var list []AgentInput
	if err := dec.Decode(&list); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "elenco di manifesti non valido")
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "dati dopo l'elenco di manifesti")
		return nil, false
	}
	if len(list) == 0 {
		writeValidation(w, "l'elenco di manifesti è vuoto")
		return nil, false
	}
	if len(list) > importMaxManifests {
		writeValidation(w, "l'elenco supera "+strconv.Itoa(importMaxManifests)+" manifesti")
		return nil, false
	}
	return list, true
}

// importAgents: POST /api/agents/import. Risponde 200 con creati, saltati, errori e dettaglio per elemento.
func (s *Server) importAgents(w http.ResponseWriter, r *http.Request) {
	list, ok := decodeManifests(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// Nomi già presenti: confronto senza distinzione di maiuscole/minuscole.
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM dbo.agent`)
	if err != nil {
		serverError(w, "importAgents names", err)
		return
	}
	seen := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			serverError(w, "importAgents scan", err)
			return
		}
		seen[strings.ToLower(strings.TrimSpace(n))] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		serverError(w, "importAgents names rows", err)
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "importAgents begin", err)
		return
	}
	defer tx.Rollback()

	resp := ImportResponse{Items: []ImportItem{}}
	for i, m := range list {
		item := ImportItem{Index: i, Name: strings.TrimSpace(m.Name)}
		key := strings.ToLower(item.Name)
		if item.Name != "" && seen[key] {
			item.Result = "skipped"
			item.Message = "agente già presente con lo stesso nome"
			resp.Skipped++
			resp.Items = append(resp.Items, item)
			continue
		}
		if msg := validateAgentInput(&m); msg != "" {
			item.Result = "error"
			item.Message = msg
			resp.Errors++
			resp.Items = append(resp.Items, item)
			continue
		}
		var id int
		err := tx.QueryRowContext(ctx,
			`INSERT INTO dbo.agent (name, description, origin, vendor, version, owner, status,
			                        model, endpoint, cost_monthly, currency,
			                        interacts_with_people, interaction_language, framework)
			 OUTPUT inserted.id
			 VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10, @p11, @p12, @p13, @p14)`,
			m.Name, nullable(m.Description), m.Origin, nullable(m.Vendor), nullable(m.Version),
			nullable(m.Owner), m.Status, nullable(m.Model), nullable(m.Endpoint),
			m.costOrZero(), m.Currency, m.InteractsWithPeople, nullable(m.InteractionLanguage),
			nullable(m.Framework),
		).Scan(&id)
		if err != nil {
			// Un errore del database sul singolo elemento non interrompe il lotto.
			item.Result = "error"
			item.Message = "agente non creato: dati non accettati dal database"
			resp.Errors++
			resp.Items = append(resp.Items, item)
			continue
		}
		if err := writeAudit(ctx, tx, "create", "agent", id,
			auditDetail("agent_imported", map[string]any{"name": m.Name})); err != nil {
			serverError(w, "importAgents audit", err)
			return
		}
		seen[key] = true
		item.Result = "created"
		item.ID = &id
		resp.Created++
		resp.Items = append(resp.Items, item)
	}

	if err := tx.Commit(); err != nil {
		serverError(w, "importAgents commit", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
