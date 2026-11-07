// Open-GenCut - Aggiornamento dei quadri di conformità da un bundle (POST /api/compliance/frameworks/import).
//
// Il bundle è {"framework": {code, name?, version?, source_url?, validation_note?}, "controls": [{code, title, description?}]}.
// Il quadro deve già esistere (riconosciuto per codice): l'import ne aggiorna versione, fonte e
// nota, registra la data di aggiornamento (updated_at) e crea o aggiorna i controlli per codice.
// I controlli non presenti nel bundle non vengono cancellati. Un codice sconosciuto risponde 404
// e non cambia nulla.
package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

const importMaxControls = 1000

// FrameworkBundleControl è un controllo nel bundle.
type FrameworkBundleControl struct {
	Code        string `json:"code"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// FrameworkBundleHeader è la parte "framework" del bundle.
type FrameworkBundleHeader struct {
	Code           string `json:"code"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	SourceURL      string `json:"source_url"`
	ValidationNote string `json:"validation_note"`
}

// FrameworkBundle è il corpo di POST /api/compliance/frameworks/import.
type FrameworkBundle struct {
	Framework FrameworkBundleHeader    `json:"framework"`
	Controls  []FrameworkBundleControl `json:"controls"`
}

func (s *Server) registerComplianceImport(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/compliance/frameworks/import", s.importFramework)
}

// importFramework: POST /api/compliance/frameworks/import. Risponde 200.
func (s *Server) importFramework(w http.ResponseWriter, r *http.Request) {
	var in FrameworkBundle
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Framework.Code = strings.TrimSpace(in.Framework.Code)
	in.Framework.Name = strings.TrimSpace(in.Framework.Name)
	h := in.Framework
	if h.Code == "" || runeLen(h.Code) > 40 {
		writeValidation(w, "framework.code è obbligatorio (massimo 40 caratteri)")
		return
	}
	if runeLen(h.Name) > 150 || runeLen(h.Version) > 50 || runeLen(h.SourceURL) > 300 || runeLen(h.ValidationNote) > 300 {
		writeValidation(w, "name, version, source_url o validation_note superano il limite di lunghezza")
		return
	}
	if len(in.Controls) > importMaxControls {
		writeValidation(w, "il bundle supera "+strconv.Itoa(importMaxControls)+" controlli")
		return
	}
	for i := range in.Controls {
		c := &in.Controls[i]
		c.Code = strings.TrimSpace(c.Code)
		c.Title = strings.TrimSpace(c.Title)
		if c.Code == "" || runeLen(c.Code) > 40 || c.Title == "" || runeLen(c.Title) > 200 {
			writeValidation(w, "ogni controllo richiede code (massimo 40) e title (massimo 200)")
			return
		}
		if runeLen(c.Description) > 1000 {
			writeValidation(w, "description di un controllo supera 1000 caratteri")
			return
		}
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "importFramework begin", err)
		return
	}
	defer tx.Rollback()

	var id int
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM dbo.compliance_framework WITH (UPDLOCK, ROWLOCK) WHERE code = @p1`, h.Code).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "framework con questo code")
		return
	}
	if err != nil {
		serverError(w, "importFramework read", err)
		return
	}

	// Versione, fonte e nota: solo i campi presenti nel bundle sostituiscono i valori attuali.
	if _, err := tx.ExecContext(ctx,
		`UPDATE dbo.compliance_framework
		    SET name = COALESCE(@p1, name),
		        version = COALESCE(@p2, version),
		        source_url = COALESCE(@p3, source_url),
		        validation_note = COALESCE(@p4, validation_note),
		        updated_at = SYSUTCDATETIME()
		  WHERE id = @p5`,
		nullable(h.Name), nullable(h.Version), nullable(h.SourceURL), nullable(h.ValidationNote), id); err != nil {
		writeDBError(w, "importFramework update", err)
		return
	}

	created, updated := 0, 0
	for _, c := range in.Controls {
		var action string
		err := tx.QueryRowContext(ctx,
			`MERGE dbo.compliance_control AS t
			 USING (SELECT @p1 AS framework_id, @p2 AS code, @p3 AS title, @p4 AS description) AS s
			    ON t.framework_id = s.framework_id AND t.code = s.code
			 WHEN MATCHED THEN UPDATE SET title = s.title, description = s.description
			 WHEN NOT MATCHED THEN INSERT (framework_id, code, title, description)
			      VALUES (s.framework_id, s.code, s.title, s.description)
			 OUTPUT $action;`,
			id, c.Code, c.Title, nullable(c.Description)).Scan(&action)
		if err != nil {
			writeDBError(w, "importFramework control", err)
			return
		}
		if action == "INSERT" {
			created++
		} else {
			updated++
		}
	}

	if err := writeAudit(ctx, tx, "update", "compliance_framework", id,
		auditDetail("framework_imported", map[string]any{
			"code": h.Code, "version": h.Version, "controls_created": created, "controls_updated": updated,
		})); err != nil {
		serverError(w, "importFramework audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "importFramework commit", err)
		return
	}

	fws, err := s.loadFrameworks(ctx)
	if err != nil {
		serverError(w, "importFramework load", err)
		return
	}
	for _, f := range fws {
		if f.ID == id {
			writeJSON(w, http.StatusOK, map[string]any{
				"framework":        f,
				"controls_created": created,
				"controls_updated": updated,
			})
			return
		}
	}
	writeNotFound(w, "framework")
}
