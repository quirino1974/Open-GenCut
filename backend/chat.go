// Open-GenCut - Chat verso OpenAI e Anthropic attraverso il backend (docs/CHAT.txt).
//
// Ordine di esecuzione di POST /api/chat/complete:
//  1. controllo di accesso su agente e risorsa (accessCheck); se negato, nessuna chiamata e nessun costo;
//  2. guardrail in ingresso sull'ultimo messaggio utente; block -> blocked; mask -> testo mascherato;
//  3. chiamata al fornitore con timeout di 60 secondi;
//  4. guardrail in uscita sulla risposta; block -> la risposta non viene restituita;
//  5. registrazione del monitoraggio: esito, latenza, token, costo.
//
// La chiave API è usata solo nella chiamata al fornitore: non viene salvata, registrata né restituita.
// Il testo dei messaggi non viene salvato (il registro dei guardrail conserva solo lunghezza e hash).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	chatMaxTokens     = 1024             // max_tokens fisso per la risposta
	chatTimeout       = 60 * time.Second // timeout della chiamata al fornitore
	chatMaxRespBytes  = 2 << 20          // dimensione massima della risposta del fornitore
	chatMaxMessages   = 50
	chatMaxMsgRunes   = 20000
	chatMaxSystemRune = 10000
)

var (
	chatProviders = map[string]bool{"anthropic": true, "openai": true}
	chatRoles     = map[string]bool{"user": true, "assistant": true}
)

// ChatMessage è un messaggio della conversazione.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatInput è il corpo di POST /api/chat/complete.
type ChatInput struct {
	Provider         string        `json:"provider"`
	APIKey           string        `json:"api_key"`
	Model            string        `json:"model"`
	System           string        `json:"system"`
	Messages         []ChatMessage `json:"messages"`
	AgentID          int           `json:"agent_id"`
	ResourceID       int           `json:"resource_id"`
	Environment      string        `json:"environment"`
	PriceInputPer1k  *float64      `json:"price_input_per_1k"`
	PriceOutputPer1k *float64      `json:"price_output_per_1k"`
}

// ChatGuardrail è l'esito dei guardrail nella chiamata.
type ChatGuardrail struct {
	Decision   string      `json:"decision"`
	Violations []Violation `json:"violations"`
}

// ChatResult è la risposta di POST /api/chat/complete.
type ChatResult struct {
	Decision     string        `json:"decision"` // allowed | denied | blocked
	Reason       string        `json:"reason"`
	Text         string        `json:"text"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
	LatencyMS    int           `json:"latency_ms"`
	Guardrail    ChatGuardrail `json:"guardrail"`
	ApprovalID   *int          `json:"approval_id,omitempty"`
}

// registerChat aggiunge la rotta di chat al mux principale.
func (s *Server) registerChat(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/chat/complete", s.chatComplete)
}

// providerURL restituisce l'URL del fornitore. Gli URL predefiniti sono quelli pubblici;
// le variabili CHAT_ANTHROPIC_URL e CHAT_OPENAI_URL servono solo per i test con un fornitore finto.
func providerURL(provider string) string {
	if provider == "anthropic" {
		return getenv("CHAT_ANTHROPIC_URL", "https://api.anthropic.com/v1/messages")
	}
	return getenv("CHAT_OPENAI_URL", "https://api.openai.com/v1/chat/completions")
}

// validateChat controlla il corpo. Nessun messaggio di errore contiene la chiave.
func validateChat(in *ChatInput) string {
	if !chatProviders[in.Provider] {
		return "provider deve essere anthropic o openai"
	}
	if in.APIKey == "" || runeLen(in.APIKey) > 500 {
		return "api_key è obbligatoria (massimo 500 caratteri)"
	}
	in.Model = strings.TrimSpace(in.Model)
	if in.Model == "" || runeLen(in.Model) > 100 {
		return "model è obbligatorio (massimo 100 caratteri)"
	}
	if runeLen(in.System) > chatMaxSystemRune {
		return "system supera il limite di caratteri"
	}
	if len(in.Messages) == 0 || len(in.Messages) > chatMaxMessages {
		return fmt.Sprintf("messages deve contenere da 1 a %d elementi", chatMaxMessages)
	}
	for i, m := range in.Messages {
		if !chatRoles[m.Role] {
			return fmt.Sprintf("messaggio %d: role deve essere user o assistant", i+1)
		}
		if m.Content == "" || runeLen(m.Content) > chatMaxMsgRunes {
			return fmt.Sprintf("messaggio %d: content obbligatorio (massimo %d caratteri)", i+1, chatMaxMsgRunes)
		}
	}
	if in.Messages[len(in.Messages)-1].Role != "user" {
		return "l'ultimo messaggio deve avere role user"
	}
	if in.AgentID <= 0 || in.ResourceID <= 0 {
		return "agent_id e resource_id sono obbligatori"
	}
	if in.Environment == "" {
		in.Environment = "production"
	}
	if !validEnvironments[in.Environment] {
		return "environment non valido"
	}
	for _, p := range []*float64{in.PriceInputPer1k, in.PriceOutputPer1k} {
		if p != nil && !validMoney(*p) {
			return "i prezzi per 1000 token devono essere maggiori o uguali a 0"
		}
	}
	return ""
}

func priceOf(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// estimateCost stima il costo massimo di una chiamata: testo in ingresso (circa 3 caratteri per token)
// e risposta al limite chatMaxTokens. Serve al controllo di accesso prima della chiamata.
func estimateCost(in ChatInput) float64 {
	chars := runeLen(in.System)
	for _, m := range in.Messages {
		chars += runeLen(m.Content)
	}
	inTokens := math.Ceil(float64(chars) / 3)
	return inTokens/1000*priceOf(in.PriceInputPer1k) + float64(chatMaxTokens)/1000*priceOf(in.PriceOutputPer1k)
}

// chatComplete: POST /api/chat/complete
func (s *Server) chatComplete(w http.ResponseWriter, r *http.Request) {
	var in ChatInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if msg := validateChat(&in); msg != "" {
		writeValidation(w, msg)
		return
	}
	res, serr := s.runChat(r.Context(), in)
	if serr != nil {
		writeSvcErr(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// runChat esegue la chat: controllo di accesso, guardrail, fornitore, guardrail, monitoraggio.
// È riusata da /api/chat/complete e dal gateway per le risorse llm_model. Il corpo deve essere già validato.
func (s *Server) runChat(ctx context.Context, in ChatInput) (ChatResult, *svcErr) {

	// 1. Controllo di accesso. Negato: nessuna chiamata, nessun costo.
	cost := estimateCost(in)
	access, serr := s.accessCheck(ctx, in.AgentID, in.ResourceID, cost)
	if serr != nil {
		return ChatResult{}, serr
	}
	if access.Decision != "allowed" {
		return ChatResult{Decision: "denied", Reason: access.Reason, ApprovalID: access.ApprovalID,
			Guardrail: ChatGuardrail{Decision: "allow", Violations: []Violation{}}}, nil
	}

	// 2. Guardrail in ingresso sull'ultimo messaggio utente.
	last := len(in.Messages) - 1
	inRes, serr := s.guardEval(ctx, EvaluateInput{AgentID: in.AgentID, ResourceID: &in.ResourceID,
		Direction: "input", Text: in.Messages[last].Content})
	if serr != nil {
		return ChatResult{}, serr
	}
	inGuard := ChatGuardrail{Decision: inRes.Decision, Violations: inRes.Violations}
	if inRes.Decision == "block" {
		return ChatResult{Decision: "blocked", Reason: "guardrail_input",
			Guardrail: inGuard}, nil
	}
	if inRes.Decision == "mask" {
		in.Messages[last].Content = inRes.Text
	}

	// 3. Chiamata al fornitore. La chiave passa solo qui.
	started := time.Now()
	text, inTok, outTok, err := callProvider(ctx, in)
	latency := int(time.Since(started).Milliseconds())
	if err != nil {
		// Chiamata fallita: registrata nel monitoraggio come esito negativo, senza costo.
		if rerr := recordUsage(ctx, s, in.AgentID, in.Environment, 0, 0, false, latency); rerr != nil {
			return ChatResult{}, internalErr("chat usage", rerr)
		}
		return ChatResult{}, &svcErr{status: http.StatusBadGateway, code: "provider_error", msg: err.Error()}
	}

	// 4. Guardrail in uscita sulla risposta.
	outRes, serr := s.guardEval(ctx, EvaluateInput{AgentID: in.AgentID, ResourceID: &in.ResourceID,
		Direction: "output", Text: text})
	if serr != nil {
		return ChatResult{}, serr
	}
	outGuard := ChatGuardrail{Decision: outRes.Decision, Violations: outRes.Violations}
	guard := combineGuard(inGuard, outGuard)

	// 5. Registrazione del monitoraggio con costo reale (il fornitore ha risposto e ha fatturato).
	realCost := float64(inTok)/1000*priceOf(in.PriceInputPer1k) + float64(outTok)/1000*priceOf(in.PriceOutputPer1k)
	realCost = math.Round(realCost*1e6) / 1e6
	if err := recordUsage(ctx, s, in.AgentID, in.Environment, inTok+outTok, realCost, true, latency); err != nil {
		return ChatResult{}, internalErr("chat usage", err)
	}

	if outRes.Decision == "block" {
		return ChatResult{Decision: "blocked", Reason: "guardrail_output", Text: "",
			InputTokens: inTok, OutputTokens: outTok, LatencyMS: latency, Guardrail: guard}, nil
	}
	if outRes.Decision == "mask" {
		text = outRes.Text
	}
	return ChatResult{Decision: "allowed", Reason: "ok", Text: text,
		InputTokens: inTok, OutputTokens: outTok, LatencyMS: latency, Guardrail: guard}, nil
}

// combineGuard unisce gli esiti dei guardrail di ingresso e uscita: block prevale su mask.
func combineGuard(in, out ChatGuardrail) ChatGuardrail {
	g := ChatGuardrail{Decision: "allow", Violations: []Violation{}}
	for _, x := range []ChatGuardrail{in, out} {
		g.Violations = append(g.Violations, x.Violations...)
		if x.Decision == "block" {
			g.Decision = "block"
		} else if x.Decision == "mask" && g.Decision != "block" {
			g.Decision = "mask"
		}
	}
	return g
}

// recordUsage registra una chiamata nel monitoraggio (stessa tabella di /api/lifecycle/agents/{id}/calls).
func recordUsage(ctx context.Context, s *Server, agentID int, env string, tokens int, cost float64,
	success bool, latency int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dbo.usage_log (agent_id, environment, tokens, cost, success, latency_ms)
		 VALUES (@p1, @p2, @p3, @p4, @p5, @p6)`,
		agentID, env, tokens, cost, success, latency)
	return err
}

// callProvider chiama il fornitore con timeout di 60 secondi. Restituisce il testo e i token.
// Gli errori non contengono la chiave né il corpo della risposta del fornitore.
func callProvider(ctx context.Context, in ChatInput) (string, int, int, error) {
	var body map[string]any
	var headers map[string]string
	msgs := make([]map[string]string, 0, len(in.Messages)+1)
	if in.Provider == "anthropic" {
		for _, m := range in.Messages {
			msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
		}
		body = map[string]any{"model": in.Model, "max_tokens": chatMaxTokens, "messages": msgs}
		if in.System != "" {
			body["system"] = in.System
		}
		headers = map[string]string{"x-api-key": in.APIKey, "anthropic-version": "2023-06-01",
			"content-type": "application/json"}
	} else {
		if in.System != "" {
			msgs = append(msgs, map[string]string{"role": "system", "content": in.System})
		}
		for _, m := range in.Messages {
			msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
		}
		body = map[string]any{"model": in.Model, "max_completion_tokens": chatMaxTokens, "messages": msgs}
		headers = map[string]string{"Authorization": "Bearer " + in.APIKey, "content-type": "application/json"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", 0, 0, errors.New("richiesta non valida")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, providerURL(in.Provider), bytes.NewReader(raw))
	if err != nil {
		return "", 0, 0, errors.New("richiesta non valida")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: chatTimeout}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Timeout") {
			return "", 0, 0, errors.New("il fornitore non ha risposto entro 60 secondi")
		}
		return "", 0, 0, errors.New("impossibile contattare il fornitore")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, chatMaxRespBytes))
	if err != nil {
		return "", 0, 0, errors.New("risposta del fornitore non leggibile")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, 0, fmt.Errorf("il fornitore ha risposto con stato %d", resp.StatusCode)
	}
	return parseProviderReply(in.Provider, data)
}

// parseProviderReply estrae testo e token dalla risposta del fornitore.
func parseProviderReply(provider string, data []byte) (string, int, int, error) {
	if provider == "anthropic" {
		var r struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return "", 0, 0, errors.New("risposta del fornitore non valida")
		}
		var sb strings.Builder
		for _, c := range r.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		return sb.String(), r.Usage.InputTokens, r.Usage.OutputTokens, nil
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return "", 0, 0, errors.New("risposta del fornitore non valida")
	}
	if len(r.Choices) == 0 {
		return "", 0, 0, errors.New("risposta del fornitore senza testo")
	}
	return r.Choices[0].Message.Content, r.Usage.PromptTokens, r.Usage.CompletionTokens, nil
}
