// Open-GenCut - Sicurezza: utenti, sessioni, ruoli e autorizzazione (docs/SICUREZZA.txt).
//
//   - Password: solo hash bcrypt. Nessuna password in chiaro in log, audit o risposte.
//   - Sessione: token casuale nel cookie gencut_session (HttpOnly, SameSite=Strict, 8 ore);
//     nel database solo il suo SHA-256.
//   - Tentativi: dopo 5 fallimenti in 10 minuti per uno username, il login è bloccato (429).
//   - Primo accesso: l'utente admin iniziale ha password "admin" e il cambio è obbligatorio.
//   - Ogni rotta /api richiede sessione, tranne login e health. Il ruolo è verificato dalla tabella
//     authRules: se nessuna regola corrisponde, l'accesso è negato (default deny).
//   - L'actor del tracciamento è lo username della sessione.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie    = "gencut_session"
	sessionTTL       = 8 * time.Hour
	maxFailedLogins  = 5
	loginWindow      = 10 * time.Minute
	minPasswordLen   = 8
	maxBcryptBytes   = 72
	initialAdminUser = "admin"
	initialAdminPass = "admin"
)

// Ruoli.
const (
	roleAdmin       = "admin"
	roleManager     = "manager"
	roleOperator    = "operator"
	roleAuditor     = "auditor"
	roleViewer      = "viewer"
	roleIntegration = "integration"
)

var (
	validRoles   = map[string]bool{roleAdmin: true, roleManager: true, roleOperator: true, roleAuditor: true, roleViewer: true, roleIntegration: true}
	usernameRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{3,50}$`)
	dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)
)

// Insiemi di ruoli riusabili nelle regole.
var (
	rolesRead    = []string{roleAdmin, roleManager, roleOperator, roleAuditor, roleViewer}
	rolesAudit   = []string{roleAdmin, roleManager, roleAuditor}
	rolesWrite   = []string{roleAdmin, roleManager, roleOperator}
	rolesManage  = []string{roleAdmin, roleManager}
	rolesAdmin   = []string{roleAdmin}
	rolesAllUser = []string{roleAdmin, roleManager, roleOperator, roleAuditor, roleViewer, roleIntegration}
	rolesCall    = []string{roleAdmin, roleManager, roleOperator, roleIntegration}
)

// authRule associa un metodo e un percorso (con {x} e * jolly) ai ruoli che possono usarlo.
type authRule struct {
	method  string
	pattern string
	roles   []string
}

// authRules è la tabella di autorizzazione. La prima regola che corrisponde decide.
// Le rotte del ruolo integration sono solo quelle di chiamata degli agenti.
var authRules = []authRule{
	// sessione
	{"GET", "/api/auth/me", rolesAllUser},
	{"POST", "/api/auth/logout", rolesAllUser},
	{"POST", "/api/auth/change-password", rolesAllUser},
	// gestione utenti
	{"*", "/api/users*", rolesAdmin},
	// tracciamento e verifica
	{"GET", "/api/audit*", rolesAudit},
	{"GET", "/api/integrations/access-log", rolesAudit},
	{"GET", "/api/guardrails/events", rolesAudit},
	// chiamate degli agenti (anche per il ruolo integration)
	{"POST", "/api/integrations/check", rolesCall},
	{"POST", "/api/lifecycle/agents/{id}/calls", rolesCall},
	{"POST", "/api/chat/complete", rolesCall},
	{"POST", "/api/evaluation/otlp", rolesCall},
	{"POST", "/api/guardrails/evaluate", rolesCall},
	// agenti
	{"GET", "/api/agents*", rolesRead},
	{"POST", "/api/agents", rolesWrite},
	{"POST", "/api/agents/import", rolesWrite},
	{"PUT", "/api/agents/{id}", rolesWrite},
	{"DELETE", "/api/agents/{id}", rolesManage},
	// configurazioni
	{"GET", "/api/configurations*", rolesRead},
	{"POST", "/api/configurations", rolesWrite},
	{"PUT", "/api/configurations/{id}", rolesWrite},
	{"DELETE", "/api/configurations/{id}", rolesManage},
	// metriche
	{"GET", "/api/metrics/*", rolesRead},
	// server MCP e accesso unificato
	{"POST", "/api/integrations/resources/{id}/tools", rolesManage},
	{"POST", "/api/integrations/resources/{id}/tools/sync", rolesManage},
	{"PUT", "/api/integrations/tools/{tool_id}", rolesManage},
	{"DELETE", "/api/integrations/tools/{tool_id}", rolesManage},
	{"GET", "/api/integrations/tool-allowlist", rolesRead},
	{"POST", "/api/integrations/tool-allowlist", rolesWrite},
	{"DELETE", "/api/integrations/tool-allowlist/{id}", rolesWrite},
	{"POST", "/api/gateway/call", rolesCall},
	// integrazioni
	{"GET", "/api/integrations/resources*", rolesRead},
	{"POST", "/api/integrations/resources", rolesWrite},
	{"PUT", "/api/integrations/resources/{id}", rolesWrite},
	{"DELETE", "/api/integrations/resources/{id}", rolesManage},
	{"GET", "/api/integrations/allowlist", rolesRead},
	{"POST", "/api/integrations/allowlist", rolesWrite},
	{"DELETE", "/api/integrations/allowlist/{id}", rolesWrite},
	{"GET", "/api/integrations/limits", rolesRead},
	{"POST", "/api/integrations/limits", rolesWrite},
	{"PUT", "/api/integrations/limits/{id}", rolesWrite},
	{"DELETE", "/api/integrations/limits/{id}", rolesWrite},
	{"GET", "/api/integrations/approvals*", rolesRead},
	{"POST", "/api/integrations/approvals", rolesWrite},
	{"POST", "/api/integrations/approvals/{id}/*", rolesManage},
	// ciclo di vita
	{"GET", "/api/lifecycle/*", rolesRead},
	{"POST", "/api/lifecycle/agents/{id}/versions", rolesWrite},
	{"POST", "/api/lifecycle/agents/{id}/deploy", rolesWrite},
	{"POST", "/api/lifecycle/agents/{id}/promote", rolesWrite},
	{"POST", "/api/lifecycle/agents/{id}/suspend", rolesWrite},
	{"POST", "/api/lifecycle/agents/{id}/reactivate", rolesWrite},
	{"POST", "/api/lifecycle/agents/{id}/retire", rolesManage},
	{"POST", "/api/lifecycle/agents/{id}/evaluations", rolesManage},
	// valutazione
	{"GET", "/api/evaluation/*", rolesRead},
	{"POST", "/api/evaluation/traces", rolesWrite},
	{"PUT", "/api/evaluation/agents/{id}/reference", rolesManage},
	{"POST", "/api/evaluation/deviations", rolesAudit},
	{"POST", "/api/evaluation/agents/{id}/judge", rolesManage},
	{"POST", "/api/evaluation/rules", rolesManage},
	{"PUT", "/api/evaluation/rules/{id}", rolesManage},
	{"DELETE", "/api/evaluation/rules/{id}", rolesManage},
	{"POST", "/api/evaluation/agents/{id}/scores", rolesManage},
	// marketplace
	{"GET", "/api/marketplace/*", rolesRead},
	{"POST", "/api/marketplace/listings", rolesManage},
	{"PUT", "/api/marketplace/listings/{id}", rolesManage},
	{"DELETE", "/api/marketplace/listings/{id}", rolesManage},
	{"POST", "/api/marketplace/requests", rolesWrite},
	{"POST", "/api/marketplace/requests/{id}/*", rolesManage},
	// finops
	{"GET", "/api/finops/*", rolesRead},
	{"POST", "/api/finops/outcomes", rolesWrite},
	// identità
	{"GET", "/api/identity/*", rolesRead},
	{"POST", "/api/identity/agents/{id}/revoke", rolesManage},
	// conformità
	{"GET", "/api/compliance/*", rolesRead},
	{"POST", "/api/compliance/*", rolesManage},
	{"PUT", "/api/compliance/*", rolesManage},
	{"DELETE", "/api/compliance/*", rolesManage},
	// guardrail
	{"GET", "/api/guardrails/summary", rolesRead},
	{"GET", "/api/guardrails*", rolesRead},
	{"POST", "/api/guardrails", rolesManage},
	{"PUT", "/api/guardrails/{id}", rolesManage},
	{"DELETE", "/api/guardrails/{id}", rolesManage},
}

// matchRule confronta un metodo e un percorso con una regola.
func matchRule(rule authRule, method, path string) bool {
	if rule.method != "*" && rule.method != method {
		return false
	}
	ps := strings.Split(strings.Trim(path, "/"), "/")
	rs := strings.Split(strings.Trim(rule.pattern, "/"), "/")
	for i, seg := range rs {
		if seg == "*" {
			return true // jolly finale: qualsiasi resto
		}
		if strings.HasSuffix(seg, "*") && i == len(rs)-1 {
			// prefisso finale, es. "agents*": il segmento base deve coincidere con quello del percorso
			base := strings.TrimSuffix(seg, "*")
			return i < len(ps) && strings.HasPrefix(ps[i], base)
		}
		if i >= len(ps) {
			return false
		}
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			continue
		}
		if seg != ps[i] {
			return false
		}
	}
	return len(ps) == len(rs)
}

// authorized dice se il ruolo può usare il metodo e il percorso. Default deny.
func authorized(role, method, path string) bool {
	for _, rule := range authRules {
		if matchRule(rule, method, path) {
			for _, r := range rule.roles {
				if r == role {
					return true
				}
			}
			return false
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Utilità
// ---------------------------------------------------------------------------

type actorKey struct{}

// withActor mette lo username nel contesto: il tracciamento lo usa come actor.
func withActor(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, actorKey{}, username)
}

// actorFrom restituisce l'actor della richiesta, o "sistema" fuori da una sessione.
func actorFrom(ctx context.Context) string {
	if v, ok := ctx.Value(actorKey{}).(string); ok && v != "" {
		return v
	}
	return "sistema"
}

// sessionUser è l'utente della sessione corrente.
type sessionUser struct {
	ID                 int
	Username           string
	Role               string
	MustChangePassword bool
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// secureCookie dice se il cookie deve avere l'attributo Secure: la richiesta arriva via HTTPS,
// direttamente o dal reverse proxy (X-Forwarded-Proto: https). Su HTTP il cookie resta senza Secure.
func secureCookie(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// currentUser legge la sessione dal cookie e restituisce l'utente attivo, o nil.
func (s *Server) currentUser(r *http.Request) *sessionUser {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	var u sessionUser
	var must bool
	err = s.db.QueryRowContext(r.Context(),
		`SELECT u.id, u.username, u.role, u.must_change_password
		   FROM dbo.login_session ss JOIN dbo.app_user u ON u.id = ss.user_id
		  WHERE ss.token_hash = @p1 AND ss.expires_at > SYSUTCDATETIME() AND u.active = 1`,
		hashToken(c.Value)).Scan(&u.ID, &u.Username, &u.Role, &must)
	if err != nil {
		return nil
	}
	u.MustChangePassword = must
	return &u
}

// ensureAdmin crea l'utente admin iniziale se non esistono utenti.
func ensureAdmin(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dbo.app_user`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(initialAdminPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO dbo.app_user (username, password_hash, role, active, must_change_password)
		 VALUES (@p1, @p2, 'admin', 1, 1)`, initialAdminUser, string(hash))
	return err
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// secure applica sessione, cambio password obbligatorio e autorizzazione.
// Le pagine HTML senza sessione vanno a /login.html.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/api/") {
			if p == "/login.html" || (!strings.HasSuffix(p, "/") && !strings.HasSuffix(p, ".html") && p != "/") {
				next.ServeHTTP(w, r) // asset statici pubblici (login.html, CSS, JS)
				return
			}
			if s.currentUser(r) == nil {
				http.Redirect(w, r, "/login.html", http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if (r.Method == http.MethodPost && p == "/api/auth/login") || (r.Method == http.MethodGet && p == "/api/health") {
			next.ServeHTTP(w, r)
			return
		}
		u := s.currentUser(r)
		if u == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "sessione non valida o scaduta")
			return
		}
		if u.MustChangePassword && !authChangeAllowed(r.Method, p) {
			writeError(w, http.StatusForbidden, "password_change_required", "cambia la password prima di continuare")
			return
		}
		if !authorized(u.Role, r.Method, p) {
			writeError(w, http.StatusForbidden, "forbidden", "ruolo insufficiente per questa operazione")
			return
		}
		next.ServeHTTP(w, r.WithContext(withActor(r.Context(), u.Username)))
	})
}

// authChangeAllowed: con cambio password obbligatorio restano accessibili solo me, logout e cambio password.
func authChangeAllowed(method, path string) bool {
	return (method == http.MethodGet && path == "/api/auth/me") ||
		(method == http.MethodPost && path == "/api/auth/logout") ||
		(method == http.MethodPost && path == "/api/auth/change-password")
}

// ---------------------------------------------------------------------------
// Rotte di autenticazione
// ---------------------------------------------------------------------------

func (s *Server) registerAuth(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("GET /api/auth/me", s.me)
	mux.HandleFunc("POST /api/auth/change-password", s.changePassword)

	mux.HandleFunc("GET /api/users", s.listUsers)
	mux.HandleFunc("POST /api/users", s.createUser)
	mux.HandleFunc("PUT /api/users/{id}", s.updateUser)
	mux.HandleFunc("DELETE /api/users/{id}", s.deleteUser)
	mux.HandleFunc("POST /api/users/{id}/reset-password", s.resetPassword)
}

type loginInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// login: POST /api/auth/login. Risposta generica sugli errori; blocco dopo 5 fallimenti in 10 minuti.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in loginInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || in.Password == "" || runeLen(in.Username) > 50 || len(in.Password) > maxBcryptBytes {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "credenziali non valide")
		return
	}
	ctx := r.Context()

	var failed int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dbo.login_attempt WHERE username = @p1 AND success = 0 AND at >= DATEADD(MINUTE, -10, SYSUTCDATETIME())`,
		in.Username).Scan(&failed); err != nil {
		serverError(w, "login attempts", err)
		return
	}
	if failed >= maxFailedLogins {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "troppi tentativi: riprova tra qualche minuto")
		return
	}

	var id int
	var hash string
	var role string
	var active, must bool
	err := s.db.QueryRowContext(ctx,
		`SELECT id, password_hash, role, active, must_change_password FROM dbo.app_user WHERE username = @p1`,
		in.Username).Scan(&id, &hash, &role, &active, &must)
	found := err == nil
	if !found && !errors.Is(err, sql.ErrNoRows) {
		serverError(w, "login user", err)
		return
	}
	// Confronto sempre eseguito, anche per utenti inesistenti: tempi simili.
	cmpHash := hash
	if !found {
		cmpHash = string(dummyHash)
	}
	pwOK := bcrypt.CompareHashAndPassword([]byte(cmpHash), []byte(in.Password)) == nil
	success := found && active && pwOK

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		serverError(w, "login begin", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dbo.login_attempt (username, success) VALUES (@p1, @p2)`, in.Username, success); err != nil {
		serverError(w, "login attempt", err)
		return
	}
	if !success {
		if err := writeAudit(withActor(ctx, in.Username), tx, "create", "auth", 0,
			auditDetail("auth_failed", map[string]any{"username": in.Username})); err != nil {
			serverError(w, "login audit", err)
			return
		}
		if err := tx.Commit(); err != nil {
			serverError(w, "login commit", err)
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "credenziali non valide")
		return
	}

	token, err := randomToken(32)
	if err != nil {
		serverError(w, "login token", err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dbo.login_session (token_hash, user_id, expires_at) VALUES (@p1, @p2, DATEADD(SECOND, @p3, SYSUTCDATETIME()))`,
		hashToken(token), id, int(sessionTTL.Seconds())); err != nil {
		serverError(w, "login session", err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE dbo.app_user SET last_login_at = SYSUTCDATETIME() WHERE id = @p1`, id); err != nil {
		serverError(w, "login update", err)
		return
	}
	if err := writeAudit(withActor(ctx, in.Username), tx, "create", "auth", id,
		auditDetail("auth_login", nil)); err != nil {
		serverError(w, "login audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "login commit", err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: secureCookie(r), SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	writeJSON(w, http.StatusOK, map[string]any{"username": in.Username, "role": role, "must_change_password": must})
}

// logout: POST /api/auth/logout. Invalida la sessione. Risponde 204.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = s.db.ExecContext(r.Context(), `DELETE FROM dbo.login_session WHERE token_hash = @p1`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: secureCookie(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// me: GET /api/auth/me
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sessione non valida o scaduta")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": u.Username, "role": u.Role,
		"must_change_password": u.MustChangePassword})
}

// validatePassword applica la politica: minimo 8 caratteri, diversa dal nome utente, al massimo 72 byte.
func validatePassword(password, username string) string {
	if len(password) < minPasswordLen {
		return "la password deve avere almeno 8 caratteri"
	}
	if len(password) > maxBcryptBytes {
		return "la password supera 72 byte"
	}
	if strings.EqualFold(password, username) {
		return "la password non può coincidere con il nome utente"
	}
	return ""
}

// changePassword: POST /api/auth/change-password {old_password, new_password}. Risponde 204.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sessione non valida o scaduta")
		return
	}
	var in struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if msg := validatePassword(in.NewPassword, u.Username); msg != "" {
		writeValidation(w, msg)
		return
	}
	var hash string
	if err := s.db.QueryRowContext(r.Context(), `SELECT password_hash FROM dbo.app_user WHERE id = @p1`, u.ID).Scan(&hash); err != nil {
		serverError(w, "changePassword read", err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.OldPassword)) != nil {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "password attuale non corretta")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(in.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		serverError(w, "changePassword hash", err)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "changePassword begin", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.app_user SET password_hash = @p1, must_change_password = 0 WHERE id = @p2`,
		string(newHash), u.ID); err != nil {
		serverError(w, "changePassword update", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "app_user", u.ID,
		auditDetail("password_changed", map[string]any{"username": u.Username})); err != nil {
		serverError(w, "changePassword audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "changePassword commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Gestione utenti (solo admin)
// ---------------------------------------------------------------------------

// User è un utente senza dati segreti.
type User struct {
	ID                 int        `json:"id"`
	Username           string     `json:"username"`
	Role               string     `json:"role"`
	Active             bool       `json:"active"`
	MustChangePassword bool       `json:"must_change_password"`
	CreatedAt          time.Time  `json:"created_at"`
	LastLoginAt        *time.Time `json:"last_login_at"`
}

func scanUser(sc rowScanner) (User, error) {
	var u User
	var last sql.NullTime
	if err := sc.Scan(&u.ID, &u.Username, &u.Role, &u.Active, &u.MustChangePassword, &u.CreatedAt, &last); err != nil {
		return User{}, err
	}
	if last.Valid {
		t := last.Time
		u.LastLoginAt = &t
	}
	return u, nil
}

const userColumns = `id, username, role, active, must_change_password, created_at, last_login_at`

func (s *Server) fetchUser(ctx context.Context, id int) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM dbo.app_user WHERE id = @p1`, id))
}

// randomPassword genera una password temporanea: 16 caratteri casuali, mostrata una sola volta.
func randomPassword() (string, error) {
	return randomToken(12)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT `+userColumns+` FROM dbo.app_user ORDER BY username`)
	if err != nil {
		serverError(w, "listUsers", err)
		return
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			serverError(w, "listUsers scan", err)
			return
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		serverError(w, "listUsers rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createUser: POST /api/users {username, role, password?}. Senza password ne genera una temporanea.
// Ogni nuovo utente deve cambiare la password al primo accesso. Risponde 201.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if !usernameRe.MatchString(in.Username) {
		writeValidation(w, "username: da 3 a 50 caratteri, solo lettere, cifre, punto, trattino e underscore")
		return
	}
	if !validRoles[in.Role] {
		writeValidation(w, "ruolo non valido")
		return
	}
	temp := ""
	if in.Password == "" {
		p, err := randomPassword()
		if err != nil {
			serverError(w, "createUser random", err)
			return
		}
		temp = p
		in.Password = p
	}
	if msg := validatePassword(in.Password, in.Username); msg != "" {
		writeValidation(w, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		serverError(w, "createUser hash", err)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "createUser begin", err)
		return
	}
	defer tx.Rollback()
	var id int
	err = tx.QueryRowContext(r.Context(),
		`INSERT INTO dbo.app_user (username, password_hash, role, active, must_change_password)
		 OUTPUT inserted.id VALUES (@p1, @p2, @p3, 1, 1)`, in.Username, string(hash), in.Role).Scan(&id)
	if err != nil {
		writeDBError(w, "createUser insert", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "create", "app_user", id,
		auditDetail("user_created", map[string]any{"username": in.Username, "role": in.Role})); err != nil {
		serverError(w, "createUser audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "createUser commit", err)
		return
	}
	u, err := s.fetchUser(r.Context(), id)
	if err != nil {
		serverError(w, "createUser fetch", err)
		return
	}
	resp := map[string]any{"user": u}
	if temp != "" {
		resp["temporary_password"] = temp // mostrata una sola volta
	}
	writeJSON(w, http.StatusCreated, resp)
}

// activeAdminsExcept conta gli admin attivi diversi da id.
func (s *Server) activeAdminsExcept(ctx context.Context, id int) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dbo.app_user WHERE role = 'admin' AND active = 1 AND id <> @p1`, id).Scan(&n)
	return n, err
}

// updateUser: PUT /api/users/{id} {role, active}. Non si può togliere l'ultimo admin attivo, né demansionare se stessi.
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Role   string `json:"role"`
		Active bool   `json:"active"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !validRoles[in.Role] {
		writeValidation(w, "ruolo non valido")
		return
	}
	self := s.isSelf(r, id)
	target, err := s.fetchUser(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "utente")
		return
	}
	if err != nil {
		serverError(w, "updateUser read", err)
		return
	}
	losesAdmin := target.Role == roleAdmin && (in.Role != roleAdmin || !in.Active)
	if losesAdmin {
		n, err := s.activeAdminsExcept(r.Context(), id)
		if err != nil {
			serverError(w, "updateUser admins", err)
			return
		}
		if n == 0 {
			writeInvalidState(w, "non si può togliere l'ultimo amministratore attivo")
			return
		}
	}
	if self && (in.Role != roleAdmin || !in.Active) {
		writeInvalidState(w, "non si può disattivare o demansionare il proprio account")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "updateUser begin", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.app_user SET role = @p1, active = @p2 WHERE id = @p3`, in.Role, in.Active, id); err != nil {
		serverError(w, "updateUser update", err)
		return
	}
	if !in.Active {
		if _, err := tx.ExecContext(r.Context(),
			`DELETE FROM dbo.login_session WHERE user_id = @p1`, id); err != nil {
			serverError(w, "updateUser sessions", err)
			return
		}
	}
	if err := writeAudit(r.Context(), tx, "update", "app_user", id,
		auditDetail("user_updated", map[string]any{"username": target.Username, "role": in.Role, "active": in.Active})); err != nil {
		serverError(w, "updateUser audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "updateUser commit", err)
		return
	}
	u, err := s.fetchUser(r.Context(), id)
	if err != nil {
		serverError(w, "updateUser fetch", err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// isSelf dice se l'id è quello dell'utente della richiesta.
func (s *Server) isSelf(r *http.Request, id int) bool {
	u := s.currentUser(r)
	return u != nil && u.ID == id
}

// deleteUser: DELETE /api/users/{id}. Non si cancella se stessi né l'ultimo admin attivo. Risponde 204.
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if s.isSelf(r, id) {
		writeInvalidState(w, "non si può cancellare il proprio account")
		return
	}
	target, err := s.fetchUser(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "utente")
		return
	}
	if err != nil {
		serverError(w, "deleteUser read", err)
		return
	}
	if target.Role == roleAdmin && target.Active {
		n, err := s.activeAdminsExcept(r.Context(), id)
		if err != nil {
			serverError(w, "deleteUser admins", err)
			return
		}
		if n == 0 {
			writeInvalidState(w, "non si può cancellare l'ultimo amministratore attivo")
			return
		}
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "deleteUser begin", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.app_user WHERE id = @p1`, id); err != nil {
		serverError(w, "deleteUser delete", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "delete", "app_user", id,
		auditDetail("user_deleted", map[string]any{"username": target.Username})); err != nil {
		serverError(w, "deleteUser audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "deleteUser commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resetPassword: POST /api/users/{id}/reset-password. Password temporanea, mostrata una volta; cambio obbligatorio.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	target, err := s.fetchUser(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(w, "utente")
		return
	}
	if err != nil {
		serverError(w, "resetPassword read", err)
		return
	}
	temp, err := randomPassword()
	if err != nil {
		serverError(w, "resetPassword random", err)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(temp), bcrypt.DefaultCost)
	if err != nil {
		serverError(w, "resetPassword hash", err)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		serverError(w, "resetPassword begin", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE dbo.app_user SET password_hash = @p1, must_change_password = 1 WHERE id = @p2`, string(hash), id); err != nil {
		serverError(w, "resetPassword update", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM dbo.login_session WHERE user_id = @p1`, id); err != nil {
		serverError(w, "resetPassword sessions", err)
		return
	}
	if err := writeAudit(r.Context(), tx, "update", "app_user", id,
		auditDetail("password_reset", map[string]any{"username": target.Username})); err != nil {
		serverError(w, "resetPassword audit", err)
		return
	}
	if err := tx.Commit(); err != nil {
		serverError(w, "resetPassword commit", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": target.Username, "temporary_password": temp})
}
