// Open-GenCut - Punto di accesso unificato: POST /api/gateway/call (docs/MCP.txt).
//
// Per mcp_server: controllo, poi tools/call JSON-RPC verso il server MCP.
// Per api: controllo, poi la richiesta (GET, POST, PUT, DELETE) verso l'endpoint della risorsa.
//
// Ordine dei controlli: risorsa attiva, agente (revocato, dismesso, attivo), allowlist della risorsa,
// voce dello strumento, approvazione (approval_required con approval_id), limiti, guardrail in ingresso
// sugli argomenti, chiamata, guardrail in uscita sul risultato.
//
// Le approvazioni valide vengono prenotate in transazione e rilasciate se la chiamata non avviene
// o non riesce. Argomenti e risultato non vengono salvati: solo lunghezza e SHA-256.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	gatewayTimeout = 60 * time.Second
	gatewayMaxResp = 2 << 20
	gatewayMaxArgs = 100000
)

var gatewayMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true}

// GatewayInput è il corpo di POST /api/gateway/call.
type GatewayInput struct {
	AgentID    int             `json:"agent_id"`
	ResourceID int             `json:"resource_id"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	Method     string          `json:"method"`
	Payload    string          `json:"payload"`
	// Campi per le risorse llm_model: stessi di /api/chat/complete.
	Provider         string        `json:"provider"`
	APIKey           string        `json:"api_key"`
	Model            string        `json:"model"`
	System           string        `json:"system"`
	Messages         []ChatMessage `json:"messages"`
	Environment      string        `json:"environment"`
	PriceInputPer1k  *float64      `json:"price_input_per_1k"`
	PriceOutputPer1k *float64      `json:"price_output_per_1k"`
}

// GatewayResponse è la risposta di POST /api/gateway/call.
type GatewayResponse struct {
	Decision   string          `json:"decision"` // allowed | denied | blocked
	Reason     string          `json:"reason"`
	Result     json.RawMessage `json:"result,omitempty"`
	ApprovalID *int            `json:"approval_id"`
	LatencyMS  int             `json:"latency_ms"`
}

// registerGateway aggiunge la rotta del gateway al mux principale.
func (s *Server) registerGateway(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/gateway/call", s.gatewayCall)
}

// mcpRPC invia una richiesta JSON-RPC 2.0 al server MCP e restituisce il campo result.
// Gli errori non contengono dettagli interni del server.
func mcpRPC(ctx context.Context, endpoint, method string, params any) (json.RawMessage, error) {
	if err := checkEndpoint(endpoint); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("richiesta non valida")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: gatewayTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("il server MCP non risponde")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, gatewayMaxResp))
	if err != nil {
		return nil, errors.New("risposta del server MCP non leggibile")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("il server MCP ha risposto con stato %d", resp.StatusCode)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, errors.New("risposta del server MCP non valida")
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, errors.New("il server MCP ha restituito un errore")
	}
	if len(env.Result) == 0 {
		return nil, errors.New("risposta del server MCP senza risultato")
	}
	return env.Result, nil
}

// checkEndpoint accetta solo URL http o https con host.
func checkEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("endpoint della risorsa non valido o assente")
	}
	return nil
}

// apiCall invia la richiesta all'endpoint di una risorsa api e restituisce il corpo della risposta.
func apiCall(ctx context.Context, endpoint, method, payload string) ([]byte, error) {
	if err := checkEndpoint(endpoint); err != nil {
		return nil, err
	}
	var body io.Reader
	if method != http.MethodGet && payload != "" {
		body = strings.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("richiesta non valida")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: gatewayTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("l'endpoint non risponde")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, gatewayMaxResp))
	if err != nil {
		return nil, errors.New("risposta dell'endpoint non leggibile")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("l'endpoint ha risposto con stato %d", resp.StatusCode)
	}
	return data, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// gwPre è l'esito dei controlli prima della chiamata.
type gwPre struct {
	decision   string
	reason     string
	approvalID *int
	toolID     int
	toolName   string
	reserved   []int64 // approvazioni prenotate (usate) da rilasciare se la chiamata non avviene
}

// gatewayChecks esegue i controlli fino ai limiti. Le approvazioni valide vengono prenotate.
// Se manca un'approvazione, viene aperta una richiesta pending e si nega con approval_required.
// Va chiamata dentro la transazione, che il chiamante commette o annulla.
func (s *Server) gatewayChecks(ctx context.Context, tx *sql.Tx, agentID, resourceID int, mcp bool, toolName string) (gwPre, error) {
	var pre gwPre
	var resStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM dbo.integration_resource WHERE id = @p1`, resourceID).Scan(&resStatus); err != nil {
		return pre, err
	}
	if resStatus != "enabled" {
		return gwPre{decision: "denied", reason: "resource_disabled"}, nil
	}
	var agentStatus string
	var revoked bool
	if err := tx.QueryRowContext(ctx,
		`SELECT status, CASE WHEN identity_revoked_at IS NULL THEN 0 ELSE 1 END FROM dbo.agent WHERE id = @p1`,
		agentID).Scan(&agentStatus, &revoked); err != nil {
		return pre, err
	}
	switch {
	case revoked:
		return gwPre{decision: "denied", reason: "agent_revoked"}, nil
	case agentStatus == "retired":
		return gwPre{decision: "denied", reason: "agent_retired"}, nil
	case agentStatus != "active":
		return gwPre{decision: "denied", reason: "agent_not_active"}, nil
	}
	var pairRA bool
	err := tx.QueryRowContext(ctx,
		`SELECT requires_approval FROM dbo.agent_access WHERE agent_id = @p1 AND resource_id = @p2`,
		agentID, resourceID).Scan(&pairRA)
	if errors.Is(err, sql.ErrNoRows) {
		return gwPre{decision: "denied", reason: "not_allowed"}, nil
	}
	if err != nil {
		return pre, err
	}
	// Strumento: deve esistere sul server e avere una voce per l'agente.
	needsApproval := map[int]bool{} // tool_id (0 = livello risorsa) -> richiesta
	if pairRA {
		needsApproval[0] = true
	}
	if mcp {
		var toolID int
		var toolRA, accessRA bool
		err := tx.QueryRowContext(ctx,
			`SELECT t.id, t.requires_approval, ta.requires_approval
			   FROM dbo.mcp_tool t
			   JOIN dbo.tool_access ta ON ta.tool_id = t.id AND ta.agent_id = @p1
			  WHERE t.resource_id = @p2 AND t.name = @p3`, agentID, resourceID, toolName).
			Scan(&toolID, &toolRA, &accessRA)
		if errors.Is(err, sql.ErrNoRows) {
			return gwPre{decision: "denied", reason: "tool_not_allowed"}, nil
		}
		if err != nil {
			return pre, err
		}
		pre.toolID = toolID
		pre.toolName = toolName
		if toolRA || accessRA {
			needsApproval[toolID] = true
		}
	}
	// Approvazioni. Passata 1: se una manca, si apre (o riusa) la richiesta pending e si nega.
	// Nessuna approvazione viene prenotata finché non sono tutte presenti.
	tids := make([]int, 0, len(needsApproval))
	for tid := range needsApproval {
		tids = append(tids, tid)
	}
	for _, tid := range tids {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM dbo.integration_approval WITH (UPDLOCK, ROWLOCK)
			  WHERE agent_id = @p1 AND resource_id = @p2 AND ISNULL(tool_id, 0) = @p3 AND status = 'approved'`,
			agentID, resourceID, tid).Scan(&n); err != nil {
			return pre, err
		}
		if n > 0 {
			continue
		}
		var pid int
		err := tx.QueryRowContext(ctx,
			`SELECT TOP (1) id FROM dbo.integration_approval WITH (UPDLOCK, ROWLOCK)
			  WHERE agent_id = @p1 AND resource_id = @p2 AND ISNULL(tool_id, 0) = @p3 AND status = 'pending'
			  ORDER BY id`, agentID, resourceID, tid).Scan(&pid)
		if errors.Is(err, sql.ErrNoRows) {
			pid, err = insertPendingApproval(ctx, tx, agentID, resourceID, tid, "richiesta automatica dal gateway")
		}
		if err != nil {
			return pre, err
		}
		return gwPre{decision: "denied", reason: "approval_required", approvalID: &pid, toolID: pre.toolID,
			toolName: pre.toolName}, nil
	}
	// Passata 2: tutte le approvazioni ci sono; si prenotano (used) finché la chiamata non avviene.
	for _, tid := range tids {
		var id int64
		if err := tx.QueryRowContext(ctx,
			`SELECT TOP (1) id FROM dbo.integration_approval WITH (UPDLOCK, ROWLOCK)
			  WHERE agent_id = @p1 AND resource_id = @p2 AND ISNULL(tool_id, 0) = @p3 AND status = 'approved'
			  ORDER BY id`, agentID, resourceID, tid).Scan(&id); err != nil {
			return pre, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE dbo.integration_approval SET status = 'used', used_at = SYSUTCDATETIME() WHERE id = @p1`, id); err != nil {
			return pre, err
		}
		pre.reserved = append(pre.reserved, id)
	}
	if d, r := limitDecision(ctx, tx, agentID, resourceID, 0); d != "allowed" {
		return gwPre{decision: "denied", reason: r, toolID: pre.toolID, toolName: pre.toolName}, nil
	}
	pre.decision = "allowed"
	if len(pre.reserved) > 0 {
		pre.reason = "ok_approved"
	} else {
		pre.reason = "ok"
	}
	return pre, nil
}

// releaseApprovals riporta a approved le approvazioni prenotate, se la chiamata non è avvenuta.
func (s *Server) releaseApprovals(ctx context.Context, ids []int64) {
	for _, id := range ids {
		_, _ = s.db.ExecContext(ctx,
			`UPDATE dbo.integration_approval SET status = 'approved', used_at = NULL WHERE id = @p1 AND status = 'used'`, id)
	}
}

// recordGateway registra esito e latenza: access_event (senza testo), gateway_event (solo lunghezze e hash)
// e, se la chiamata ha raggiunto il fornitore, usage_log.
func (s *Server) recordGateway(ctx context.Context, agentID, resourceID int, toolName, decision, reason string,
	argsText string, result []byte, latency int, reachedProvider, success bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	accessDecision := "denied"
	if decision == "allowed" {
		accessDecision = "allowed"
	}
	if err := writeAccessEvent(ctx, tx, agentID, resourceID, accessDecision, reason, 0); err != nil {
		return err
	}
	var argsHash, resHash sql.NullString
	if argsText != "" {
		argsHash = sql.NullString{String: sha256Hex(argsText), Valid: true}
	}
	if len(result) > 0 {
		resHash = sql.NullString{String: sha256Hex(string(result)), Valid: true}
	}
	var tn sql.NullString
	if toolName != "" {
		tn = sql.NullString{String: toolName, Valid: true}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dbo.gateway_event (agent_id, resource_id, tool_name, decision, reason, args_length, args_sha256,
		                                result_length, result_sha256, latency_ms)
		 VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10)`,
		agentID, resourceID, tn, gatewayDecisionDB(decision, success), reason, runeLen(argsText), argsHash,
		runeLen(string(result)), resHash, latency); err != nil {
		return err
	}
	if reachedProvider {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO dbo.usage_log (agent_id, tokens, cost, success, latency_ms) VALUES (@p1, 0, 0, @p2, @p3)`,
			agentID, success, latency); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// gatewayDecisionDB è l'esito salvato nel registro del gateway.
func gatewayDecisionDB(decision string, success bool) string {
	if decision == "allowed" && !success {
		return "error"
	}
	return decision
}

// gatewayCall: POST /api/gateway/call
func (s *Server) gatewayCall(w http.ResponseWriter, r *http.Request) {
	var in GatewayInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.AgentID <= 0 || in.ResourceID <= 0 {
		writeValidation(w, "agent_id e resource_id sono obbligatori")
		return
	}
	ctx := r.Context()
	var kind, endpoint string
	err := s.db.QueryRowContext(ctx,
		`SELECT kind, ISNULL(endpoint, N'') FROM dbo.integration_resource WHERE id = @p1`, in.ResourceID).Scan(&kind, &endpoint)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "risorsa")
		return
	}
	if err != nil {
		serverError(w, "gateway resource", err)
		return
	}
	mcp := kind == "mcp_server"
	if kind == "llm_model" {
		s.gatewayLLM(w, r, in, time.Now())
		return
	}
	argsText := ""
	switch {
	case mcp:
		in.Tool = strings.TrimSpace(in.Tool)
		if in.Tool == "" || runeLen(in.Tool) > 150 {
			writeValidation(w, "tool è obbligatorio per un server MCP (massimo 150 caratteri)")
			return
		}
		if len(in.Arguments) == 0 {
			in.Arguments = json.RawMessage("{}")
		}
		args, err := normalizeObject(in.Arguments)
		if err != nil {
			writeValidation(w, "arguments deve essere un oggetto JSON")
			return
		}
		in.Arguments = args
		argsText = string(args)
	case kind == "api":
		if !gatewayMethods[in.Method] {
			writeValidation(w, "method deve essere GET, POST, PUT o DELETE")
			return
		}
		if runeLen(in.Payload) > gatewayMaxArgs {
			writeValidation(w, "payload supera il limite di caratteri")
			return
		}
		argsText = in.Payload
	default:
		writeValidation(w, "il gateway accetta solo risorse mcp_server e api; per i modelli usa /api/chat/complete")
		return
	}
	if runeLen(argsText) > gatewayMaxArgs {
		writeValidation(w, "gli argomenti superano il limite di caratteri")
		return
	}

	started := time.Now()
	fail := func(decision, reason string, pre gwPre) {
		resp := GatewayResponse{Decision: decision, Reason: reason, LatencyMS: int(time.Since(started).Milliseconds())}
		resp.ApprovalID = pre.approvalID
		writeJSON(w, http.StatusOK, resp)
	}

	// Fase 1: controlli e prenotazione delle approvazioni, in transazione.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "gateway begin", err)
		return
	}
	pre, err := s.gatewayChecks(ctx, tx, in.AgentID, in.ResourceID, mcp, in.Tool)
	if err != nil {
		tx.Rollback()
		serverError(w, "gateway checks", err)
		return
	}
	if pre.decision != "allowed" {
		if pre.approvalID != nil {
			// la richiesta pending va salvata: si conferma la transazione (nessuna approvazione prenotata)
			if err := tx.Commit(); err != nil {
				serverError(w, "gateway commit pending", err)
				return
			}
		} else {
			tx.Rollback()
		}
		if err := s.recordGateway(ctx, in.AgentID, in.ResourceID, in.Tool, "denied", pre.reason, argsText, nil,
			int(time.Since(started).Milliseconds()), false, false); err != nil {
			serverError(w, "gateway record denial", err)
			return
		}
		fail("denied", pre.reason, pre)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "gateway commit", err)
		return
	}
	reserved := pre.reserved
	release := func() { s.releaseApprovals(ctx, reserved) }

	// Fase 2: guardrail in ingresso sugli argomenti.
	inRes, serr := s.guardEval(ctx, EvaluateInput{AgentID: in.AgentID, ResourceID: &in.ResourceID,
		Direction: "input", Text: argsText})
	if serr != nil {
		release()
		writeSvcErr(w, serr)
		return
	}
	if inRes.Decision == "block" {
		release()
		_ = s.recordGateway(ctx, in.AgentID, in.ResourceID, in.Tool, "blocked", "guardrail_input", argsText, nil,
			int(time.Since(started).Milliseconds()), false, false)
		fail("blocked", "guardrail_input", pre)
		return
	}
	if inRes.Decision == "mask" {
		if mcp {
			masked, err := normalizeObject(json.RawMessage(inRes.Text))
			if err != nil {
				// argomenti mascherati non più validi come oggetto: si nega per sicurezza
				release()
				_ = s.recordGateway(ctx, in.AgentID, in.ResourceID, in.Tool, "blocked", "guardrail_input", argsText, nil,
					int(time.Since(started).Milliseconds()), false, false)
				fail("blocked", "guardrail_input", pre)
				return
			}
			in.Arguments = masked
		} else {
			in.Payload = inRes.Text
		}
	}

	// Fase 3: chiamata verso il server MCP o l'endpoint.
	var result []byte
	var callErr error
	if mcp {
		var params = map[string]any{"name": in.Tool, "arguments": json.RawMessage(in.Arguments)}
		result, callErr = func() ([]byte, error) {
			raw, err := mcpRPC(ctx, endpoint, "tools/call", params)
			return raw, err
		}()
	} else {
		result, callErr = apiCall(ctx, endpoint, in.Method, in.Payload)
	}
	latency := int(time.Since(started).Milliseconds())
	if callErr != nil {
		release()
		_ = s.recordGateway(ctx, in.AgentID, in.ResourceID, in.Tool, "allowed", pre.reason, argsText, nil, latency, true, false)
		writeError(w, http.StatusBadGateway, "provider_error", callErr.Error())
		return
	}

	// Fase 4: guardrail in uscita sul risultato.
	resultText := string(result)
	outRes, serr := s.guardEval(ctx, EvaluateInput{AgentID: in.AgentID, ResourceID: &in.ResourceID,
		Direction: "output", Text: resultText})
	if serr != nil {
		writeSvcErr(w, serr)
		return
	}
	if outRes.Decision == "block" {
		_ = s.recordGateway(ctx, in.AgentID, in.ResourceID, in.Tool, "blocked", "guardrail_output", argsText, nil, latency, true, true)
		writeJSON(w, http.StatusOK, GatewayResponse{Decision: "blocked", Reason: "guardrail_output", ApprovalID: nil, LatencyMS: latency})
		return
	}
	if outRes.Decision == "mask" {
		resultText = outRes.Text
	}
	out := json.RawMessage(resultText)
	if !json.Valid(out) {
		b, _ := json.Marshal(resultText)
		out = b
	}
	_ = s.recordGateway(ctx, in.AgentID, in.ResourceID, in.Tool, "allowed", pre.reason, argsText, []byte(resultText), latency, true, true)
	writeJSON(w, http.StatusOK, GatewayResponse{Decision: "allowed", Reason: pre.reason, Result: out, LatencyMS: latency})
}

// gatewayLLM instrada una chiamata a un modello (risorsa llm_model) attraverso la stessa logica
// di /api/chat/complete: stessi controlli, stessi guardrail, stesso registro senza testo.
func (s *Server) gatewayLLM(w http.ResponseWriter, r *http.Request, in GatewayInput, started time.Time) {
	ci := ChatInput{Provider: in.Provider, APIKey: in.APIKey, Model: in.Model, System: in.System,
		Messages: in.Messages, AgentID: in.AgentID, ResourceID: in.ResourceID, Environment: in.Environment,
		PriceInputPer1k: in.PriceInputPer1k, PriceOutputPer1k: in.PriceOutputPer1k}
	if msg := validateChat(&ci); msg != "" {
		writeValidation(w, msg)
		return
	}
	res, serr := s.runChat(r.Context(), ci)
	if serr != nil {
		writeSvcErr(w, serr)
		return
	}
	out := GatewayResponse{Decision: res.Decision, Reason: res.Reason, ApprovalID: res.ApprovalID,
		LatencyMS: int(time.Since(started).Milliseconds())}
	if res.Decision == "allowed" {
		b, _ := json.Marshal(map[string]any{"text": res.Text, "input_tokens": res.InputTokens,
			"output_tokens": res.OutputTokens, "guardrail": res.Guardrail})
		out.Result = b
	}
	writeJSON(w, http.StatusOK, out)
}
