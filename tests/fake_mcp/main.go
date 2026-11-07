// Server MCP finto per i test del gateway (tests/regression_mcp.py).
// Risponde a tools/list e tools/call via JSON-RPC su HTTP e a GET /ping per le risorse api.
// Non chiama nessun servizio reale.
//   echo        restituisce gli argomenti ricevuti
//   fail        restituisce un errore JSON-RPC
//   ping-tool   restituisce un testo con un'email (per il test dei guardrail in uscita)
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

type rpc struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func main() {
	http.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req rpc
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"tools": []map[string]string{
					{"name": "echo", "description": "Restituisce gli argomenti"},
					{"name": "fail", "description": "Restituisce un errore"},
					{"name": "ping-tool", "description": "Restituisce un testo con un dato personale"},
				}}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			switch p.Name {
			case "echo":
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{"echo": json.RawMessage(p.Arguments)}})
			case "ping-tool":
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{"text": "contatto: mario.rossi@example.com"}})
			default:
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
					"error": map[string]any{"code": -32000, "message": "errore finto"}})
			}
		default:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "metodo non previsto"}})
		}
	})
	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"message":"pong"}`))
	})
	log.Fatal(http.ListenAndServe(":8082", nil))
}
