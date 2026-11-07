// Open-GenCut - Punto 7 della specifica: marketplace degli agenti.
//
// Inserzioni di agenti di terze parti con revisione di sicurezza; richieste di adozione;
// decisione (approva/rifiuta) e provisioning che crea l'agente nel registro con origine marketplace.
// Stati della richiesta: pending -> approved | rejected; approved -> provisioned (una sola volta).
package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

var (
	priceModels     = map[string]bool{"free": true, "subscription": true, "per_use": true}
	securityReviews = map[string]bool{"pending": true, "approved": true, "rejected": true}
	listingStatuses = map[string]bool{"listed": true, "hidden": true}
)

// ---------------------------------------------------------------------------
// Modelli
// ---------------------------------------------------------------------------

// Listing è un'inserzione del marketplace.
type Listing struct {
	ID             int     `json:"id"`
	Provider       string  `json:"provider"`
	Name           string  `json:"name"`
	Version        *string `json:"version"`
	Framework      *string `json:"framework"`
	PriceModel     string  `json:"price_model"`
	Price          float64 `json:"price"`
	Currency       string  `json:"currency"`
	Certified      bool    `json:"certified"`
	SecurityReview string  `json:"security_review"`
	Status         string  `json:"status"`
}

// ListingInput è il corpo di POST e PUT delle inserzioni (sostituzione completa).
type ListingInput struct {
	Provider       string   `json:"provider"`
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	Framework      string   `json:"framework"`
	PriceModel     string   `json:"price_model"`
	Price          *float64 `json:"price"`
	Currency       string   `json:"currency"`
	Certified      bool     `json:"certified"`
	SecurityReview string   `json:"security_review"` // default "pending"
	Status         string   `json:"status"`          // default "hidden"
}

// MarketRequest è una richiesta di adozione di un'inserzione.
type MarketRequest struct {
	ID            int        `json:"id"`
	ListingID     int        `json:"listing_id"`
	RequestedBy   string     `json:"requested_by"`
	Justification *string    `json:"justification"`
	Status        string     `json:"status"`
	DecidedBy     *string    `json:"decided_by"`
	DecisionNote  *string    `json:"decision_note"`
	AgentID       *int       `json:"agent_id"`
	CreatedAt     time.Time  `json:"created_at"`
	DecidedAt     *time.Time `json:"decided_at"`
	ProvisionedAt *time.Time `json:"provisioned_at"`
}

// RequestInput è il corpo di POST /api/marketplace/requests.
type RequestInput struct {
	ListingID     int    `json:"listing_id"`
	RequestedBy   string `json:"requested_by"`
	Justification string `json:"justification"`
}

// ---------------------------------------------------------------------------
// Rotte
// ---------------------------------------------------------------------------

// registerMarketplace aggiunge le rotte del marketplace al mux principale.
func (s *Server) registerMarketplace(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/marketplace/listings", s.listListings)
	mux.HandleFunc("POST /api/marketplace/listings", s.createListing)
	mux.HandleFunc("GET /api/marketplace/listings/{id}", s.getListing)
	mux.HandleFunc("PUT /api/marketplace/listings/{id}", s.updateListing)
	mux.HandleFunc("DELETE /api/marketplace/listings/{id}", s.deleteListing)

	mux.HandleFunc("GET /api/marketplace/requests", s.listRequests)
	mux.HandleFunc("POST /api/marketplace/requests", s.createRequest)
	mux.HandleFunc("GET /api/marketplace/requests/{id}", s.getRequest)
	mux.HandleFunc("POST /api/marketplace/requests/{id}/approve", s.approveRequest)
	mux.HandleFunc("POST /api/marketplace/requests/{id}/reject", s.rejectRequest)
	mux.HandleFunc("POST /api/marketplace/requests/{id}/provision", s.provisionRequest)
}

// ---------------------------------------------------------------------------
// Inserzioni
// ---------------------------------------------------------------------------

const listingColumns = `l.id, l.provider, l.name, l.version, l.framework, l.price_model, l.price, l.currency,
	l.certified, l.security_review, l.status`

func scanListing(sc rowScanner) (Listing, error) {
	var l Listing
	var ver, fw sql.NullString
	if err := sc.Scan(&l.ID, &l.Provider, &l.Name, &ver, &fw, &l.PriceModel, &l.Price, &l.Currency,
		&l.Certified, &l.SecurityReview, &l.Status); err != nil {
		return Listing{}, err
	}
	l.Version = ptrOf(ver)
	l.Framework = ptrOf(fw)
	return l, nil
}

func (s *Server) fetchListing(ctx context.Context, id int) (Listing, error) {
	return scanListing(s.db.QueryRowContext(ctx,
		`SELECT `+listingColumns+` FROM dbo.marketplace_listing l WHERE l.id = @p1`, id))
}

// decodeListingInput legge e valida un'inserzione.
func decodeListingInput(w http.ResponseWriter, r *http.Request) (ListingInput, bool) {
	var in ListingInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	in.Provider = strings.TrimSpace(in.Provider)
	in.Name = strings.TrimSpace(in.Name)
	if in.Provider == "" || runeLen(in.Provider) > 150 || in.Name == "" || runeLen(in.Name) > 150 {
		writeValidation(w, "provider e name sono obbligatori (massimo 150 caratteri)")
		return in, false
	}
	if runeLen(in.Version) > 50 || runeLen(in.Framework) > 100 {
		writeValidation(w, "version o framework superano il limite di lunghezza")
		return in, false
	}
	if !priceModels[in.PriceModel] {
		writeValidation(w, "price_model deve essere free, subscription o per_use")
		return in, false
	}
	if in.Currency == "" {
		in.Currency = "EUR"
	}
	if !validCurrency[in.Currency] {
		writeValidation(w, "currency deve essere EUR o USD")
		return in, false
	}
	price := 0.0
	if in.Price != nil {
		price = *in.Price
	}
	if !validMoney(price) {
		writeValidation(w, "price deve essere maggiore o uguale a 0 nei limiti del database")
		return in, false
	}
	if in.PriceModel == "free" && price != 0 {
		writeValidation(w, "un'inserzione gratuita ha price 0")
		return in, false
	}
	if in.SecurityReview == "" {
		in.SecurityReview = "pending"
	}
	if !securityReviews[in.SecurityReview] {
		writeValidation(w, "security_review deve essere pending, approved o rejected")
		return in, false
	}
	if in.Status == "" {
		in.Status = "hidden"
	}
	if !listingStatuses[in.Status] {
		writeValidation(w, "status deve essere listed o hidden")
		return in, false
	}
	return in, true
}

// listListings: GET /api/marketplace/listings
func (s *Server) listListings(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+listingColumns+` FROM dbo.marketplace_listing l ORDER BY l.id`)
	if err != nil {
		serverError(w, "listListings", err)
		return
	}
	defer rows.Close()
	out := []Listing{}
	for rows.Next() {
		l, err := scanListing(rows)
		if err != nil {
			serverError(w, "listListings scan", err)
			return
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listListings rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getListing: GET /api/marketplace/listings/{id}
func (s *Server) getListing(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	l, err := s.fetchListing(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "inserzione")
		return
	}
	if err != nil {
		serverError(w, "getListing", err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// createListing: POST /api/marketplace/listings. Risponde 201.
func (s *Server) createListing(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeListingInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createListing begin", err)
		return
	}
	defer tx.Rollback()
	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.marketplace_listing (provider, name, version, framework, price_model, price, currency,
		                                     certified, security_review, status)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10)`,
		in.Provider, in.Name, nullable(in.Version), nullable(in.Framework), in.PriceModel,
		priceOrZero(in.Price), in.Currency, in.Certified, in.SecurityReview, in.Status).Scan(&id)
	if err != nil {
		writeDBError(w, "createListing insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "marketplace_listing", id,
		auditDetail("listing_saved", map[string]any{"name": in.Name, "provider": in.Provider})); err != nil {
		serverError(w, "createListing audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createListing commit", err)
		return
	}
	l, err := s.fetchListing(r.Context(), id)
	if err != nil {
		serverError(w, "createListing fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

// updateListing: PUT /api/marketplace/listings/{id}. Sostituzione completa.
func (s *Server) updateListing(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeListingInput(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateListing begin", err)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.marketplace_listing
		    SET provider = @p1, name = @p2, version = @p3, framework = @p4, price_model = @p5, price = @p6,
		        currency = @p7, certified = @p8, security_review = @p9, status = @p10, updated_at = SYSUTCDATETIME()
		  WHERE id = @p11`,
		in.Provider, in.Name, nullable(in.Version), nullable(in.Framework), in.PriceModel, priceOrZero(in.Price),
		in.Currency, in.Certified, in.SecurityReview, in.Status, id)
	if err != nil {
		writeDBError(w, "updateListing update", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeNotFound(w, "inserzione")
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "marketplace_listing", id,
		auditDetail("listing_saved", map[string]any{"name": in.Name, "provider": in.Provider})); err != nil {
		serverError(w, "updateListing audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateListing commit", err)
		return
	}
	l, err := s.fetchListing(r.Context(), id)
	if err != nil {
		serverError(w, "updateListing fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// deleteListing: DELETE /api/marketplace/listings/{id}. Le richieste collegate vengono rimosse. Risponde 204.
func (s *Server) deleteListing(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteListing begin", err)
		return
	}
	defer tx.Rollback()
	var name string
	err = tx.QueryRowContext(r.Context(),
		`SELECT name FROM dbo.marketplace_listing WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "inserzione")
		return
	}
	if err != nil {
		serverError(w, "deleteListing read", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.marketplace_listing WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteListing delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "marketplace_listing", id,
		auditDetail("listing_deleted", map[string]any{"name": name})); err != nil {
		serverError(w, "deleteListing audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteListing commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func priceOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// ---------------------------------------------------------------------------
// Richieste
// ---------------------------------------------------------------------------

const requestColumns = `q.id, q.listing_id, q.requested_by, q.justification, q.status, q.decided_by,
	q.decision_note, q.agent_id, q.created_at, q.decided_at, q.provisioned_at`

func scanRequest(sc rowScanner) (MarketRequest, error) {
	var q MarketRequest
	var just, by, note sql.NullString
	var agent sql.NullInt64
	var decided, prov sql.NullTime
	if err := sc.Scan(&q.ID, &q.ListingID, &q.RequestedBy, &just, &q.Status, &by, &note, &agent,
		&q.CreatedAt, &decided, &prov); err != nil {
		return MarketRequest{}, err
	}
	q.Justification = ptrOf(just)
	q.DecidedBy = ptrOf(by)
	q.DecisionNote = ptrOf(note)
	if agent.Valid {
		v := int(agent.Int64)
		q.AgentID = &v
	}
	if decided.Valid {
		t := decided.Time
		q.DecidedAt = &t
	}
	if prov.Valid {
		t := prov.Time
		q.ProvisionedAt = &t
	}
	return q, nil
}

func (s *Server) fetchRequest(ctx context.Context, id int) (MarketRequest, error) {
	return scanRequest(s.db.QueryRowContext(ctx,
		`SELECT `+requestColumns+` FROM dbo.marketplace_request q WHERE q.id = @p1`, id))
}

// listRequests: GET /api/marketplace/requests?status=
func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "", "pending", "approved", "rejected", "provisioned":
	default:
		writeValidation(w, "status deve essere pending, approved, rejected o provisioned")
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+requestColumns+` FROM dbo.marketplace_request q
		  WHERE (@p1 = N'' OR q.status = @p1) ORDER BY q.id DESC`, status)
	if err != nil {
		serverError(w, "listRequests", err)
		return
	}
	defer rows.Close()
	out := []MarketRequest{}
	for rows.Next() {
		q, err := scanRequest(rows)
		if err != nil {
			serverError(w, "listRequests scan", err)
			return
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listRequests rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getRequest: GET /api/marketplace/requests/{id}
func (s *Server) getRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q, err := s.fetchRequest(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "richiesta")
		return
	}
	if err != nil {
		serverError(w, "getRequest", err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

// createRequest: POST /api/marketplace/requests. Risponde 201 pending.
// Non ammessa se l'inserzione non è listed con security_review approved (409 invalid_state).
func (s *Server) createRequest(w http.ResponseWriter, r *http.Request) {
	var in RequestInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.RequestedBy = strings.TrimSpace(in.RequestedBy)
	if in.ListingID <= 0 || in.RequestedBy == "" || runeLen(in.RequestedBy) > 150 {
		writeValidation(w, "listing_id e requested_by sono obbligatori")
		return
	}
	if runeLen(in.Justification) > 500 {
		writeValidation(w, "justification supera 500 caratteri")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createRequest begin", err)
		return
	}
	defer tx.Rollback()

	var status, review string
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, security_review FROM dbo.marketplace_listing WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`,
		in.ListingID).Scan(&status, &review)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "inserzione")
		return
	}
	if err != nil {
		serverError(w, "createRequest listing", err)
		return
	}
	if status != "listed" || review != "approved" {
		writeInvalidState(w, "inserzione non richiedibile: deve essere listed e con revisione di sicurezza approved")
		return
	}
	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.marketplace_request (listing_id, requested_by, justification)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3)`,
		in.ListingID, in.RequestedBy, nullable(strings.TrimSpace(in.Justification))).Scan(&id)
	if err != nil {
		writeDBError(w, "createRequest insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "marketplace_request", id,
		auditDetail("request_created", map[string]any{"listing_id": in.ListingID})); err != nil {
		serverError(w, "createRequest audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createRequest commit", err)
		return
	}
	q, err := s.fetchRequest(r.Context(), id)
	if err != nil {
		serverError(w, "createRequest fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, q)
}

// decideRequest porta una richiesta pending verso approved o rejected.
func (s *Server) decideRequest(w http.ResponseWriter, r *http.Request, to, action string) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in DecisionInput
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	if runeLen(in.Note) > 500 {
		writeValidation(w, "note supera 500 caratteri")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, action+" begin", err)
		return
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(r.Context(),
		`SELECT status FROM dbo.marketplace_request WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "richiesta")
		return
	}
	if err != nil {
		serverError(w, action+" read", err)
		return
	}
	if current != "pending" {
		writeInvalidState(w, "transizione non consentita: "+action+" da "+current)
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.marketplace_request SET status = @p1, decided_by = @p4, decided_at = SYSUTCDATETIME(),
		        decision_note = @p2 WHERE id = @p3`, to, nullable(in.Note), id, actorFrom(r.Context())); err != nil {
		serverError(w, action+" update", err)
		return
	}
	if err := writeAudit(r.Context(), tx, action, "marketplace_request", id,
		auditDetail("request_"+to, nil)); err != nil {
		serverError(w, action+" audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, action+" commit", err)
		return
	}
	q, err := s.fetchRequest(r.Context(), id)
	if err != nil {
		serverError(w, action+" fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

// approveRequest: POST /api/marketplace/requests/{id}/approve
func (s *Server) approveRequest(w http.ResponseWriter, r *http.Request) {
	s.decideRequest(w, r, "approved", "approve")
}

// rejectRequest: POST /api/marketplace/requests/{id}/reject
func (s *Server) rejectRequest(w http.ResponseWriter, r *http.Request) {
	s.decideRequest(w, r, "rejected", "reject")
}

// provisionRequest: POST /api/marketplace/requests/{id}/provision. Solo da approved; una sola volta.
// Crea l'agente con origine marketplace, stato registered, fornitore, versione, framework e
// costo mensile dal prezzo se il modello è subscription. Risponde 201 con l'agente.
func (s *Server) provisionRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "provision begin", err)
		return
	}
	defer tx.Rollback()

	var status string
	var listingID int
	err = tx.QueryRowContext(r.Context(),
		`SELECT status, listing_id FROM dbo.marketplace_request WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`, id).
		Scan(&status, &listingID)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "richiesta")
		return
	}
	if err != nil {
		serverError(w, "provision read", err)
		return
	}
	if status != "approved" {
		writeInvalidState(w, "provisioning non consentito da "+status)
		return
	}

	var l Listing
	var ver, fw sql.NullString
	err = tx.QueryRowContext(r.Context(),
		`SELECT id, provider, name, version, framework, price_model, price, currency, certified, security_review, status
		   FROM dbo.marketplace_listing WHERE id = @p1`, listingID).
		Scan(&l.ID, &l.Provider, &l.Name, &ver, &fw, &l.PriceModel, &l.Price, &l.Currency, &l.Certified,
			&l.SecurityReview, &l.Status)
	if err != nil {
		serverError(w, "provision listing", err)
		return
	}
	l.Version = ptrOf(ver)
	l.Framework = ptrOf(fw)

	cost := 0.0
	if l.PriceModel == "subscription" {
		cost = l.Price
	}
	var agentID int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.agent (name, origin, vendor, version, status, framework, cost_monthly, currency)
		 OUTPUT inserted.id VALUES (@p1, 'marketplace', @p2, @p3, 'registered', @p4, @p5, @p6)`,
		l.Name, l.Provider, nullable(ptrStr(l.Version)), nullable(ptrStr(l.Framework)), cost, l.Currency).Scan(&agentID)
	if err != nil {
		writeDBError(w, "provision agent", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.marketplace_request SET status = 'provisioned', agent_id = @p1, provisioned_at = SYSUTCDATETIME()
		  WHERE id = @p2`, agentID, id); err != nil {
		serverError(w, "provision request", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "agent", agentID,
		auditDetail("request_provisioned", map[string]any{"request_id": id, "name": l.Name})); err != nil {
		serverError(w, "provision audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "provision commit", err)
		return
	}
	a, err := s.fetchAgent(r.Context(), agentID)
	if err != nil {
		serverError(w, "provision fetch", err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// ptrStr restituisce il valore del puntatore, o "" se nil.
func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
