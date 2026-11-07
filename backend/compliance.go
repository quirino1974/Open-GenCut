// Open-GenCut - Punto 4 della specifica: conformità.
//
// Contiene:
//   - misure di sicurezza dei dati per risorsa (GET/PUT .../resources/{id}/security);
//   - politiche di governance con quattro tipi di regola (CRUD) e verifica per agente;
//   - librerie di framework e controlli, con seed da validare (CRUD);
//   - valutazione dei controlli per agente (GET/PUT);
//   - vista d'insieme per framework (GET /overview).
//
// I gateway AI sono risorse di tipo ai_gateway (vedi integrations.go).
// Ogni scrittura produce una riga di audit con chiave tradotta.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var (
	classRank = map[string]int{"public": 0, "internal": 1, "confidential": 2, "restricted": 3}
	ruleTypes = map[string]bool{"transparency_required": true, "evaluation_before_production": true,
		"max_classification": true, "pii_requires_approval": true}
	assessmentStatuses = map[string]bool{"compliant": true, "non_compliant": true,
		"not_assessed": true, "not_applicable": true}
)

// ---------------------------------------------------------------------------
// Modelli
// ---------------------------------------------------------------------------

// Security è la misura di sicurezza dei dati di una risorsa.
type Security struct {
	ResourceID         int     `json:"resource_id"`
	Classification     string  `json:"classification"`
	EncryptedAtRest    bool    `json:"encrypted_at_rest"`
	EncryptedInTransit bool    `json:"encrypted_in_transit"`
	PIIAllowed         bool    `json:"pii_allowed"`
	RetentionDays      *int    `json:"retention_days"`
	UpdatedAt          *string `json:"updated_at"`
}

// SecurityInput è il corpo di PUT .../security (sostituzione completa).
type SecurityInput struct {
	Classification     string `json:"classification"`
	EncryptedAtRest    bool   `json:"encrypted_at_rest"`
	EncryptedInTransit bool   `json:"encrypted_in_transit"`
	PIIAllowed         bool   `json:"pii_allowed"`
	RetentionDays      *int   `json:"retention_days"`
}

// Policy è una politica di governance.
type Policy struct {
	ID          int             `json:"id"`
	Name        string          `json:"name"`
	Description *string         `json:"description"`
	RuleType    string          `json:"rule_type"`
	Params      json.RawMessage `json:"params"`
	Enabled     bool            `json:"enabled"`
}

// PolicyInput è il corpo di POST e PUT delle politiche.
type PolicyInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	RuleType    string          `json:"rule_type"`
	Params      json.RawMessage `json:"params"`
	Enabled     *bool           `json:"enabled"` // default true
}

// PolicyResult è l'esito di una politica per un agente.
type PolicyResult struct {
	PolicyID int    `json:"policy_id"`
	Name     string `json:"name"`
	RuleType string `json:"rule_type"`
	Pass     bool   `json:"pass"`
	Reason   string `json:"reason"`
}

// Framework è una libreria di controlli, con i suoi controlli.
type Framework struct {
	ID             int       `json:"id"`
	Code           string    `json:"code"`
	Name           string    `json:"name"`
	Version        *string   `json:"version"`
	SourceURL      *string   `json:"source_url"`
	ValidationNote *string   `json:"validation_note"`
	UpdatedAt      string    `json:"updated_at"`
	Controls       []Control `json:"controls"`
}

// FrameworkInput è il corpo di POST e PUT dei framework.
type FrameworkInput struct {
	Code           string `json:"code"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	SourceURL      string `json:"source_url"`
	ValidationNote string `json:"validation_note"`
}

// Control è un controllo di un framework.
type Control struct {
	ID          int     `json:"id"`
	FrameworkID int     `json:"framework_id"`
	Code        string  `json:"code"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
}

// ControlInput è il corpo di POST e PUT dei controlli.
type ControlInput struct {
	FrameworkID int    `json:"framework_id"`
	Code        string `json:"code"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// Assessment è la valutazione di un controllo per un agente.
type Assessment struct {
	ControlID     int     `json:"control_id"`
	FrameworkCode string  `json:"framework_code"`
	ControlCode   string  `json:"control_code"`
	Title         string  `json:"title"`
	Status        string  `json:"status"`
	EvidenceNote  *string `json:"evidence_note"`
	UpdatedAt     *string `json:"updated_at"`
}

// AssessmentInput è il corpo di PUT .../assessments/{control_id}.
type AssessmentInput struct {
	Status       string `json:"status"`
	EvidenceNote string `json:"evidence_note"`
}

// ---------------------------------------------------------------------------
// Rotte
// ---------------------------------------------------------------------------

// registerCompliance aggiunge le rotte di conformità al mux principale.
func (s *Server) registerCompliance(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/compliance/resources/{id}/security", s.getSecurity)
	mux.HandleFunc("PUT /api/compliance/resources/{id}/security", s.putSecurity)

	mux.HandleFunc("GET /api/compliance/policies", s.listPolicies)
	mux.HandleFunc("POST /api/compliance/policies", s.createPolicy)
	mux.HandleFunc("GET /api/compliance/policies/{id}", s.getPolicy)
	mux.HandleFunc("PUT /api/compliance/policies/{id}", s.updatePolicy)
	mux.HandleFunc("DELETE /api/compliance/policies/{id}", s.deletePolicy)
	mux.HandleFunc("GET /api/compliance/agents/{id}/policies", s.agentPolicies)

	mux.HandleFunc("GET /api/compliance/frameworks", s.listFrameworks)
	mux.HandleFunc("POST /api/compliance/frameworks", s.createFramework)
	mux.HandleFunc("PUT /api/compliance/frameworks/{id}", s.updateFramework)
	mux.HandleFunc("DELETE /api/compliance/frameworks/{id}", s.deleteFramework)
	mux.HandleFunc("POST /api/compliance/controls", s.createControl)
	mux.HandleFunc("PUT /api/compliance/controls/{id}", s.updateControl)
	mux.HandleFunc("DELETE /api/compliance/controls/{id}", s.deleteControl)

	mux.HandleFunc("GET /api/compliance/agents/{id}/assessments", s.listAssessments)
	mux.HandleFunc("PUT /api/compliance/agents/{id}/assessments/{control_id}", s.putAssessment)

	mux.HandleFunc("GET /api/compliance/overview", s.complianceOverview)
}

// ---------------------------------------------------------------------------
// Misure di sicurezza dei dati
// ---------------------------------------------------------------------------

// resourceExists verifica che la risorsa esista; restituisce false e risponde 404 se manca.
func (s *Server) resourceExists(w http.ResponseWriter, r *http.Request, id int) bool {
	var n int
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM dbo.integration_resource WHERE id = @p1`, id).Scan(&n); err != nil {
		serverError(w, "resourceExists", err)
		return false
	}
	if n == 0 {
		writeNotFound(w, "risorsa")
		return false
	}
	return true
}

// getSecurity: GET /api/compliance/resources/{id}/security. Senza riga, valgono i default.
func (s *Server) getSecurity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !s.resourceExists(w, r, id) {
		return
	}
	sec := Security{ResourceID: id, Classification: "internal"}
	var at sql.NullTime
	var retention sql.NullInt64
	err := s.db.QueryRowContext(r.Context(),
		`SELECT classification, encrypted_at_rest, encrypted_in_transit, pii_allowed, retention_days, updated_at
		   FROM dbo.resource_security WHERE resource_id = @p1`, id).
		Scan(&sec.Classification, &sec.EncryptedAtRest, &sec.EncryptedInTransit, &sec.PIIAllowed, &retention, &at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		serverError(w, "getSecurity", err)
		return
	}
	if retention.Valid {
		v := int(retention.Int64)
		sec.RetentionDays = &v
	}
	if at.Valid {
		t := at.Time.UTC().Format("2006-01-02T15:04:05.000Z")
		sec.UpdatedAt = &t
	}
	writeJSON(w, http.StatusOK, sec)
}

// putSecurity: PUT /api/compliance/resources/{id}/security. Sostituzione completa, upsert.
func (s *Server) putSecurity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in SecurityInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if !validClassification(in.Classification) {
		writeValidation(w, "classification deve essere public, internal, confidential o restricted")
		return
	}
	if in.RetentionDays != nil && (*in.RetentionDays < 0 || *in.RetentionDays > maxInt32) {
		writeValidation(w, "retention_days deve essere maggiore o uguale a 0")
		return
	}
	if !s.resourceExists(w, r, id) {
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "putSecurity begin", err)
		return
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(r.Context(),
		`MERGE dbo.resource_security AS t
		 USING (SELECT @p1 AS resource_id) AS s ON t.resource_id = s.resource_id
		 WHEN MATCHED THEN UPDATE SET classification = @p2, encrypted_at_rest = @p3, encrypted_in_transit = @p4,
		      pii_allowed = @p5, retention_days = @p6, updated_at = SYSUTCDATETIME()
		 WHEN NOT MATCHED THEN INSERT (resource_id, classification, encrypted_at_rest, encrypted_in_transit,
		      pii_allowed, retention_days) VALUES (@p1, @p2, @p3, @p4, @p5, @p6);`,
		id, in.Classification, in.EncryptedAtRest, in.EncryptedInTransit, in.PIIAllowed, intParam(in.RetentionDays))
	if err != nil {
		writeDBError(w, "putSecurity upsert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "resource_security", id,
		auditDetail("resource_security_saved", map[string]any{"resource_id": id, "classification": in.Classification})); err != nil {
		serverError(w, "putSecurity audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "putSecurity commit", err)
		return
	}
	s.getSecurityAfterWrite(w, r, id)
}

// getSecurityAfterWrite rilegge e restituisce la misura appena salvata.
func (s *Server) getSecurityAfterWrite(w http.ResponseWriter, r *http.Request, id int) {
	var sec Security
	var at sql.NullTime
	var retention sql.NullInt64
	sec.ResourceID = id
	err := s.db.QueryRowContext(r.Context(),
		`SELECT classification, encrypted_at_rest, encrypted_in_transit, pii_allowed, retention_days, updated_at
		   FROM dbo.resource_security WHERE resource_id = @p1`, id).
		Scan(&sec.Classification, &sec.EncryptedAtRest, &sec.EncryptedInTransit, &sec.PIIAllowed, &retention, &at)
	if err != nil {
		serverError(w, "getSecurityAfterWrite", err)
		return
	}
	if retention.Valid {
		v := int(retention.Int64)
		sec.RetentionDays = &v
	}
	if at.Valid {
		t := at.Time.UTC().Format("2006-01-02T15:04:05.000Z")
		sec.UpdatedAt = &t
	}
	writeJSON(w, http.StatusOK, sec)
}

func validClassification(c string) bool {
	_, ok := classRank[c]
	return ok
}

// ---------------------------------------------------------------------------
// Politiche
// ---------------------------------------------------------------------------

const policyColumns = `p.id, p.name, p.description, p.rule_type, p.params, p.enabled`

func scanPolicy(sc rowScanner) (Policy, error) {
	var p Policy
	var desc sql.NullString
	var params string
	if err := sc.Scan(&p.ID, &p.Name, &desc, &p.RuleType, &params, &p.Enabled); err != nil {
		return Policy{}, err
	}
	p.Description = ptrOf(desc)
	p.Params = json.RawMessage(params)
	return p, nil
}

// decodePolicyInput legge e valida una politica. I parametri devono essere un oggetto JSON.
func decodePolicyInput(w http.ResponseWriter, r *http.Request) (PolicyInput, bool) {
	var in PolicyInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || runeLen(in.Name) > 150 {
		writeValidation(w, "name è obbligatorio (massimo 150 caratteri)")
		return in, false
	}
	if !ruleTypes[in.RuleType] {
		writeValidation(w, "rule_type non valido")
		return in, false
	}
	if runeLen(in.Description) > 1000 {
		writeValidation(w, "description supera 1000 caratteri")
		return in, false
	}
	params, err := normalizeObject(in.Params)
	if err != nil {
		writeValidation(w, "params deve essere un oggetto JSON")
		return in, false
	}
	in.Params = params
	if in.RuleType == "max_classification" {
		var p struct {
			Max string `json:"max"`
		}
		if err := json.Unmarshal(params, &p); err != nil || !validClassification(p.Max) {
			writeValidation(w, `max_classification richiede params {"max": public|internal|confidential|restricted}`)
			return in, false
		}
	}
	return in, true
}

// normalizeObject accetta un oggetto JSON (anche assente, che diventa {}) e lo restituisce compatto.
func normalizeObject(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage("{}"), nil
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, errors.New("non è un oggetto")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

// listPolicies: GET /api/compliance/policies
func (s *Server) listPolicies(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+policyColumns+` FROM dbo.compliance_policy p ORDER BY p.name`)
	if err != nil {
		serverError(w, "listPolicies", err)
		return
	}
	defer rows.Close()
	out := []Policy{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			serverError(w, "listPolicies scan", err)
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listPolicies rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getPolicy: GET /api/compliance/policies/{id}
func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := s.fetchPolicy(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "politica")
		return
	}
	if err != nil {
		serverError(w, "getPolicy", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) fetchPolicy(ctx context.Context, id int) (Policy, error) {
	return scanPolicy(s.db.QueryRowContext(ctx,
		`SELECT `+policyColumns+` FROM dbo.compliance_policy p WHERE p.id = @p1`, id))
}

// createPolicy: POST /api/compliance/policies. Risponde 201.
func (s *Server) createPolicy(w http.ResponseWriter, r *http.Request) {
	in, ok := decodePolicyInput(w, r)
	if !ok {
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createPolicy begin", err)
		return
	}
	defer tx.Rollback()

	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.compliance_policy (name, description, rule_type, params, enabled)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4, @p5)`,
		in.Name, nullable(in.Description), in.RuleType, string(in.Params), enabled).Scan(&id)
	if err != nil {
		writeDBError(w, "createPolicy insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "compliance_policy", id,
		auditDetail("policy_saved", map[string]any{"name": in.Name, "rule_type": in.RuleType})); err != nil {
		serverError(w, "createPolicy audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createPolicy commit", err)
		return
	}
	p, err := s.fetchPolicy(r.Context(), id)
	if err != nil {
		serverError(w, "createPolicy fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// updatePolicy: PUT /api/compliance/policies/{id}. Sostituzione completa.
func (s *Server) updatePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodePolicyInput(w, r)
	if !ok {
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updatePolicy begin", err)
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.compliance_policy
		    SET name = @p1, description = @p2, rule_type = @p3, params = @p4, enabled = @p5,
		        updated_at = SYSUTCDATETIME()
		  WHERE id = @p6`,
		in.Name, nullable(in.Description), in.RuleType, string(in.Params), enabled, id)
	if err != nil {
		writeDBError(w, "updatePolicy update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "politica")
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "compliance_policy", id,
		auditDetail("policy_saved", map[string]any{"name": in.Name, "rule_type": in.RuleType})); err != nil {
		serverError(w, "updatePolicy audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updatePolicy commit", err)
		return
	}
	p, err := s.fetchPolicy(r.Context(), id)
	if err != nil {
		serverError(w, "updatePolicy fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// deletePolicy: DELETE /api/compliance/policies/{id}. Risponde 204.
func (s *Server) deletePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deletePolicy begin", err)
		return
	}
	defer tx.Rollback()

	var name string
	err = tx.QueryRowContext(r.Context(),
		`SELECT name FROM dbo.compliance_policy WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "politica")
		return
	}
	if err != nil {
		serverError(w, "deletePolicy read", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.compliance_policy WHERE id = @p1`, id); err != nil {
		serverError(w, "deletePolicy delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "compliance_policy", id,
		auditDetail("policy_deleted", map[string]any{"name": name})); err != nil {
		serverError(w, "deletePolicy audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deletePolicy commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Verifica delle politiche per agente
// ---------------------------------------------------------------------------

// agentFacts sono i dati dell'agente usati dalle regole di conformità.
type agentFacts struct {
	interacts   bool
	language    string
	prodVersion *string
	lastResult  *string
	lastVersion *string
	resources   []resourceFact
}

type resourceFact struct {
	id       int
	name     string
	class    string
	piiOK    bool // pii_allowed
	approval bool // requires_approval nella voce di allowlist
}

// loadAgentFacts raccoglie i dati dell'agente necessari alle regole.
func (s *Server) loadAgentFacts(ctx context.Context, agentID int) (agentFacts, error) {
	var f agentFacts
	var lang sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT interacts_with_people, interaction_language FROM dbo.agent WHERE id = @p1`, agentID).
		Scan(&f.interacts, &lang); err != nil {
		return f, err
	}
	f.language = lang.String

	var prod sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT version_label FROM dbo.agent_environment WHERE agent_id = @p1 AND environment = 'production'`,
		agentID).Scan(&prod)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return f, err
	}
	f.prodVersion = ptrOf(prod)

	var res, ver sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT TOP (1) result, version_label FROM dbo.agent_evaluation WHERE agent_id = @p1 ORDER BY id DESC`,
		agentID).Scan(&res, &ver)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return f, err
	}
	f.lastResult = ptrOf(res)
	f.lastVersion = ptrOf(ver)

	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.name, ISNULL(sec.classification, 'internal'), ISNULL(sec.pii_allowed, 0), aa.requires_approval
		   FROM dbo.agent_access aa
		   JOIN dbo.integration_resource r ON r.id = aa.resource_id
		   LEFT JOIN dbo.resource_security sec ON sec.resource_id = r.id
		  WHERE aa.agent_id = @p1
		  ORDER BY r.id`, agentID)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	for rows.Next() {
		var rf resourceFact
		if err := rows.Scan(&rf.id, &rf.name, &rf.class, &rf.piiOK, &rf.approval); err != nil {
			return f, err
		}
		f.resources = append(f.resources, rf)
	}
	return f, rows.Err()
}

// evaluatePolicy applica una regola ai dati dell'agente. Restituisce esito e motivo.
func evaluatePolicy(p Policy, f agentFacts) (bool, string) {
	switch p.RuleType {
	case "transparency_required":
		if !f.interacts {
			return true, "l'agente non interagisce con persone"
		}
		if f.language != "" {
			return true, "lingua dichiarata: " + f.language
		}
		return false, "l'agente interagisce con persone ma non dichiara la lingua"

	case "evaluation_before_production":
		if f.prodVersion == nil {
			return true, "nessuna versione in produzione"
		}
		if f.lastResult == nil {
			return false, "nessuna valutazione registrata per la versione in produzione " + *f.prodVersion
		}
		if *f.lastResult != "passed" {
			return false, "ultima valutazione non superata"
		}
		if f.lastVersion == nil || *f.lastVersion != *f.prodVersion {
			return false, "ultima valutazione su una versione diversa da quella in produzione"
		}
		return true, "valutazione superata per la versione in produzione " + *f.prodVersion

	case "max_classification":
		var params struct {
			Max string `json:"max"`
		}
		if err := json.Unmarshal(p.Params, &params); err != nil || !validClassification(params.Max) {
			return false, "parametro max non valido"
		}
		limit := classRank[params.Max]
		var over []string
		for _, rf := range f.resources {
			if classRank[rf.class] > limit {
				over = append(over, fmt.Sprintf("%s (%s)", rf.name, rf.class))
			}
		}
		if len(over) > 0 {
			return false, "risorse oltre il livello " + params.Max + ": " + strings.Join(over, ", ")
		}
		return true, "tutte le risorse in allowlist sono entro il livello " + params.Max

	case "pii_requires_approval":
		var missing []string
		for _, rf := range f.resources {
			if rf.piiOK && !rf.approval {
				missing = append(missing, rf.name)
			}
		}
		if len(missing) > 0 {
			return false, "risorse con dati personali senza approvazione obbligatoria: " + strings.Join(missing, ", ")
		}
		return true, "le risorse con dati personali richiedono approvazione"
	}
	return false, "tipo di regola sconosciuto"
}

// agentPolicies: GET /api/compliance/agents/{id}/policies. Valuta le politiche attive.
func (s *Server) agentPolicies(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	results, err := s.policyResults(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "agente")
		return
	}
	if err != nil {
		serverError(w, "agentPolicies", err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

// policyResults valuta tutte le politiche attive per un agente.
func (s *Server) policyResults(ctx context.Context, agentID int) ([]PolicyResult, error) {
	facts, err := s.loadAgentFacts(ctx, agentID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+policyColumns+` FROM dbo.compliance_policy p WHERE p.enabled = 1 ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	var policies []Policy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		policies = append(policies, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []PolicyResult{}
	for _, p := range policies {
		pass, reason := evaluatePolicy(p, facts)
		out = append(out, PolicyResult{PolicyID: p.ID, Name: p.Name, RuleType: p.RuleType, Pass: pass, Reason: reason})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Framework e controlli
// ---------------------------------------------------------------------------

// listFrameworks: GET /api/compliance/frameworks. Ogni framework riporta i propri controlli.
func (s *Server) listFrameworks(w http.ResponseWriter, r *http.Request) {
	fws, err := s.loadFrameworks(r.Context())
	if err != nil {
		serverError(w, "listFrameworks", err)
		return
	}
	writeJSON(w, http.StatusOK, fws)
}

func (s *Server) loadFrameworks(ctx context.Context) ([]Framework, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, code, name, version, source_url, validation_note, updated_at
		   FROM dbo.compliance_framework ORDER BY code`)
	if err != nil {
		return nil, err
	}
	fws := []Framework{}
	index := map[int]int{}
	for rows.Next() {
		var f Framework
		var ver, src, note sql.NullString
		var at sql.NullTime
		if err := rows.Scan(&f.ID, &f.Code, &f.Name, &ver, &src, &note, &at); err != nil {
			rows.Close()
			return nil, err
		}
		f.Version = ptrOf(ver)
		f.SourceURL = ptrOf(src)
		f.ValidationNote = ptrOf(note)
		if at.Valid {
			f.UpdatedAt = at.Time.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		f.Controls = []Control{}
		index[f.ID] = len(fws)
		fws = append(fws, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	crows, err := s.db.QueryContext(ctx,
		`SELECT id, framework_id, code, title, description FROM dbo.compliance_control ORDER BY framework_id, code`)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		c, err := scanControl(crows)
		if err != nil {
			return nil, err
		}
		if i, ok := index[c.FrameworkID]; ok {
			fws[i].Controls = append(fws[i].Controls, c)
		}
	}
	return fws, crows.Err()
}

func scanControl(sc rowScanner) (Control, error) {
	var c Control
	var desc sql.NullString
	if err := sc.Scan(&c.ID, &c.FrameworkID, &c.Code, &c.Title, &desc); err != nil {
		return Control{}, err
	}
	c.Description = ptrOf(desc)
	return c, nil
}

func decodeFrameworkInput(w http.ResponseWriter, r *http.Request) (FrameworkInput, bool) {
	var in FrameworkInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	in.Code = strings.TrimSpace(in.Code)
	in.Name = strings.TrimSpace(in.Name)
	if in.Code == "" || runeLen(in.Code) > 40 || in.Name == "" || runeLen(in.Name) > 150 {
		writeValidation(w, "code (massimo 40 caratteri) e name (massimo 150) sono obbligatori")
		return in, false
	}
	if runeLen(in.Version) > 50 || runeLen(in.SourceURL) > 300 || runeLen(in.ValidationNote) > 300 {
		writeValidation(w, "version, source_url o validation_note superano il limite di lunghezza")
		return in, false
	}
	return in, true
}

// createFramework: POST /api/compliance/frameworks. Risponde 201.
func (s *Server) createFramework(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeFrameworkInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createFramework begin", err)
		return
	}
	defer tx.Rollback()

	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.compliance_framework (code, name, version, source_url, validation_note)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4, @p5)`,
		in.Code, in.Name, nullable(in.Version), nullable(in.SourceURL), nullable(in.ValidationNote)).Scan(&id)
	if err != nil {
		writeDBError(w, "createFramework insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "compliance_framework", id,
		auditDetail("framework_saved", map[string]any{"code": in.Code})); err != nil {
		serverError(w, "createFramework audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createFramework commit", err)
		return
	}
	s.respondFramework(w, r, id, http.StatusCreated)
}

// updateFramework: PUT /api/compliance/frameworks/{id}. Sostituzione completa.
func (s *Server) updateFramework(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeFrameworkInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateFramework begin", err)
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.compliance_framework
		    SET code = @p1, name = @p2, version = @p3, source_url = @p4, validation_note = @p5,
		        updated_at = SYSUTCDATETIME()
		  WHERE id = @p6`,
		in.Code, in.Name, nullable(in.Version), nullable(in.SourceURL), nullable(in.ValidationNote), id)
	if err != nil {
		writeDBError(w, "updateFramework update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "framework")
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "compliance_framework", id,
		auditDetail("framework_saved", map[string]any{"code": in.Code})); err != nil {
		serverError(w, "updateFramework audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateFramework commit", err)
		return
	}
	s.respondFramework(w, r, id, http.StatusOK)
}

// respondFramework restituisce un framework con i suoi controlli.
func (s *Server) respondFramework(w http.ResponseWriter, r *http.Request, id int, status int) {
	fws, err := s.loadFrameworks(r.Context())
	if err != nil {
		serverError(w, "respondFramework", err)
		return
	}
	for _, f := range fws {
		if f.ID == id {
			writeJSON(w, status, f)
			return
		}
	}
	writeNotFound(w, "framework")
}

// deleteFramework: DELETE /api/compliance/frameworks/{id}. Controlli e valutazioni collegati vengono rimossi.
func (s *Server) deleteFramework(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteFramework begin", err)
		return
	}
	defer tx.Rollback()

	var code string
	err = tx.QueryRowContext(r.Context(),
		`SELECT code FROM dbo.compliance_framework WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "framework")
		return
	}
	if err != nil {
		serverError(w, "deleteFramework read", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.compliance_framework WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteFramework delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "compliance_framework", id,
		auditDetail("framework_deleted", map[string]any{"code": code})); err != nil {
		serverError(w, "deleteFramework audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteFramework commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeControlInput(w http.ResponseWriter, r *http.Request) (ControlInput, bool) {
	var in ControlInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	in.Code = strings.TrimSpace(in.Code)
	in.Title = strings.TrimSpace(in.Title)
	if in.FrameworkID <= 0 || in.Code == "" || runeLen(in.Code) > 40 || in.Title == "" || runeLen(in.Title) > 200 {
		writeValidation(w, "framework_id, code (massimo 40) e title (massimo 200) sono obbligatori")
		return in, false
	}
	if runeLen(in.Description) > 1000 {
		writeValidation(w, "description supera 1000 caratteri")
		return in, false
	}
	return in, true
}

// createControl: POST /api/compliance/controls. Risponde 201.
func (s *Server) createControl(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeControlInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createControl begin", err)
		return
	}
	defer tx.Rollback()

	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.compliance_control (framework_id, code, title, description)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4)`,
		in.FrameworkID, in.Code, in.Title, nullable(in.Description)).Scan(&id)
	if err != nil {
		writeDBError(w, "createControl insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "compliance_control", id,
		auditDetail("control_saved", map[string]any{"code": in.Code})); err != nil {
		serverError(w, "createControl audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createControl commit", err)
		return
	}
	c, err := s.fetchControl(r.Context(), id)
	if err != nil {
		serverError(w, "createControl fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// updateControl: PUT /api/compliance/controls/{id}. Sostituzione completa.
func (s *Server) updateControl(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeControlInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateControl begin", err)
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.compliance_control SET framework_id = @p1, code = @p2, title = @p3, description = @p4
		  WHERE id = @p5`,
		in.FrameworkID, in.Code, in.Title, nullable(in.Description), id)
	if err != nil {
		writeDBError(w, "updateControl update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "controllo")
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "compliance_control", id,
		auditDetail("control_saved", map[string]any{"code": in.Code})); err != nil {
		serverError(w, "updateControl audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateControl commit", err)
		return
	}
	c, err := s.fetchControl(r.Context(), id)
	if err != nil {
		serverError(w, "updateControl fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// deleteControl: DELETE /api/compliance/controls/{id}. Risponde 204.
func (s *Server) deleteControl(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteControl begin", err)
		return
	}
	defer tx.Rollback()

	var code string
	err = tx.QueryRowContext(r.Context(),
		`SELECT code FROM dbo.compliance_control WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "controllo")
		return
	}
	if err != nil {
		serverError(w, "deleteControl read", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.compliance_control WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteControl delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "compliance_control", id,
		auditDetail("control_deleted", map[string]any{"code": code})); err != nil {
		serverError(w, "deleteControl audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteControl commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fetchControl(ctx context.Context, id int) (Control, error) {
	return scanControl(s.db.QueryRowContext(ctx,
		`SELECT id, framework_id, code, title, description FROM dbo.compliance_control WHERE id = @p1`, id))
}

// ---------------------------------------------------------------------------
// Valutazione dei controlli per agente
// ---------------------------------------------------------------------------

// listAssessments: GET /api/compliance/agents/{id}/assessments. Un elemento per controllo; senza valutazione: not_assessed.
func (s *Server) listAssessments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var n int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM dbo.agent WHERE id = @p1`, id).Scan(&n); err != nil {
		serverError(w, "listAssessments agent", err)
		return
	}
	if n == 0 {
		writeNotFound(w, "agente")
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT c.id, f.code, c.code, c.title, ISNULL(s.status, 'not_assessed'), s.evidence_note, s.updated_at
		   FROM dbo.compliance_control c
		   JOIN dbo.compliance_framework f ON f.id = c.framework_id
		   LEFT JOIN dbo.agent_control_assessment s ON s.control_id = c.id AND s.agent_id = @p1
		  ORDER BY f.code, c.code`, id)
	if err != nil {
		serverError(w, "listAssessments", err)
		return
	}
	defer rows.Close()
	out := []Assessment{}
	for rows.Next() {
		a, err := scanAssessment(rows)
		if err != nil {
			serverError(w, "listAssessments scan", err)
			return
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listAssessments rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func scanAssessment(sc rowScanner) (Assessment, error) {
	var a Assessment
	var note sql.NullString
	var at sql.NullTime
	if err := sc.Scan(&a.ControlID, &a.FrameworkCode, &a.ControlCode, &a.Title, &a.Status, &note, &at); err != nil {
		return Assessment{}, err
	}
	a.EvidenceNote = ptrOf(note)
	if at.Valid {
		t := at.Time.UTC().Format("2006-01-02T15:04:05.000Z")
		a.UpdatedAt = &t
	}
	return a, nil
}

// putAssessment: PUT /api/compliance/agents/{id}/assessments/{control_id}. Upsert. Risponde 200.
func (s *Server) putAssessment(w http.ResponseWriter, r *http.Request) {
	agentID, ok := pathID(w, r)
	if !ok {
		return
	}
	controlID, err := parsePathInt(r, "control_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "control_id non valido")
		return
	}
	var in AssessmentInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if !assessmentStatuses[in.Status] {
		writeValidation(w, "status deve essere compliant, non_compliant, not_assessed o not_applicable")
		return
	}
	if runeLen(in.EvidenceNote) > 1000 {
		writeValidation(w, "evidence_note supera 1000 caratteri")
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "putAssessment begin", err)
		return
	}
	defer tx.Rollback()

	var code string
	err = tx.QueryRowContext(r.Context(),
		`SELECT code FROM dbo.compliance_control WHERE id = @p1`, controlID).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "controllo")
		return
	}
	if err != nil {
		serverError(w, "putAssessment control", err)
		return
	}
	if _, err := lockAgent(r.Context(), tx, agentID); err != nil {
		agentLockError(w, "putAssessment agent", err)
		return
	}

	_, err = tx.ExecContext(r.Context(),
		`MERGE dbo.agent_control_assessment AS t
		 USING (SELECT @p1 AS agent_id, @p2 AS control_id) AS s
		    ON t.agent_id = s.agent_id AND t.control_id = s.control_id
		 WHEN MATCHED THEN UPDATE SET status = @p3, evidence_note = @p4, updated_at = SYSUTCDATETIME()
		 WHEN NOT MATCHED THEN INSERT (agent_id, control_id, status, evidence_note)
		      VALUES (@p1, @p2, @p3, @p4);`,
		agentID, controlID, in.Status, nullable(in.EvidenceNote))
	if err != nil {
		writeDBError(w, "putAssessment upsert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "agent_control_assessment", agentID,
		auditDetail("assessment_saved", map[string]any{"control": code, "status": in.Status})); err != nil {
		serverError(w, "putAssessment audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "putAssessment commit", err)
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT c.id, f.code, c.code, c.title, ISNULL(s.status, 'not_assessed'), s.evidence_note, s.updated_at
		   FROM dbo.compliance_control c
		   JOIN dbo.compliance_framework f ON f.id = c.framework_id
		   LEFT JOIN dbo.agent_control_assessment s ON s.control_id = c.id AND s.agent_id = @p1
		  WHERE c.id = @p2`, agentID, controlID)
	if err != nil {
		serverError(w, "putAssessment fetch", err)
		return
	}
	defer rows.Close()
	if !rows.Next() {
		writeNotFound(w, "controllo")
		return
	}
	a, err := scanAssessment(rows)
	if err != nil {
		serverError(w, "putAssessment scan", err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// parsePathInt legge un parametro numerico positivo dal percorso.
func parsePathInt(r *http.Request, name string) (int, error) {
	var v int
	if _, err := fmt.Sscanf(r.PathValue(name), "%d", &v); err != nil || v <= 0 {
		return 0, errors.New("valore non valido")
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Vista d'insieme
// ---------------------------------------------------------------------------

// FrameworkOverview è la sintesi di un framework.
type FrameworkOverview struct {
	FrameworkID      int     `json:"framework_id"`
	Code             string  `json:"code"`
	Name             string  `json:"name"`
	ControlsTotal    int     `json:"controls_total"`
	AgentsTotal      int     `json:"agents_total"`
	AgentsCompliant  int     `json:"agents_compliant"`
	CompliantPercent float64 `json:"compliant_percent"`
}

// ComplianceOverview è la risposta di GET /api/compliance/overview.
// policies_failing conta le coppie (agente, politica attiva) in fallimento: le politiche non sono legate ai framework.
type ComplianceOverview struct {
	Frameworks      []FrameworkOverview `json:"frameworks"`
	PoliciesFailing int                 `json:"policies_failing"`
}

// complianceOverview: GET /api/compliance/overview
func (s *Server) complianceOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := ComplianceOverview{Frameworks: []FrameworkOverview{}}

	var agents int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dbo.agent`).Scan(&agents); err != nil {
		serverError(w, "overview agents", err)
		return
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT f.id, f.code, f.name, COUNT(c.id)
		   FROM dbo.compliance_framework f
		   LEFT JOIN dbo.compliance_control c ON c.framework_id = f.id
		  GROUP BY f.id, f.code, f.name ORDER BY f.code`)
	if err != nil {
		serverError(w, "overview frameworks", err)
		return
	}
	var fos []FrameworkOverview
	for rows.Next() {
		var fo FrameworkOverview
		if err := rows.Scan(&fo.FrameworkID, &fo.Code, &fo.Name, &fo.ControlsTotal); err != nil {
			rows.Close()
			serverError(w, "overview scan", err)
			return
		}
		fo.AgentsTotal = agents
		fos = append(fos, fo)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		serverError(w, "overview rows", err)
		return
	}

	for i := range fos {
		fo := &fos[i]
		if fo.ControlsTotal == 0 || agents == 0 {
			continue
		}
		// Un agente è conforme al framework se ogni suo controllo è compliant o not_applicable.
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM dbo.agent a
			  WHERE NOT EXISTS (
			     SELECT 1 FROM dbo.compliance_control c
			      WHERE c.framework_id = @p1
			        AND NOT EXISTS (
			           SELECT 1 FROM dbo.agent_control_assessment s
			            WHERE s.agent_id = a.id AND s.control_id = c.id
			              AND s.status IN ('compliant', 'not_applicable')))`, fo.FrameworkID).
			Scan(&fo.AgentsCompliant)
		if err != nil {
			serverError(w, "overview compliant", err)
			return
		}
		fo.CompliantPercent = float64(fo.AgentsCompliant) * 100 / float64(agents)
	}
	out.Frameworks = append(out.Frameworks, fos...)

	// Politiche in fallimento: coppie (agente, politica attiva) non superate.
	var policies []Policy
	prows, err := s.db.QueryContext(ctx, `SELECT `+policyColumns+` FROM dbo.compliance_policy p WHERE p.enabled = 1`)
	if err != nil {
		serverError(w, "overview policies", err)
		return
	}
	for prows.Next() {
		p, err := scanPolicy(prows)
		if err != nil {
			prows.Close()
			serverError(w, "overview policy scan", err)
			return
		}
		policies = append(policies, p)
	}
	prows.Close()

	agentIDs := []int{}
	arows, err := s.db.QueryContext(ctx, `SELECT id FROM dbo.agent`)
	if err != nil {
		serverError(w, "overview agent ids", err)
		return
	}
	for arows.Next() {
		var id int
		if err := arows.Scan(&id); err != nil {
			arows.Close()
			serverError(w, "overview agent id scan", err)
			return
		}
		agentIDs = append(agentIDs, id)
	}
	arows.Close()
	for _, id := range agentIDs {
		facts, err := s.loadAgentFacts(ctx, id)
		if err != nil {
			serverError(w, "overview facts", err)
			return
		}
		for _, p := range policies {
			if pass, _ := evaluatePolicy(p, facts); !pass {
				out.PoliciesFailing++
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
