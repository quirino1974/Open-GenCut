// Open-GenCut v0.1 - Punto 4, R1: approvazione umana per azioni sensibili.
//
// Una voce di allowlist con requires_approval=true marca la coppia agente-risorsa come sensibile.
// Ogni accesso a una coppia sensibile richiede un'approvazione:
//  1. POST /api/integrations/check senza approvazione valida risponde denied con reason
//     "approval_required" e apre automaticamente una richiesta in stato pending (approval_id).
//  2. Un approvatore approva o rifiuta la richiesta (POST .../approve o .../reject).
//  3. Il successivo check con approvazione valida risponde allowed (reason "ok_approved")
//     e la richiesta passa a "used": l'approvazione vale per un solo accesso.
//
// Stati: pending -> approved -> used; pending -> rejected.
package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

var validApprovalStatuses = map[string]bool{"pending": true, "approved": true, "rejected": true, "used": true}

// Approval è una richiesta di approvazione per un accesso sensibile.
type Approval struct {
	ID            int        `json:"id"`
	AgentID       int        `json:"agent_id"`
	ResourceID    int        `json:"resource_id"`
	Status        string     `json:"status"`
	RequestedBy   string     `json:"requested_by"`
	Justification *string    `json:"justification"`
	DecidedBy     *string    `json:"decided_by"`
	DecisionNote  *string    `json:"decision_note"`
	CreatedAt     time.Time  `json:"created_at"`
	DecidedAt     *time.Time `json:"decided_at"`
	UsedAt        *time.Time `json:"used_at"`
}

// ApprovalInput è il corpo di POST /api/integrations/approvals.
type ApprovalInput struct {
	AgentID       int    `json:"agent_id"`
	ResourceID    int    `json:"resource_id"`
	Justification string `json:"justification"`
}

// DecisionInput è il corpo facoltativo di approve e reject.
type DecisionInput struct {
	Note string `json:"note"`
}

// registerApprovals aggiunge le rotte di approvazione al mux principale.
func (s *Server) registerApprovals(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations/approvals", s.listApprovals)
	mux.HandleFunc("POST /api/integrations/approvals", s.createApproval)
	mux.HandleFunc("POST /api/integrations/approvals/{id}/approve", s.approveApproval)
	mux.HandleFunc("POST /api/integrations/approvals/{id}/reject", s.rejectApproval)
}

const approvalColumns = `a.id, a.agent_id, a.resource_id, a.status, a.requested_by, a.justification,
	a.decided_by, a.decision_note, a.created_at, a.decided_at, a.used_at`

func scanApproval(sc rowScanner) (Approval, error) {
	var a Approval
	var just, by, note sql.NullString
	var decided, used sql.NullTime
	if err := sc.Scan(&a.ID, &a.AgentID, &a.ResourceID, &a.Status, &a.RequestedBy, &just,
		&by, &note, &a.CreatedAt, &decided, &used); err != nil {
		return Approval{}, err
	}
	a.Justification = ptrOf(just)
	a.DecidedBy = ptrOf(by)
	a.DecisionNote = ptrOf(note)
	if decided.Valid {
		t := decided.Time
		a.DecidedAt = &t
	}
	if used.Valid {
		t := used.Time
		a.UsedAt = &t
	}
	return a, nil
}

// listApprovals: GET /api/integrations/approvals?status=&agent_id=
func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	status := qs.Get("status")
	if status != "" && !validApprovalStatuses[status] {
		writeValidation(w, "status deve essere pending, approved, rejected o used")
		return
	}
	agentID, ok := optionalID(w, qs.Get("agent_id"), "agent_id")
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+approvalColumns+` FROM dbo.integration_approval a
		  WHERE (@p1 = N'' OR a.status = @p1) AND (@p2 = 0 OR a.agent_id = @p2)
		  ORDER BY a.id DESC`, status, agentID)
	if err != nil {
		serverError(w, "listApprovals", err)
		return
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			serverError(w, "listApprovals scan", err)
			return
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listApprovals rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createApproval: POST /api/integrations/approvals. Richiesta manuale. Risponde 201.
// La coppia deve essere in allowlist e marcata come sensibile.
func (s *Server) createApproval(w http.ResponseWriter, r *http.Request) {
	var in ApprovalInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.AgentID <= 0 || in.ResourceID <= 0 {
		writeValidation(w, "agent_id e resource_id sono obbligatori")
		return
	}
	if runeLen(in.Justification) > 500 {
		writeValidation(w, "justification supera 500 caratteri")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createApproval begin", err)
		return
	}
	defer tx.Rollback()

	var sensitive bool
	err = tx.QueryRowContext(r.Context(),
		`SELECT requires_approval FROM dbo.agent_access WHERE agent_id = @p1 AND resource_id = @p2`,
		in.AgentID, in.ResourceID).Scan(&sensitive)
	if errors.Is(err, sql.ErrNoRows) {
		writeValidation(w, "la coppia agente-risorsa non è in allowlist")
		return
	}
	if err != nil {
		serverError(w, "createApproval pair", err)
		return
	}
	if !sensitive {
		writeValidation(w, "la coppia agente-risorsa non è marcata come sensibile")
		return
	}

	id, err := insertPendingApproval(r.Context(), tx, in.AgentID, in.ResourceID, 0, in.Justification)
	if err != nil {
		serverError(w, "createApproval insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "approval", id, auditDetail("approval_requested", nil)); err != nil {
		serverError(w, "createApproval audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createApproval commit", err)
		return
	}
	a, err := s.fetchApproval(r.Context(), id)
	if err != nil {
		serverError(w, "createApproval fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// approveApproval: POST /api/integrations/approvals/{id}/approve. Solo da pending.
func (s *Server) approveApproval(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, "approved", "approve")
}

// rejectApproval: POST /api/integrations/approvals/{id}/reject. Solo da pending.
func (s *Server) rejectApproval(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, "rejected", "reject")
}

// decideApproval porta una richiesta pending verso approved o rejected.
func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request, to, action string) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in DecisionInput
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	if runeLen(in.Note) > 500 {
		writeValidation(w, "note supera 500 caratteri")
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, action+" begin", err)
		return
	}
	defer tx.Rollback()

	var current string
	err = tx.QueryRowContext(r.Context(),
		`SELECT status FROM dbo.integration_approval WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "richiesta di approvazione")
		return
	}
	if err != nil {
		serverError(w, action+" read", err)
		return
	}
	if current != "pending" {
		writeInvalidState(w, "transizione non consentita: "+action+" da "+current)
		return
	}

	if _, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.integration_approval
		    SET status = @p1, decided_by = @p4, decided_at = SYSUTCDATETIME(), decision_note = @p2
		  WHERE id = @p3`, to, nullable(in.Note), id, actorFrom(r.Context())); err != nil {
		serverError(w, action+" update", err)
		return
	}
	if err := writeAudit(r.Context(), tx, action, "approval", id, auditDetail("approval_"+to, nil)); err != nil {
		serverError(w, action+" audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, action+" commit", err)
		return
	}
	a, err := s.fetchApproval(r.Context(), id)
	if err != nil {
		serverError(w, action+" fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) fetchApproval(ctx context.Context, id int) (Approval, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+approvalColumns+` FROM dbo.integration_approval a WHERE a.id = @p1`, id)
	return scanApproval(row)
}

// insertPendingApproval crea una richiesta in stato pending e ne restituisce l'id.
func insertPendingApproval(ctx context.Context, tx *sql.Tx, agentID, resourceID, toolID int, justification string) (int, error) {
	var id int
	err := tx.QueryRowContext(ctx,
		`INSERT INTO dbo.integration_approval (agent_id, resource_id, tool_id, status, requested_by, justification)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, 'pending', @p4, @p5)`,
		agentID, resourceID, nullTool(toolID), actorFrom(ctx), nullable(strings.TrimSpace(justification))).Scan(&id)
	return id, err
}

// approvalGate decide un accesso a una coppia sensibile. Va chiamata in checkAccess, dentro la transazione,
// dopo i controlli su stato, allowlist e limiti. Restituisce decisione, motivo e id della richiesta.
//   - approvazione valida presente: la consuma (used) e permette l'accesso;
//   - nessuna approvazione: riusa o apre una richiesta pending e nega con approval_required.
func approvalGate(ctx context.Context, tx *sql.Tx, agentID, resourceID, toolID int) (string, string, *int) {
	var id int
	err := tx.QueryRowContext(ctx,
		`SELECT TOP (1) id FROM dbo.integration_approval WITH (UPDLOCK, ROWLOCK)
		  WHERE agent_id = @p1 AND resource_id = @p2 AND ISNULL(tool_id, 0) = @p3 AND status = 'approved'
		  ORDER BY id`, agentID, resourceID, toolID).Scan(&id)
	if err == nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE dbo.integration_approval SET status = 'used', used_at = SYSUTCDATETIME() WHERE id = @p1`,
			id); err != nil {
			return "denied", "check_error", nil
		}
		return "allowed", "ok_approved", &id
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "denied", "check_error", nil
	}

	err = tx.QueryRowContext(ctx,
		`SELECT TOP (1) id FROM dbo.integration_approval WITH (UPDLOCK, ROWLOCK)
		  WHERE agent_id = @p1 AND resource_id = @p2 AND ISNULL(tool_id, 0) = @p3 AND status = 'pending'
		  ORDER BY id`, agentID, resourceID, toolID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id, err = insertPendingApproval(ctx, tx, agentID, resourceID, toolID, "richiesta automatica da /check")
	}
	if err != nil {
		return "denied", "check_error", nil
	}
	return "denied", "approval_required", &id
}

// nullTool restituisce NULL per il livello risorsa (toolID 0), altrimenti l'id dello strumento.
func nullTool(toolID int) any {
	if toolID == 0 {
		return nil
	}
	return toolID
}
