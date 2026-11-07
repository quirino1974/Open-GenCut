// Open-GenCut - Caratteristica chiave: identità per agente con revoca.
//
// Ogni agente ha un UUID di identità generato alla creazione. La revoca è definitiva:
// dopo la revoca, /api/integrations/check nega con agent_revoked (dopo il controllo sulla
// risorsa disattivata e prima di ogni altro controllo).
// La sospensione blocca l'agente subito; la riattivazione richiede una chiamata esplicita.
package main

import (
	"database/sql"
	"errors"
	"net/http"
	"time"
)

// Identity è l'identità di un agente.
type Identity struct {
	AgentID    int        `json:"agent_id"`
	IdentityID string     `json:"identity_id"`
	Revoked    bool       `json:"revoked"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// registerIdentity aggiunge le rotte di identità al mux principale.
func (s *Server) registerIdentity(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/identity/agents/{id}", s.getIdentity)
	mux.HandleFunc("POST /api/identity/agents/{id}/revoke", s.revokeIdentity)
}

func (s *Server) fetchIdentity(r *http.Request, id int) (Identity, error) {
	var ident Identity
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, CAST(identity_id AS NVARCHAR(36)), identity_revoked_at FROM dbo.agent WHERE id = @p1`, id).
		Scan(&ident.AgentID, &ident.IdentityID, &revokedAt)
	if err != nil {
		return ident, err
	}
	if revokedAt.Valid {
		t := revokedAt.Time
		ident.RevokedAt = &t
		ident.Revoked = true
	}
	return ident, nil
}

// getIdentity: GET /api/identity/agents/{id}
func (s *Server) getIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ident, err := s.fetchIdentity(r, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "agente")
		return
	}
	if err != nil {
		serverError(w, "getIdentity", err)
		return
	}
	writeJSON(w, http.StatusOK, ident)
}

// revokeIdentity: POST /api/identity/agents/{id}/revoke. Definitiva: una seconda revoca dà 409.
func (s *Server) revokeIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "revoke begin", err)
		return
	}
	defer tx.Rollback()
	var name string
	var revokedAt sql.NullTime
	err = tx.QueryRowContext(r.Context(),
		`SELECT name, identity_revoked_at FROM dbo.agent WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).
		Scan(&name, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "agente")
		return
	}
	if err != nil {
		serverError(w, "revoke read", err)
		return
	}
	if revokedAt.Valid {
		writeInvalidState(w, "identità già revocata")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.agent SET identity_revoked_at = SYSUTCDATETIME(), updated_at = SYSUTCDATETIME() WHERE id = @p1`,
		id); err != nil {
		serverError(w, "revoke update", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "agent", id,
		auditDetail("identity_revoked", map[string]any{"name": name})); err != nil {
		serverError(w, "revoke audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "revoke commit", err)
		return
	}
	ident, err := s.fetchIdentity(r, id)
	if err != nil {
		serverError(w, "revoke fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, ident)
}
