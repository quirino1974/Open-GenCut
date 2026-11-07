// Open-GenCut v0.1 - Punto 4, R2: audit a prova di manomissione.
//
// Ogni riga di dbo.audit e di dbo.access_event porta:
//
//	prev_hash = hash della riga precedente della stessa catena (NULL per la prima)
//	hash      = SHA-256 di (prev_hash, campi della riga), serializzati in JSON
//
// La scrittura prende un blocco sull'ultima riga della catena (UPDLOCK, HOLDLOCK),
// così due scritture concorrenti non si biforcano.
// GET /api/audit/verify ricalcola tutta la catena e segnala la prima riga alterata.
//
// Le righe senza hash (scritte prima dell'introduzione della catena) sono "legacy":
// vengono contate ma non verificate.
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
	"net/http"
	"strings"
	"time"
)

// Nomi delle catene: tabella e nome logico (usato nel verify).
const (
	chainAudit       = "audit"
	chainAccessEvent = "access_event"
)

// hashRecord calcola l'hash di una riga: SHA-256 di un array JSON
// [catena, prev_hash esadecimale, campi...]. La serializzazione JSON evita ambiguità tra campi.
func hashRecord(chain string, prev []byte, fields ...string) []byte {
	parts := make([]string, 0, len(fields)+2)
	parts = append(parts, chain, hex.EncodeToString(prev))
	parts = append(parts, fields...)
	raw, _ := json.Marshal(parts) // un array di stringhe non può fallire
	sum := sha256.Sum256(raw)
	return sum[:]
}

// formatAt è la rappresentazione canonica di un istante, a precisione di millisecondo.
func formatAt(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// chainTail legge l'hash dell'ultima riga della catena, con blocco per la durata della transazione.
// Restituisce nil se la catena non ha ancora righe.
//
// Il blocco è un lock applicativo (sp_getapplock) per catena, non un lock sulla riga:
// con la tabella vuota o con sole righe legacy non c'è nessuna riga su cui bloccare,
// e due scritture simultanee biforcherebbero la catena.
func chainTail(ctx context.Context, tx *sql.Tx, chain string) ([]byte, error) {
	table := "dbo." + chain
	if _, err := tx.ExecContext(ctx,
		`EXEC sp_getapplock @Resource = @p1, @LockMode = 'Exclusive', @LockOwner = 'Transaction', @LockTimeout = 30000`,
		"opengencut_chain_"+chain); err != nil {
		return nil, err
	}
	var h []byte
	err := tx.QueryRowContext(ctx,
		`SELECT TOP (1) hash FROM `+table+` WHERE hash IS NOT NULL ORDER BY id DESC`).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return h, err
}

// writeAudit registra una riga di audit concatenata. Va chiamata nella transazione della scrittura.
func writeAudit(ctx context.Context, tx *sql.Tx, action, entity string, entityID int, detail string) error {
	prev, err := chainTail(ctx, tx, chainAudit)
	if err != nil {
		return err
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	actor := actorFrom(ctx)
	h := hashRecord(chainAudit, prev, formatAt(at), actor, action, entity,
		fmt.Sprint(entityID), detail)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO dbo.audit (at, actor, action, entity, entity_id, detail, prev_hash, hash)
		 VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8)`,
		at, actor, action, entity, entityID, detail, nullBytes(prev), h)
	return err
}

// writeAccessEvent registra una decisione di accesso concatenata. Va chiamata nella transazione di checkAccess.
func writeAccessEvent(ctx context.Context, tx *sql.Tx, agentID, resourceID int,
	decision, reason string, cost float64) error {

	prev, err := chainTail(ctx, tx, chainAccessEvent)
	if err != nil {
		return err
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	h := hashRecord(chainAccessEvent, prev, formatAt(at),
		fmt.Sprint(agentID), fmt.Sprint(resourceID), decision, reason, fmt.Sprintf("%.4f", cost))

	_, err = tx.ExecContext(ctx,
		`INSERT INTO dbo.access_event (at, agent_id, resource_id, decision, reason, cost, prev_hash, hash)
		 VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8)`,
		at, agentID, resourceID, decision, reason, cost, nullBytes(prev), h)
	return err
}

// nullBytes restituisce un []byte nil per la prima riga della catena.
// Va tipizzato: un NULL senza tipo verrebbe inviato come nvarchar e non sarebbe convertibile in binary.
func nullBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// ---------------------------------------------------------------------------
// Verifica
// ---------------------------------------------------------------------------

// ChainReport è l'esito della verifica di una catena.
type ChainReport struct {
	Chain        string  `json:"chain"`
	Checked      int     `json:"checked"` // righe con hash verificate
	Legacy       int     `json:"legacy"`  // righe senza hash, non verificabili
	Valid        bool    `json:"valid"`
	BrokenAtID   *int64  `json:"broken_at_id"`  // id della prima riga alterata, o null
	BrokenReason *string `json:"broken_reason"` // motivo della prima alterazione, o null
}

// VerifyResponse è la risposta di GET /api/audit/verify.
type VerifyResponse struct {
	Valid       bool        `json:"valid"`
	Audit       ChainReport `json:"audit"`
	AccessEvent ChainReport `json:"access_event"`
}

// verifyAudit: GET /api/audit/verify. Ricalcola entrambe le catene.
func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request) {
	a, err := s.verifyChain(r.Context(), chainAudit)
	if err != nil {
		serverError(w, "verify audit", err)
		return
	}
	e, err := s.verifyChain(r.Context(), chainAccessEvent)
	if err != nil {
		serverError(w, "verify access_event", err)
		return
	}
	writeJSON(w, http.StatusOK, VerifyResponse{Valid: a.Valid && e.Valid, Audit: a, AccessEvent: e})
}

// verifyChain rilegge la catena in ordine di id e controlla ogni collegamento e ogni hash.
func (s *Server) verifyChain(ctx context.Context, chain string) (ChainReport, error) {
	rep := ChainReport{Chain: chain, Valid: true}
	var rows *sql.Rows
	var err error
	switch chain {
	case chainAudit:
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, at, actor, action, entity, entity_id, detail, prev_hash, hash
			   FROM dbo.audit ORDER BY id`)
	case chainAccessEvent:
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, at, agent_id, resource_id, decision, reason, cost, prev_hash, hash
			   FROM dbo.access_event ORDER BY id`)
	default:
		return rep, fmt.Errorf("catena sconosciuta: %s", chain)
	}
	if err != nil {
		return rep, err
	}
	defer rows.Close()

	var expectedPrev []byte // hash dell'ultima riga verificata
	seenHashed := false
	for rows.Next() {
		var id int64
		var at time.Time
		var prev, h []byte
		var fields []string
		switch chain {
		case chainAudit:
			var actor, action, entity string
			var entityID sql.NullInt64
			var detail sql.NullString
			if err := rows.Scan(&id, &at, &actor, &action, &entity, &entityID, &detail, &prev, &h); err != nil {
				return rep, err
			}
			fields = []string{formatAt(at), actor, action, entity, fmt.Sprint(entityID.Int64), detail.String}
		case chainAccessEvent:
			var agentID, resourceID int
			var decision, reason string
			var cost float64
			if err := rows.Scan(&id, &at, &agentID, &resourceID, &decision, &reason, &cost, &prev, &h); err != nil {
				return rep, err
			}
			fields = []string{formatAt(at), fmt.Sprint(agentID), fmt.Sprint(resourceID), decision, reason,
				fmt.Sprintf("%.4f", cost)}
		}

		if h == nil {
			if seenHashed {
				// Riga senza hash dopo l'inizio della catena: è una manomissione.
				return breakAt(rep, id, "riga senza hash dopo l'inizio della catena"), nil
			}
			rep.Legacy++
			continue
		}
		seenHashed = true

		if !bytes.Equal(prev, expectedPrev) {
			return breakAt(rep, id, "prev_hash non coincide con l'hash della riga precedente"), nil
		}
		recomputed := hashRecord(chain, prev, fields...)
		if !bytes.Equal(recomputed, h) {
			return breakAt(rep, id, "hash non coincide con il contenuto della riga"), nil
		}
		expectedPrev = h
		rep.Checked++
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}
	return rep, nil
}

// breakAt segna la catena come alterata alla riga indicata.
func breakAt(rep ChainReport, id int64, reason string) ChainReport {
	rep.Valid = false
	rep.BrokenAtID = &id
	rep.BrokenReason = &reason
	return rep
}

// auditDetail costruisce il dettaglio di una riga di audit come JSON a chiave stabile:
//
//	{"key":"status_change","params":{"from":"active","to":"suspended"}}
//
// La GUI traduce la chiave nella lingua dell'utente; i parametri sono i valori da inserire nel testo.
// Le righe scritte prima di questo formato restano con il testo italiano originale.
func auditDetail(key string, params map[string]any) string {
	if params == nil {
		params = map[string]any{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Il codificatore JSON ordina le chiavi della mappa: il risultato è deterministico.
	_ = enc.Encode(map[string]any{"key": key, "params": params})
	return strings.TrimRight(buf.String(), "\n")
}
