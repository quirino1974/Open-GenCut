// Open-GenCut - Errori dei servizi interni, riusabili fuori dai gestori HTTP (chat, guardrail, controllo accessi).
package main

import (
	"fmt"
	"net/http"
)

// svcErr è un errore di servizio con stato HTTP e codice stabile.
type svcErr struct {
	status int
	code   string
	msg    string
	err    error
}

func (e *svcErr) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %v", e.msg, e.err)
	}
	return e.msg
}

// internalErr è un errore interno: il dettaglio resta nel log, la risposta è generica.
func internalErr(where string, err error) *svcErr {
	return &svcErr{status: http.StatusInternalServerError, code: "internal_error", msg: where, err: err}
}

// notFoundErr è una risorsa inesistente.
func notFoundErr(what string) *svcErr {
	return &svcErr{status: http.StatusNotFound, code: "not_found", msg: what + " non trovato"}
}

// writeSvcErr scrive l'errore di servizio nella forma uniforme dell'API.
func writeSvcErr(w http.ResponseWriter, e *svcErr) {
	if e.status == http.StatusInternalServerError {
		serverError(w, e.msg, e.err)
		return
	}
	writeError(w, e.status, e.code, e.msg)
}
