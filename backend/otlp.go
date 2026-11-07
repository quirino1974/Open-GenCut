// Open-GenCut - Ingestione OpenTelemetry in JSON (punto 3 della specifica, docs/API.txt).
//
// POST /api/evaluation/otlp?agent_id=N accetta il formato OTLP/JSON (resourceSpans).
// Ogni traccia OpenTelemetry diventa una traccia di valutazione dell'agente indicato:
//   - esito: successo; partial se un passo ha errore; failure se lo span radice ha errore;
//   - passi: ogni span, con tipo (attributo gencut.step_type, altrimenti dedotto), nome, token, latenza;
//   - token: gen_ai.usage.input_tokens + gen_ai.usage.output_tokens;
//   - obiettivo e costo: attributi gencut.goal_met e gencut.cost dello span radice.
//
// Gli attributi testuali (prompt, risposte, valori) non vengono mai letti né salvati.
// I nomi degli span sono identificatori e vengono salvati (massimo 150 caratteri).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	otlpMaxBody   = 5 << 20 // 5 MB
	otlpMaxSpans  = 5000
	otlpMaxTraces = 500
)

// otlpAny è un valore di attributo OTLP/JSON.
type otlpAny struct {
	StringValue *string         `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
	DoubleValue *float64        `json:"doubleValue"`
	BoolValue   *bool           `json:"boolValue"`
}

type otlpKV struct {
	Key   string  `json:"key"`
	Value otlpAny `json:"value"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId"`
	Name              string          `json:"name"`
	StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
	EndTimeUnixNano   json.RawMessage `json:"endTimeUnixNano"`
	Status            struct {
		Code json.RawMessage `json:"code"`
	} `json:"status"`
	Attributes []otlpKV `json:"attributes"`
}

type otlpRequest struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []otlpKV `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []otlpSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// registerOTLP aggiunge la rotta di ingestione al mux principale.
func (s *Server) registerOTLP(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/evaluation/otlp", s.ingestOTLP)
}

// attr cerca un attributo per chiave.
func attr(kvs []otlpKV, key string) (otlpAny, bool) {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return otlpAny{}, false
}

// rawInt legge un intero OTLP (stringa o numero).
func rawInt(raw json.RawMessage) (int64, bool) {
	t := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if t == "" || t == "null" {
		return 0, false
	}
	n, err := strconv.ParseInt(t, 10, 64)
	return n, err == nil
}

// intAttr restituisce un attributo intero (non negativo, entro il limite del database).
func intAttr(kvs []otlpKV, key string) (int, bool) {
	v, ok := attr(kvs, key)
	if !ok {
		return 0, false
	}
	n, ok := rawInt(v.IntValue)
	if !ok {
		if v.DoubleValue != nil {
			return int(*v.DoubleValue), true
		}
		return 0, false
	}
	if n < 0 {
		n = 0
	}
	if n > maxInt32 {
		n = maxInt32
	}
	return int(n), true
}

// spanIsError dice se lo span ha stato di errore (codice 2 o nome STATUS_CODE_ERROR).
func spanIsError(sp otlpSpan) bool {
	t := strings.Trim(strings.TrimSpace(string(sp.Status.Code)), `"`)
	return t == "2" || strings.Contains(t, "ERROR")
}

// nanoMS converte un istante OTLP in nanosecondi (testo o numero) in millisecondi.
func nanoMS(start, end json.RawMessage) int {
	s, ok1 := rawInt(start)
	e, ok2 := rawInt(end)
	if !ok1 || !ok2 || e <= s {
		return 0
	}
	ms := (e - s) / 1_000_000
	if ms > maxInt32 {
		ms = maxInt32
	}
	return int(ms)
}

// stepType sceglie il tipo di passo: attributo gencut.step_type, altrimenti dedotto dagli attributi e dal nome.
func stepType(sp otlpSpan) string {
	if v, ok := attr(sp.Attributes, "gencut.step_type"); ok && v.StringValue != nil && stepTypes[*v.StringValue] {
		return *v.StringValue
	}
	if _, ok := attr(sp.Attributes, "gen_ai.request.model"); ok {
		return "llm"
	}
	if _, ok := attr(sp.Attributes, "gen_ai.usage.input_tokens"); ok {
		return "llm"
	}
	name := strings.ToLower(sp.Name)
	switch {
	case strings.Contains(name, "tool"):
		return "tool"
	case strings.Contains(name, "memory"):
		return "memory"
	}
	return "reasoning"
}

// spanToTrace converte un gruppo di span con la stessa traceId in una traccia di valutazione.
func spanToTrace(traceID string, spans []otlpSpan) TraceInput {
	in := TraceInput{TraceID: traceID, Outcome: "success", Steps: []TraceStepInput{}}
	var root *otlpSpan
	hasErr := false
	for i := range spans {
		if spans[i].ParentSpanID == "" && root == nil {
			root = &spans[i]
		}
	}
	if root == nil {
		root = &spans[0]
	}
	rootErr := spanIsError(*root)
	for _, sp := range spans {
		if spanIsError(sp) {
			hasErr = true
		}
		name := strings.TrimSpace(sp.Name)
		if name == "" {
			name = "span"
		}
		if runeLen(name) > 150 {
			name = string([]rune(name)[:150])
		}
		st := TraceStepInput{Type: stepType(sp), Name: name}
		tokens := 0
		if n, ok := intAttr(sp.Attributes, "gen_ai.usage.input_tokens"); ok {
			tokens += n
		}
		if n, ok := intAttr(sp.Attributes, "gen_ai.usage.output_tokens"); ok {
			tokens += n
		}
		if tokens > maxInt32 {
			tokens = maxInt32
		}
		st.Tokens = tokens
		st.LatencyMS = nanoMS(sp.StartTimeUnixNano, sp.EndTimeUnixNano)
		ok := !spanIsError(sp)
		st.OK = &ok
		if st.Type == "memory" {
			if age, found := intAttr(sp.Attributes, "gencut.memory_age_days"); found && age <= 36500 {
				st.MemoryAgeDays = &age
			}
		}
		in.Steps = append(in.Steps, st)
	}
	switch {
	case rootErr:
		in.Outcome = "failure"
	case hasErr:
		in.Outcome = "partial"
	}
	if v, ok := attr(root.Attributes, "gencut.goal_met"); ok && v.BoolValue != nil {
		in.GoalMet = *v.BoolValue
	}
	if v, ok := attr(root.Attributes, "gencut.cost"); ok {
		c := 0.0
		if v.DoubleValue != nil {
			c = *v.DoubleValue
		} else if n, ok := rawInt(v.IntValue); ok {
			c = float64(n)
		}
		if validMoney(c) {
			in.Cost = c
		}
	}
	return in
}

// ingestOTLP: POST /api/evaluation/otlp?agent_id=N
// Risponde 200 {traces_created, traces_skipped, spans}. Le tracce già presenti (stesso trace_id) sono saltate.
func (s *Server) ingestOTLP(w http.ResponseWriter, r *http.Request) {
	agentID, ok := optionalID(w, r.URL.Query().Get("agent_id"), "agent_id")
	if !ok || agentID == 0 {
		writeValidation(w, "agent_id è obbligatorio")
		return
	}
	var req otlpRequest
	r.Body = http.MaxBytesReader(w, r.Body, otlpMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "OTLP/JSON non valido")
		return
	}
	// Ambiente dal resource (deployment.environment), se valido.
	env := ""
	groups := map[string][]otlpSpan{}
	order := []string{}
	total := 0
	for _, rs := range req.ResourceSpans {
		if v, found := attr(rs.Resource.Attributes, "deployment.environment"); found && v.StringValue != nil &&
			validEnvironments[*v.StringValue] {
			env = *v.StringValue
		}
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				total++
				if total > otlpMaxSpans {
					writeValidation(w, fmt.Sprintf("il batch supera %d span", otlpMaxSpans))
					return
				}
				id := strings.ToLower(strings.TrimSpace(sp.TraceID))
				if id == "" {
					writeValidation(w, "ogni span deve avere traceId")
					return
				}
				if _, seen := groups[id]; !seen {
					order = append(order, id)
				}
				groups[id] = append(groups[id], sp)
			}
		}
	}
	if len(order) > otlpMaxTraces {
		writeValidation(w, fmt.Sprintf("il batch supera %d tracce", otlpMaxTraces))
		return
	}
	ctx := r.Context()
	created, skipped := 0, 0
	for _, id := range order {
		if runeLen(id) > 100 {
			writeValidation(w, "traceId troppo lungo")
			return
		}
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM dbo.eval_trace WHERE agent_id = @p1 AND trace_id = @p2`, agentID, id).Scan(&n); err != nil {
			serverError(w, "otlp exists", err)
			return
		}
		if n > 0 {
			skipped++
			continue
		}
		in := spanToTrace(id, groups[id])
		in.AgentID = agentID
		in.Environment = env
		if msg := validateTrace(&in); msg != "" {
			writeValidation(w, msg)
			return
		}
		if _, serr := s.recordTrace(ctx, in); serr != nil {
			writeSvcErr(w, serr)
			return
		}
		created++
	}
	writeJSON(w, http.StatusOK, map[string]int{"traces_created": created, "traces_skipped": skipped, "spans": total})
}
