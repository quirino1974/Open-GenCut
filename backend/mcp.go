// Open-GenCut - Server MCP: strumenti per server, sincronizzazione e permessi per strumento (docs/MCP.txt).
//
// Uno strumento appartiene a un server MCP (risorsa di tipo mcp_server). Per usarlo un agente deve avere
// la coppia agente-server in allowlist e una voce per lo strumento (tool_access). Senza voce: tool_not_allowed.
// La sincronizzazione legge l'elenco con tools/list via JSON-RPC su HTTP.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var toolRisks = map[string]bool{"low": true, "medium": true, "high": true}

// Tool è uno strumento registrato per un server MCP.
type Tool struct {
	ID               int       `json:"id"`
	ResourceID       int       `json:"resource_id"`
	Name             string    `json:"name"`
	Description      *string   `json:"description"`
	Risk             string    `json:"risk"`
	RequiresApproval bool      `json:"requires_approval"`
	CreatedAt        time.Time `json:"created_at"`
}

// ToolInput è il corpo di POST e PUT degli strumenti.
type ToolInput struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	Risk             string `json:"risk"` // default "low"
	RequiresApproval bool   `json:"requires_approval"`
}

// ToolAccess è la voce di permesso di un agente per uno strumento.
type ToolAccess struct {
	ID               int       `json:"id"`
	AgentID          int       `json:"agent_id"`
	ToolID           int       `json:"tool_id"`
	ToolName         string    `json:"tool_name"`
	RequiresApproval bool      `json:"requires_approval"`
	CreatedAt        time.Time `json:"created_at"`
}

// ToolAccessInput è il corpo di POST /api/integrations/tool-allowlist.
type ToolAccessInput struct {
	AgentID          int  `json:"agent_id"`
	ToolID           int  `json:"tool_id"`
	RequiresApproval bool `json:"requires_approval"`
}

// registerMCP aggiunge le rotte dei server MCP al mux principale.
func (s *Server) registerMCP(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations/resources/{id}/tools", s.listTools)
	mux.HandleFunc("POST /api/integrations/resources/{id}/tools", s.createTool)
	mux.HandleFunc("POST /api/integrations/resources/{id}/tools/sync", s.syncTools)
	mux.HandleFunc("PUT /api/integrations/tools/{tool_id}", s.updateTool)
	mux.HandleFunc("DELETE /api/integrations/tools/{tool_id}", s.deleteTool)

	mux.HandleFunc("POST /api/integrations/tool-allowlist", s.createToolAccess)
	mux.HandleFunc("GET /api/integrations/tool-allowlist", s.listToolAccess)
	mux.HandleFunc("DELETE /api/integrations/tool-allowlist/{id}", s.deleteToolAccess)
}

// mcpResource legge tipo, stato e endpoint della risorsa; sql.ErrNoRows se manca.
func (s *Server) mcpResource(ctx context.Context, id int) (kind, status, endpoint string, err error) {
	var ep sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT kind, status, endpoint FROM dbo.integration_resource WHERE id = @p1`, id).Scan(&kind, &status, &ep)
	return kind, status, ep.String, err
}

func decodeToolInput(w http.ResponseWriter, r *http.Request) (ToolInput, bool) {
	var in ToolInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || runeLen(in.Name) > 150 {
		writeValidation(w, "name è obbligatorio (massimo 150 caratteri)")
		return in, false
	}
	if in.Risk == "" {
		in.Risk = "low"
	}
	if !toolRisks[in.Risk] {
		writeValidation(w, "risk deve essere low, medium o high")
		return in, false
	}
	if runeLen(in.Description) > 1000 {
		writeValidation(w, "description supera 1000 caratteri")
		return in, false
	}
	return in, true
}

const toolColumns = `t.id, t.resource_id, t.name, t.description, t.risk, t.requires_approval, t.created_at`

func scanTool(sc rowScanner) (Tool, error) {
	var t Tool
	var desc sql.NullString
	if err := sc.Scan(&t.ID, &t.ResourceID, &t.Name, &desc, &t.Risk, &t.RequiresApproval, &t.CreatedAt); err != nil {
		return Tool{}, err
	}
	t.Description = ptrOf(desc)
	return t, nil
}

func (s *Server) fetchTool(ctx context.Context, id int) (Tool, error) {
	return scanTool(s.db.QueryRowContext(ctx, `SELECT `+toolColumns+` FROM dbo.mcp_tool t WHERE t.id = @p1`, id))
}

// listTools: GET /api/integrations/resources/{id}/tools
func (s *Server) listTools(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, _, _, err := s.mcpResource(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "risorsa")
		return
	} else if err != nil {
		serverError(w, "listTools resource", err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+toolColumns+` FROM dbo.mcp_tool t WHERE t.resource_id = @p1 ORDER BY t.name`, id)
	if err != nil {
		serverError(w, "listTools", err)
		return
	}
	defer rows.Close()
	out := []Tool{}
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			serverError(w, "listTools scan", err)
			return
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listTools rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createTool: POST /api/integrations/resources/{id}/tools. Solo per server mcp_server. Risponde 201.
func (s *Server) createTool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeToolInput(w, r)
	if !ok {
		return
	}
	kind, _, _, err := s.mcpResource(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "risorsa")
		return
	}
	if err != nil {
		serverError(w, "createTool resource", err)
		return
	}
	if kind != "mcp_server" {
		writeValidation(w, "gli strumenti si registrano solo su risorse di tipo mcp_server")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createTool begin", err)
		return
	}
	defer tx.Rollback()
	var tid int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.mcp_tool (resource_id, name, description, risk, requires_approval)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4, @p5)`,
		id, in.Name, nullable(in.Description), in.Risk, in.RequiresApproval).Scan(&tid)
	if err != nil {
		writeDBError(w, "createTool insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "mcp_tool", tid,
		auditDetail("tool_saved", map[string]any{"name": in.Name, "risk": in.Risk})); err != nil {
		serverError(w, "createTool audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createTool commit", err)
		return
	}
	t, err := s.fetchTool(r.Context(), tid)
	if err != nil {
		serverError(w, "createTool fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// updateTool: PUT /api/integrations/tools/{tool_id}. Sostituzione completa.
func (s *Server) updateTool(w http.ResponseWriter, r *http.Request) {
	id, ok := toolPathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeToolInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateTool begin", err)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.mcp_tool SET name = @p1, description = @p2, risk = @p3, requires_approval = @p4 WHERE id = @p5`,
		in.Name, nullable(in.Description), in.Risk, in.RequiresApproval, id)
	if err != nil {
		writeDBError(w, "updateTool update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "strumento")
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "mcp_tool", id,
		auditDetail("tool_saved", map[string]any{"name": in.Name, "risk": in.Risk})); err != nil {
		serverError(w, "updateTool audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateTool commit", err)
		return
	}
	t, err := s.fetchTool(r.Context(), id)
	if err != nil {
		serverError(w, "updateTool fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// deleteTool: DELETE /api/integrations/tools/{tool_id}. Le voci di permesso collegate si rimuovono. Risponde 204.
func (s *Server) deleteTool(w http.ResponseWriter, r *http.Request) {
	id, ok := toolPathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteTool begin", err)
		return
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRowContext(r.Context(),
		`SELECT name FROM dbo.mcp_tool WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "strumento")
		return
	}
	if err != nil {
		serverError(w, "deleteTool read", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.mcp_tool WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteTool delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "mcp_tool", id,
		auditDetail("tool_deleted", map[string]any{"name": name})); err != nil {
		serverError(w, "deleteTool audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteTool commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// syncTools: POST /api/integrations/resources/{id}/tools/sync. Legge tools/list dal server MCP.
// Aggiunge gli strumenti nuovi (rischio basso, senza approvazione) e aggiorna la descrizione di quelli noti.
// Gli strumenti non più presenti sul server restano registrati. Se il server non risponde: 502 provider_error.
func (s *Server) syncTools(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	kind, _, endpoint, err := s.mcpResource(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "risorsa")
		return
	}
	if err != nil {
		serverError(w, "sync resource", err)
		return
	}
	if kind != "mcp_server" {
		writeValidation(w, "la sincronizzazione vale solo per risorse di tipo mcp_server")
		return
	}
	raw, err := mcpRPC(r.Context(), endpoint, "tools/list", map[string]any{})
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		writeError(w, http.StatusBadGateway, "provider_error", "risposta tools/list non valida")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "sync begin", err)
		return
	}
	defer tx.Rollback()
	added, updated := 0, 0
	for _, t := range list.Tools {
		name := strings.TrimSpace(t.Name)
		if name == "" || runeLen(name) > 150 {
			continue
		}
		desc := t.Description
		if runeLen(desc) > 1000 {
			desc = string([]rune(desc)[:1000])
		}
		var existing int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM dbo.mcp_tool WHERE resource_id = @p1 AND name = @p2`, id, name).Scan(&existing); err != nil {
			serverError(w, "sync exists", err)
			return
		}
		if existing == 0 {
			if _, err := tx.ExecContext(r.Context(),
				`INSERT INTO dbo.mcp_tool (resource_id, name, description, risk, requires_approval) VALUES (@p1, @p2, @p3, 'low', 0)`,
				id, name, nullable(desc)); err != nil {
				serverError(w, "sync insert", err)
				return
			}
			added++
		} else {
			if _, err := tx.ExecContext(r.Context(),
				`UPDATE dbo.mcp_tool SET description = @p3 WHERE resource_id = @p1 AND name = @p2`,
				id, name, nullable(desc)); err != nil {
				serverError(w, "sync update", err)
				return
			}
			updated++
		}
	}
	if err := writeAudit(r.Context(), tx, "update", "integration_resource", id,
		auditDetail("tools_synced", map[string]any{"added": added, "updated": updated})); err != nil {
		serverError(w, "sync audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "sync commit", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added, "updated": updated, "total": len(list.Tools)})
}

// createToolAccess: POST /api/integrations/tool-allowlist. Risponde 201; duplicato 409.
func (s *Server) createToolAccess(w http.ResponseWriter, r *http.Request) {
	var in ToolAccessInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.AgentID <= 0 || in.ToolID <= 0 {
		writeValidation(w, "agent_id e tool_id sono obbligatori")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createToolAccess begin", err)
		return
	}
	defer tx.Rollback()
	// La coppia agente-server deve essere in allowlist: lo strumento non basta da solo.
	var pair int
	err = tx.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM dbo.agent_access aa
		   JOIN dbo.mcp_tool t ON t.resource_id = aa.resource_id
		  WHERE aa.agent_id = @p1 AND t.id = @p2`, in.AgentID, in.ToolID).Scan(&pair)
	if err != nil {
		serverError(w, "createToolAccess pair", err)
		return
	}
	if pair == 0 {
		writeValidation(w, "prima serve la coppia agente-server in allowlist, e lo strumento deve appartenere a quel server")
		return
	}
	var tid int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.tool_access (agent_id, tool_id, requires_approval)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3)`, in.AgentID, in.ToolID, in.RequiresApproval).Scan(&tid)
	if err != nil {
		writeDBError(w, "createToolAccess insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "tool_access", tid,
		auditDetail("tool_access_added", map[string]any{"agent_id": in.AgentID, "tool_id": in.ToolID,
			"requires_approval": in.RequiresApproval})); err != nil {
		serverError(w, "createToolAccess audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createToolAccess commit", err)
		return
	}
	var out ToolAccess
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT ta.id, ta.agent_id, ta.tool_id, t.name, ta.requires_approval, ta.created_at
		   FROM dbo.tool_access ta JOIN dbo.mcp_tool t ON t.id = ta.tool_id WHERE ta.id = @p1`, tid).
		Scan(&out.ID, &out.AgentID, &out.ToolID, &out.ToolName, &out.RequiresApproval, &out.CreatedAt); err != nil {
		serverError(w, "createToolAccess fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// listToolAccess: GET /api/integrations/tool-allowlist?agent_id=
func (s *Server) listToolAccess(w http.ResponseWriter, r *http.Request) {
	agentID, ok := optionalID(w, r.URL.Query().Get("agent_id"), "agent_id")
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT ta.id, ta.agent_id, ta.tool_id, t.name, ta.requires_approval, ta.created_at
		   FROM dbo.tool_access ta JOIN dbo.mcp_tool t ON t.id = ta.tool_id
		  WHERE (@p1 = 0 OR ta.agent_id = @p1) ORDER BY ta.agent_id, t.name`, agentID)
	if err != nil {
		serverError(w, "listToolAccess", err)
		return
	}
	defer rows.Close()
	out := []ToolAccess{}
	for rows.Next() {
		var a ToolAccess
		if err := rows.Scan(&a.ID, &a.AgentID, &a.ToolID, &a.ToolName, &a.RequiresApproval, &a.CreatedAt); err != nil {
			serverError(w, "listToolAccess scan", err)
			return
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listToolAccess rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteToolAccess: DELETE /api/integrations/tool-allowlist/{id}. Risponde 204.
func (s *Server) deleteToolAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteToolAccess begin", err)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.tool_access WHERE id = @p1`, id)
	if err != nil {
		serverError(w, "deleteToolAccess delete", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "permesso dello strumento")
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "tool_access", id,
		auditDetail("tool_access_removed", nil)); err != nil {
		serverError(w, "deleteToolAccess audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteToolAccess commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toolPathID legge il parametro {tool_id} dal percorso.
func toolPathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := parsePathInt(r, "tool_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "tool_id non valido")
		return 0, false
	}
	return id, true
}
