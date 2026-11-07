// Fornitore finto per i test della chat (tests/regression_chat.py) e del giudizio (tests/regression_judge.py).
// Imita la forma della risposta di Anthropic; non chiama nessun servizio reale.
// Se il messaggio contiene ERRORE_FORNITORE risponde con errore 500.
// Per il giudizio: GIUDICE_4 risponde "4", GIUDICE_JSON risponde {"score": 5}, GIUDICE_TESTO risponde una frase.
package main

import (
	"io"
	"log"
	"net/http"
	"strings"
)

func reply(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"content":[{"type":"text","text":"` + text + `"}],"usage":{"input_tokens":12,"output_tokens":7}}`))
}

func main() {
	http.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		switch {
		case strings.Contains(s, "ERRORE_FORNITORE"):
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"errore finto"}}`))
		case strings.Contains(s, "GIUDICE_4"):
			reply(w, "4")
		case strings.Contains(s, "GIUDICE_JSON"):
			reply(w, `{\"score\": 5}`)
		case strings.Contains(s, "GIUDICE_TESTO"):
			reply(w, "Direi quattro, forse")
		default:
			reply(w, "Risposta finta del fornitore")
		}
	})
	log.Fatal(http.ListenAndServe(":8081", nil))
}
