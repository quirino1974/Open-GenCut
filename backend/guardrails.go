// Open-GenCut - Punto 5 della specifica: guardrail.
//
// Politiche applicate a runtime alla comunicazione di agenti e sistemi agentici:
//   - CRUD delle guardrail (tipo, parametri, ambito global | agent | resource);
//   - valutazione del testo in ingresso o in uscita (allow | mask | block);
//   - registro delle valutazioni SENZA testo: solo lunghezza e hash SHA-256;
//   - riepilogo delle ultime 24 ore.
//
// Limite dichiarato: il rilevamento è basato su regole ed espressioni regolari,
// non su modelli di intelligenza artificiale.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	guardTypes = map[string]bool{"pii_mask": true, "pii_block": true, "keyword_block": true,
		"max_length": true, "rate_limit_per_minute": true, "prompt_injection": true}
	guardScopes  = map[string]bool{"global": true, "agent": true, "resource": true}
	piiKinds     = []string{"iban", "card", "cf", "email", "phone"} // ordine di applicazione
	piiLabel     = map[string]string{"iban": "IBAN", "card": "CARD", "cf": "CF", "email": "EMAIL", "phone": "PHONE"}
	guardTextMax = 100000 // caratteri massimi del testo da valutare
)

// Espressioni per il rilevamento. Le validazioni (Luhn, mod 97) riducono i falsi positivi.
var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	rePhone = regexp.MustCompile(`(?:\+\d{1,3}[ .\-]?)?\b3\d{2}[ .\-]?\d{6,7}\b|\+\d{1,3}[ .\-]?\d{6,12}\b`)
	reCF    = regexp.MustCompile(`\b[A-Z]{6}\d{2}[A-Z]\d{2}[A-Z]\d{3}[A-Z]\b`)
	reIBAN  = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,4})?\b`)
	reCard  = regexp.MustCompile(`\b(?:\d[ \-]?){12,18}\d\b`)

	reInjection = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(ignora|ignorare|dimentica|dimenticare)\b.{0,30}\b(istruzioni|regole|prompt)\b`),
		regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\b.{0,30}\b(instructions?|rules|prompts?)\b`),
		regexp.MustCompile(`(?i)\b(rivela|mostra|stampa)\b.{0,30}\b(prompt di sistema|istruzioni di sistema)\b`),
		regexp.MustCompile(`(?i)\b(reveal|show|print)\b.{0,30}\b(system prompt|system instructions)\b`),
		regexp.MustCompile(`(?i)\b(you are now|from now on you (are|will be)|act as (dan|an? unrestricted))\b`),
	}
)

// ---------------------------------------------------------------------------
// Modelli
// ---------------------------------------------------------------------------

// Guardrail è una regola applicata a runtime.
type Guardrail struct {
	ID         int             `json:"id"`
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Params     json.RawMessage `json:"params"`
	Scope      string          `json:"scope"`
	AgentID    *int            `json:"agent_id"`
	ResourceID *int            `json:"resource_id"`
	Enabled    bool            `json:"enabled"`
}

// GuardrailInput è il corpo di POST e PUT delle guardrail.
type GuardrailInput struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Params     json.RawMessage `json:"params"`
	Scope      string          `json:"scope"`
	AgentID    *int            `json:"agent_id"`
	ResourceID *int            `json:"resource_id"`
	Enabled    *bool           `json:"enabled"`
}

// Violation è una regola che ha scattato su un testo.
type Violation struct {
	GuardrailID int            `json:"guardrail_id"`
	Type        string         `json:"type"`
	Detail      string         `json:"detail"`
	Params      map[string]any `json:"params"`
}

// EvaluateInput è il corpo di POST /api/guardrails/evaluate.
type EvaluateInput struct {
	AgentID    int    `json:"agent_id"`
	ResourceID *int   `json:"resource_id"`
	Direction  string `json:"direction"`
	Text       string `json:"text"`
}

// EvaluateResponse è la risposta di POST /api/guardrails/evaluate.
// Con decision "block" il campo text è vuoto: il testo bloccato non viene restituito.
type EvaluateResponse struct {
	Decision   string      `json:"decision"`
	Text       string      `json:"text"`
	Violations []Violation `json:"violations"`
}

// GuardEvent è una riga del registro delle valutazioni (senza testo).
type GuardEvent struct {
	ID         int64           `json:"id"`
	At         time.Time       `json:"at"`
	AgentID    int             `json:"agent_id"`
	ResourceID *int            `json:"resource_id"`
	Direction  string          `json:"direction"`
	Decision   string          `json:"decision"`
	Violations json.RawMessage `json:"violations"`
	TextLength int             `json:"text_length"`
	TextSHA256 string          `json:"text_sha256"`
}

// ---------------------------------------------------------------------------
// Rotte
// ---------------------------------------------------------------------------

// registerGuardrails aggiunge le rotte dei guardrail al mux principale.
func (s *Server) registerGuardrails(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/guardrails", s.listGuardrails)
	mux.HandleFunc("POST /api/guardrails", s.createGuardrail)
	mux.HandleFunc("GET /api/guardrails/{id}", s.getGuardrail)
	mux.HandleFunc("PUT /api/guardrails/{id}", s.updateGuardrail)
	mux.HandleFunc("DELETE /api/guardrails/{id}", s.deleteGuardrail)
	mux.HandleFunc("POST /api/guardrails/evaluate", s.evaluateGuardrails)
	mux.HandleFunc("GET /api/guardrails/events", s.listGuardEvents)
	mux.HandleFunc("GET /api/guardrails/summary", s.guardrailSummary)
}

// ---------------------------------------------------------------------------
// CRUD
// ---------------------------------------------------------------------------

const guardrailColumns = `g.id, g.name, g.type, g.params, g.scope, g.agent_id, g.resource_id, g.enabled`

func scanGuardrail(sc rowScanner) (Guardrail, error) {
	var g Guardrail
	var agentID, resourceID sql.NullInt64
	var params string
	if err := sc.Scan(&g.ID, &g.Name, &g.Type, &params, &g.Scope, &agentID, &resourceID, &g.Enabled); err != nil {
		return Guardrail{}, err
	}
	g.Params = json.RawMessage(params)
	if agentID.Valid {
		v := int(agentID.Int64)
		g.AgentID = &v
	}
	if resourceID.Valid {
		v := int(resourceID.Int64)
		g.ResourceID = &v
	}
	return g, nil
}

func (s *Server) fetchGuardrail(ctx context.Context, id int) (Guardrail, error) {
	return scanGuardrail(s.db.QueryRowContext(ctx,
		`SELECT `+guardrailColumns+` FROM dbo.guardrail g WHERE g.id = @p1`, id))
}

// listGuardrails: GET /api/guardrails
func (s *Server) listGuardrails(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+guardrailColumns+` FROM dbo.guardrail g ORDER BY g.id`)
	if err != nil {
		serverError(w, "listGuardrails", err)
		return
	}
	defer rows.Close()
	out := []Guardrail{}
	for rows.Next() {
		g, err := scanGuardrail(rows)
		if err != nil {
			serverError(w, "listGuardrails scan", err)
			return
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listGuardrails rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getGuardrail: GET /api/guardrails/{id}
func (s *Server) getGuardrail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	g, err := s.fetchGuardrail(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "guardrail")
		return
	}
	if err != nil {
		serverError(w, "getGuardrail", err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// decodeGuardrailInput legge e valida una guardrail, parametri compresi.
func decodeGuardrailInput(w http.ResponseWriter, r *http.Request) (GuardrailInput, bool) {
	var in GuardrailInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || runeLen(in.Name) > 150 {
		writeValidation(w, "name è obbligatorio (massimo 150 caratteri)")
		return in, false
	}
	if !guardTypes[in.Type] {
		writeValidation(w, "type non valido")
		return in, false
	}
	if !guardScopes[in.Scope] {
		writeValidation(w, "scope deve essere global, agent o resource")
		return in, false
	}
	switch in.Scope {
	case "global":
		if in.AgentID != nil || in.ResourceID != nil {
			writeValidation(w, "con scope global agent_id e resource_id devono essere vuoti")
			return in, false
		}
	case "agent":
		if in.AgentID == nil || *in.AgentID <= 0 || in.ResourceID != nil {
			writeValidation(w, "con scope agent serve agent_id e resource_id deve essere vuoto")
			return in, false
		}
	case "resource":
		if in.ResourceID == nil || *in.ResourceID <= 0 || in.AgentID != nil {
			writeValidation(w, "con scope resource serve resource_id e agent_id deve essere vuoto")
			return in, false
		}
	}
	params, err := normalizeObject(in.Params)
	if err != nil {
		writeValidation(w, "params deve essere un oggetto JSON")
		return in, false
	}
	in.Params = params
	if msg := validateGuardParams(in.Type, params); msg != "" {
		writeValidation(w, msg)
		return in, false
	}
	return in, true
}

// validateGuardParams controlla i parametri richiesti dal tipo di guardrail. Restituisce il messaggio d'errore o "".
func validateGuardParams(typ string, raw json.RawMessage) string {
	var p map[string]json.RawMessage
	_ = json.Unmarshal(raw, &p)
	switch typ {
	case "keyword_block":
		var terms []string
		if err := json.Unmarshal(p["terms"], &terms); err != nil || len(terms) == 0 || len(terms) > 100 {
			return `keyword_block richiede params {"terms": [...]} con da 1 a 100 termini`
		}
		for _, t := range terms {
			if strings.TrimSpace(t) == "" || runeLen(t) > 100 {
				return "ogni termine deve essere non vuoto e di massimo 100 caratteri"
			}
		}
	case "max_length":
		var n int
		if err := json.Unmarshal(p["max_chars"], &n); err != nil || n < 1 || n > 1000000 {
			return `max_length richiede params {"max_chars": n} con n tra 1 e 1000000`
		}
	case "rate_limit_per_minute":
		var n int
		if err := json.Unmarshal(p["limit"], &n); err != nil || n < 1 || n > 100000 {
			return `rate_limit_per_minute richiede params {"limit": n} con n tra 1 e 100000`
		}
	case "pii_mask", "pii_block":
		if raw, ok := p["types"]; ok {
			var types []string
			if err := json.Unmarshal(raw, &types); err != nil {
				return `types deve essere una lista di: email, phone, cf, iban, card`
			}
			for _, t := range types {
				if !contains(piiKinds, t) {
					return "tipo di dato personale non riconosciuto: " + t
				}
			}
		}
	}
	return ""
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// createGuardrail: POST /api/guardrails. Risponde 201.
func (s *Server) createGuardrail(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeGuardrailInput(w, r)
	if !ok {
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createGuardrail begin", err)
		return
	}
	defer tx.Rollback()

	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.guardrail (name, type, params, scope, agent_id, resource_id, enabled)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7)`,
		in.Name, in.Type, string(in.Params), in.Scope, agentParam(in.AgentID), agentParam(in.ResourceID), enabled).Scan(&id)
	if err != nil {
		writeDBError(w, "createGuardrail insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "guardrail", id,
		auditDetail("guardrail_saved", map[string]any{"name": in.Name, "type": in.Type})); err != nil {
		serverError(w, "createGuardrail audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createGuardrail commit", err)
		return
	}
	g, err := s.fetchGuardrail(r.Context(), id)
	if err != nil {
		serverError(w, "createGuardrail fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

// updateGuardrail: PUT /api/guardrails/{id}. Sostituzione completa.
func (s *Server) updateGuardrail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeGuardrailInput(w, r)
	if !ok {
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateGuardrail begin", err)
		return
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.guardrail
		    SET name = @p1, type = @p2, params = @p3, scope = @p4, agent_id = @p5, resource_id = @p6,
		        enabled = @p7, updated_at = SYSUTCDATETIME()
		  WHERE id = @p8`,
		in.Name, in.Type, string(in.Params), in.Scope, agentParam(in.AgentID), agentParam(in.ResourceID), enabled, id)
	if err != nil {
		writeDBError(w, "updateGuardrail update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "guardrail")
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "guardrail", id,
		auditDetail("guardrail_saved", map[string]any{"name": in.Name, "type": in.Type})); err != nil {
		serverError(w, "updateGuardrail audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateGuardrail commit", err)
		return
	}
	g, err := s.fetchGuardrail(r.Context(), id)
	if err != nil {
		serverError(w, "updateGuardrail fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// deleteGuardrail: DELETE /api/guardrails/{id}. Risponde 204.
func (s *Server) deleteGuardrail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteGuardrail begin", err)
		return
	}
	defer tx.Rollback()

	var name string
	err = tx.QueryRowContext(r.Context(),
		`SELECT name FROM dbo.guardrail WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "guardrail")
		return
	}
	if err != nil {
		serverError(w, "deleteGuardrail read", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.guardrail WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteGuardrail delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "guardrail", id,
		auditDetail("guardrail_deleted", map[string]any{"name": name})); err != nil {
		serverError(w, "deleteGuardrail audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteGuardrail commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Rilevamento
// ---------------------------------------------------------------------------

// luhnValid verifica il numero di carta con l'algoritmo di Luhn.
func luhnValid(digits string) bool {
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// ibanValid verifica l'IBAN con il controllo mod 97 (ISO 13616).
func ibanValid(iban string) bool {
	s := strings.ReplaceAll(iban, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rearranged := s[4:] + s[:4]
	var b strings.Builder
	for _, c := range rearranged {
		switch {
		case c >= '0' && c <= '9':
			b.WriteRune(c)
		case c >= 'A' && c <= 'Z':
			b.WriteString(strconv.Itoa(int(c-'A') + 10))
		default:
			return false
		}
	}
	n := new(big.Int)
	if _, ok := n.SetString(b.String(), 10); !ok {
		return false
	}
	return new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

// scanPII cerca (e, se replace è vero, sostituisce) i dati personali dei tipi indicati.
// Restituisce il testo risultante e il conteggio per tipo.
// I segnaposto hanno la forma [PII:TIPO] e non contengono cifre, quindi non vengono rivalutati dai rilevatori successivi.
func scanPII(text string, kinds []string, replace bool) (string, map[string]int) {
	counts := map[string]int{}
	cur := text
	for _, kind := range piiKinds {
		if len(kinds) > 0 && !contains(kinds, kind) {
			continue
		}
		var re *regexp.Regexp
		switch kind {
		case "iban":
			re = reIBAN
		case "card":
			re = reCard
		case "cf":
			re = reCF
		case "email":
			re = reEmail
		case "phone":
			re = rePhone
		}
		locs := re.FindAllStringIndex(cur, -1)
		if len(locs) == 0 {
			continue
		}
		var b strings.Builder
		last := 0
		n := 0
		for _, loc := range locs {
			match := cur[loc[0]:loc[1]]
			valid := true
			switch kind {
			case "iban":
				valid = ibanValid(match)
			case "card":
				digits := strings.Map(func(r rune) rune {
					if r >= '0' && r <= '9' {
						return r
					}
					return -1
				}, match)
				valid = luhnValid(digits)
			}
			if !valid {
				continue
			}
			n++
			b.WriteString(cur[last:loc[0]])
			if replace {
				b.WriteString("[PII:" + piiLabel[kind] + "]")
			} else {
				b.WriteString(match)
			}
			last = loc[1]
		}
		b.WriteString(cur[last:])
		if n > 0 {
			counts[kind] = n
			if replace {
				cur = b.String()
			}
		}
	}
	return cur, counts
}

// injectionFound indica se il testo contiene un tentativo tipico di sovrascrittura delle istruzioni.
func injectionFound(text string) bool {
	for _, re := range reInjection {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// paramList legge un campo lista di stringhe dai parametri (es. "types").
func paramStrings(raw json.RawMessage, key string) []string {
	var p map[string]json.RawMessage
	_ = json.Unmarshal(raw, &p)
	var out []string
	_ = json.Unmarshal(p[key], &out)
	return out
}

// ---------------------------------------------------------------------------
// Valutazione
// ---------------------------------------------------------------------------

// evaluateGuardrails: POST /api/guardrails/evaluate
// Applica le guardrail attive nell'ambito, in ordine di id. Il block prevale sul mask.
// Ogni valutazione produce una riga nel registro, senza testo. La valutazione è serializzata per agente
// (blocco applicativo), così il limite di frequenza è esatto anche con richieste concorrenti.
func (s *Server) evaluateGuardrails(w http.ResponseWriter, r *http.Request) {
	var in EvaluateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.AgentID <= 0 {
		writeValidation(w, "agent_id è obbligatorio")
		return
	}
	if in.Direction != "input" && in.Direction != "output" {
		writeValidation(w, "direction deve essere input o output")
		return
	}
	if runeLen(in.Text) > guardTextMax {
		writeValidation(w, "text supera il limite di caratteri")
		return
	}
	if in.ResourceID != nil && *in.ResourceID <= 0 {
		writeValidation(w, "resource_id non valido")
		return
	}
	res, serr := s.guardEval(r.Context(), in)
	if serr != nil {
		writeSvcErr(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// guardEval applica le guardrail attive a un testo e registra la valutazione (senza testo).
// È riusabile da /api/guardrails/evaluate e da /api/chat/complete.
func (s *Server) guardEval(ctx context.Context, in EvaluateInput) (EvaluateResponse, *svcErr) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EvaluateResponse{}, internalErr("evaluate begin", err)
	}
	defer tx.Rollback()

	// Blocco applicativo per agente: serializza le valutazioni dello stesso agente.
	if _, err := tx.ExecContext(ctx,
		`EXEC sp_getapplock @Resource = @p1, @LockMode = 'Exclusive', @LockOwner = 'Transaction', @LockTimeout = 30000`,
		"opengencut_guard_"+strconv.Itoa(in.AgentID)); err != nil {
		return EvaluateResponse{}, internalErr("evaluate lock", err)
	}

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dbo.agent WHERE id = @p1`, in.AgentID).Scan(&n); err != nil {
		return EvaluateResponse{}, internalErr("evaluate agent", err)
	}
	if n == 0 {
		return EvaluateResponse{}, notFoundErr("agente")
	}
	if in.ResourceID != nil {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dbo.integration_resource WHERE id = @p1`, *in.ResourceID).Scan(&n); err != nil {
			return EvaluateResponse{}, internalErr("evaluate resource", err)
		}
		if n == 0 {
			return EvaluateResponse{}, notFoundErr("risorsa")
		}
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT `+guardrailColumns+` FROM dbo.guardrail g
		  WHERE g.enabled = 1
		    AND (g.scope = 'global'
		      OR (g.scope = 'agent' AND g.agent_id = @p1)
		      OR (g.scope = 'resource' AND g.resource_id = @p2))
		  ORDER BY g.id`, in.AgentID, nullInt(in.ResourceID))
	if err != nil {
		return EvaluateResponse{}, internalErr("evaluate guardrails", err)
	}
	var active []Guardrail
	for rows.Next() {
		g, err := scanGuardrail(rows)
		if err != nil {
			rows.Close()
			return EvaluateResponse{}, internalErr("evaluate scan", err)
		}
		active = append(active, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return EvaluateResponse{}, internalErr("evaluate rows", err)
	}

	original := in.Text
	cur := in.Text
	var violations []Violation
	decision := "allow"
	blocked := false

	for _, g := range active {
		switch g.Type {
		case "pii_mask":
			masked, counts := scanPII(cur, paramStrings(g.Params, "types"), true)
			if len(counts) > 0 {
				cur = masked
				if !blocked {
					decision = "mask"
				}
				violations = append(violations, Violation{GuardrailID: g.ID, Type: g.Type, Detail: countsText(counts),
					Params: piiParams(counts)})
			}
		case "pii_block":
			// Le regole di blocco leggono il testo ricevuto: un mascheramento non deve nascondere un blocco.
			_, counts := scanPII(original, paramStrings(g.Params, "types"), false)
			if len(counts) > 0 {
				blocked = true
				violations = append(violations, Violation{GuardrailID: g.ID, Type: g.Type, Detail: countsText(counts),
					Params: map[string]any{"kinds": piiKindsOf(counts)}})
			}
		case "keyword_block":
			lower := strings.ToLower(original)
			matched := 0
			for _, term := range paramStrings(g.Params, "terms") {
				if strings.Contains(lower, strings.ToLower(term)) {
					matched++
				}
			}
			if matched > 0 {
				blocked = true
				violations = append(violations, Violation{GuardrailID: g.ID, Type: g.Type,
					Detail: "termine vietato rilevato", Params: map[string]any{"count": matched}})
			}
		case "max_length":
			var p struct {
				Max int `json:"max_chars"`
			}
			_ = json.Unmarshal(g.Params, &p)
			if p.Max > 0 && runeLen(original) > p.Max {
				blocked = true
				violations = append(violations, Violation{GuardrailID: g.ID, Type: g.Type,
					Detail: fmt.Sprintf("testo di %d caratteri, massimo %d", runeLen(original), p.Max),
					Params: map[string]any{"length": runeLen(original), "max": p.Max}})
			}
		case "rate_limit_per_minute":
			var p struct {
				Limit int `json:"limit"`
			}
			_ = json.Unmarshal(g.Params, &p)
			var recent int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM dbo.guardrail_event WHERE agent_id = @p1 AND at >= DATEADD(MINUTE, -1, SYSUTCDATETIME())`,
				in.AgentID).Scan(&recent); err != nil {
				return EvaluateResponse{}, internalErr("evaluate rate", err)
			}
			if p.Limit > 0 && recent >= p.Limit {
				blocked = true
				violations = append(violations, Violation{GuardrailID: g.ID, Type: g.Type,
					Detail: fmt.Sprintf("%d valutazioni nell'ultimo minuto, limite %d", recent, p.Limit),
					Params: map[string]any{"count": recent, "limit": p.Limit}})
			}
		case "prompt_injection":
			if injectionFound(original) {
				blocked = true
				violations = append(violations, Violation{GuardrailID: g.ID, Type: g.Type,
					Detail: "tentativo di sovrascrittura delle istruzioni", Params: map[string]any{}})
			}
		}
	}

	if blocked {
		decision = "block"
	}
	if violations == nil {
		violations = []Violation{}
	}
	responseText := cur
	if decision == "block" {
		responseText = ""
	}

	violationsJSON, _ := json.Marshal(violations)
	sum := sha256.Sum256([]byte(original))
	var resID sql.NullInt64
	if in.ResourceID != nil {
		resID = sql.NullInt64{Int64: int64(*in.ResourceID), Valid: true}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dbo.guardrail_event (agent_id, resource_id, direction, decision, violations, text_length, text_sha256)
		 VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7)`,
		in.AgentID, resID, in.Direction, decision, string(violationsJSON), runeLen(original),
		hex.EncodeToString(sum[:])); err != nil {
		return EvaluateResponse{}, internalErr("evaluate event", err)
	}
	if err := tx.Commit(); err != nil {
		return EvaluateResponse{}, internalErr("evaluate commit", err)
	}
	return EvaluateResponse{Decision: decision, Text: responseText, Violations: violations}, nil
}

// countsText descrive i conteggi per tipo di dato personale, es. "email=2, card=1" (ordine stabile).
func countsText(counts map[string]int) string {
	var parts []string
	for _, k := range piiKinds {
		if n, ok := counts[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", k, n))
		}
	}
	return strings.Join(parts, ", ")
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// ---------------------------------------------------------------------------
// Registro e riepilogo
// ---------------------------------------------------------------------------

// listGuardEvents: GET /api/guardrails/events?agent_id=&decision=&limit=50 (massimo 500). Senza testo.
func (s *Server) listGuardEvents(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	agentID, ok := optionalID(w, qs.Get("agent_id"), "agent_id")
	if !ok {
		return
	}
	decision := qs.Get("decision")
	if decision != "" && decision != "allow" && decision != "mask" && decision != "block" {
		writeValidation(w, "decision deve essere allow, mask o block")
		return
	}
	limit := 50
	if v := qs.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeValidation(w, "limit deve essere tra 1 e 500")
			return
		}
		limit = n
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT TOP (@p3) id, at, agent_id, resource_id, direction, decision, violations, text_length, text_sha256
		   FROM dbo.guardrail_event
		  WHERE (@p1 = 0 OR agent_id = @p1) AND (@p2 = N'' OR decision = @p2)
		  ORDER BY id DESC`, agentID, decision, limit)
	if err != nil {
		serverError(w, "listGuardEvents", err)
		return
	}
	defer rows.Close()
	out := []GuardEvent{}
	for rows.Next() {
		var e GuardEvent
		var res sql.NullInt64
		var viol string
		if err := rows.Scan(&e.ID, &e.At, &e.AgentID, &res, &e.Direction, &e.Decision, &viol, &e.TextLength, &e.TextSHA256); err != nil {
			serverError(w, "listGuardEvents scan", err)
			return
		}
		if res.Valid {
			v := int(res.Int64)
			e.ResourceID = &v
		}
		e.Violations = json.RawMessage(viol)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listGuardEvents rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GuardSummary è il riepilogo delle ultime 24 ore.
type GuardSummary struct {
	WindowHours int            `json:"window_hours"`
	Total       int            `json:"total"`
	ByDecision  map[string]int `json:"by_decision"`
	ByType      map[string]int `json:"by_type"`
}

// guardrailSummary: GET /api/guardrails/summary. Conteggi per esito e per tipo di violazione nelle ultime 24 ore.
func (s *Server) guardrailSummary(w http.ResponseWriter, r *http.Request) {
	sum := GuardSummary{WindowHours: 24,
		ByDecision: map[string]int{"allow": 0, "mask": 0, "block": 0},
		ByType:     map[string]int{}}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT decision, violations FROM dbo.guardrail_event
		  WHERE at >= DATEADD(HOUR, -24, SYSUTCDATETIME())`)
	if err != nil {
		serverError(w, "guardrailSummary", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var decision, viol string
		if err := rows.Scan(&decision, &viol); err != nil {
			serverError(w, "guardrailSummary scan", err)
			return
		}
		sum.Total++
		sum.ByDecision[decision]++
		var vs []Violation
		_ = json.Unmarshal([]byte(viol), &vs)
		for _, v := range vs {
			sum.ByType[v.Type]++
		}
	}
	if err := rows.Err(); err != nil {
		serverError(w, "guardrailSummary rows", err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// piiParams descrive i dati personali trovati con chiavi stabili: count totale e kinds in ordine fisso.
func piiParams(counts map[string]int) map[string]any {
	total := 0
	kinds := []string{}
	for _, k := range piiKinds {
		if n, ok := counts[k]; ok {
			total += n
			kinds = append(kinds, k)
		}
	}
	return map[string]any{"count": total, "kinds": kinds}
}

// piiKindsOf restituisce i tipi di dato personale trovati, in ordine fisso.
func piiKindsOf(counts map[string]int) []string {
	kinds := []string{}
	for _, k := range piiKinds {
		if _, ok := counts[k]; ok {
			kinds = append(kinds, k)
		}
	}
	return kinds
}
