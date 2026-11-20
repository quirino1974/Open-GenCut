package main

// Compatibilità SQL: il backend scrive le query in dialetto T-SQL (SQL Server); il database
// è MariaDB. Questo file contiene il driver che traduce le query al volo, così le chiamate
// nel resto del backend restano invariate.
//
// Traduzioni (vedi translateSQL):
//   @pN                       -> ?  (gli argomenti vengono riordinati, anche se ripetuti)
//   [nome]                    -> `nome`
//   N'testo'                  -> 'testo'
//   dbo.                      -> (rimosso: su MariaDB non c'è lo schema dbo)
//   SYSUTCDATETIME(), GETUTCDATE()   -> UTC_TIMESTAMP(3)
//   ISNULL(a, b)              -> COALESCE(a, b)
//   DATEADD(unità, n, data)   -> DATE_ADD(data, INTERVAL (n) unità)
//   CAST(x AS NVARCHAR(n))    -> CAST(x AS CHAR(n)); AS FLOAT -> DOUBLE; AS BIGINT -> SIGNED
//   CONVERT(CHAR(10), d, 23)  -> DATE_FORMAT(d, '%Y-%m-%d')
//   SELECT TOP (n) ...        -> SELECT ... LIMIT n
//   WITH (UPDLOCK, ...)       -> FOR UPDATE (in fondo alla query)
//   INSERT ... OUTPUT inserted.c VALUES (...) -> INSERT ... VALUES (...) RETURNING c
//
// Limite dichiarato: il confronto dei testi è case-insensitive e insensibile agli accenti
// (utf8mb4_unicode_ci), mentre SQL Server usava Latin1_General_CI_AS (sensibile agli accenti).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
)

const driverName = "gencut-mariadb"

func init() {
	sql.Register(driverName, shimDriver{inner: &mysql.MySQLDriver{}})
}

// shimDriver avvolge il driver MySQL/MariaDB e traduce le query nelle connessioni.
type shimDriver struct{ inner driver.Driver }

func (d shimDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &shimConn{Conn: c}, nil
}

type shimConn struct{ driver.Conn }

func (c *shimConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *shimConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	q, order, err := translateSQL(query)
	if err != nil {
		return nil, err
	}
	var st driver.Stmt
	if cp, ok := c.Conn.(driver.ConnPrepareContext); ok {
		st, err = cp.PrepareContext(ctx, q)
	} else {
		st, err = c.Conn.Prepare(q)
	}
	if err != nil {
		return nil, err
	}
	return &shimStmt{Stmt: st, order: order}, nil
}

func (c *shimConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if cb, ok := c.Conn.(driver.ConnBeginTx); ok {
		return cb.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func (c *shimConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *shimConn) CheckNamedValue(nv *driver.NamedValue) error {
	if ch, ok := c.Conn.(driver.NamedValueChecker); ok {
		return ch.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func (c *shimConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *shimConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// shimStmt riordina gli argomenti secondo l'ordine dei segnaposto della query tradotta.
type shimStmt struct {
	driver.Stmt
	order []int // per ogni ? della query tradotta: numero (1-based) dell'argomento originale
}

// NumInput è -1: la query tradotta può usare più volte lo stesso argomento.
func (s *shimStmt) NumInput() int { return -1 }

func (s *shimStmt) values(args []driver.Value) ([]driver.Value, error) {
	out := make([]driver.Value, len(s.order))
	for i, n := range s.order {
		if n > len(args) {
			return nil, fmt.Errorf("segnaposto @p%d senza argomento (%d ricevuti)", n, len(args))
		}
		out[i] = args[n-1]
	}
	return out, nil
}

func (s *shimStmt) named(args []driver.NamedValue) ([]driver.NamedValue, error) {
	out := make([]driver.NamedValue, len(s.order))
	for i, n := range s.order {
		if n > len(args) {
			return nil, fmt.Errorf("segnaposto @p%d senza argomento (%d ricevuti)", n, len(args))
		}
		out[i] = args[n-1]
		out[i].Ordinal = i + 1
	}
	return out, nil
}

func (s *shimStmt) Exec(args []driver.Value) (driver.Result, error) {
	v, err := s.values(args)
	if err != nil {
		return nil, err
	}
	return s.Stmt.Exec(v) //nolint:staticcheck
}

func (s *shimStmt) Query(args []driver.Value) (driver.Rows, error) {
	v, err := s.values(args)
	if err != nil {
		return nil, err
	}
	return s.Stmt.Query(v) //nolint:staticcheck
}

func (s *shimStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	n, err := s.named(args)
	if err != nil {
		return nil, err
	}
	if sc, ok := s.Stmt.(driver.StmtExecContext); ok {
		return sc.ExecContext(ctx, n)
	}
	return s.Stmt.Exec(namedToValues(n))
}

func (s *shimStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	n, err := s.named(args)
	if err != nil {
		return nil, err
	}
	if sq, ok := s.Stmt.(driver.StmtQueryContext); ok {
		return sq.QueryContext(ctx, n)
	}
	return s.Stmt.Query(namedToValues(n))
}

func namedToValues(n []driver.NamedValue) []driver.Value {
	v := make([]driver.Value, len(n))
	for i := range n {
		v[i] = n[i].Value
	}
	return v
}

// ---------------------------------------------------------------------------
// Traduzione del testo

var (
	reBracketIdent = regexp.MustCompile(`\[([A-Za-z_][A-Za-z0-9_]*)\]`)
	reNLiteral     = regexp.MustCompile(`(^|[^A-Za-z0-9_])N'`)
	reDbo          = regexp.MustCompile(`\bdbo\.`)
	reUTCNow       = regexp.MustCompile(`\b(?:SYSUTCDATETIME|GETUTCDATE)\(\)`)
	reISNULL       = regexp.MustCompile(`\bISNULL\(`)
	reCastNVarchar = regexp.MustCompile(`\bAS NVARCHAR\((\w+)\)`)
	reCastFloat    = regexp.MustCompile(`\bAS FLOAT\)`)
	reCastBigint   = regexp.MustCompile(`\bAS BIGINT\)`)
	reConvertDate  = regexp.MustCompile(`CONVERT\(CHAR\(10\),\s*([^,()]+),\s*23\)`)
	reTop          = regexp.MustCompile(`(?i)\bTOP\s*\(\s*(\d+|@p\d+)\s*\)|\bTOP\s+(\d+)\b`)
	reWithLock     = regexp.MustCompile(`(?i)\s*WITH\s*\(\s*((?:UPDLOCK|ROWLOCK|HOLDLOCK|NOLOCK)(?:\s*,\s*(?:UPDLOCK|ROWLOCK|HOLDLOCK|NOLOCK))*)\s*\)`)
	reOutput       = regexp.MustCompile(`(?is)\bOUTPUT\s+((?:inserted\.\w+\s*,?\s*)+)`)
	reInsertedCol  = regexp.MustCompile(`inserted\.(\w+)`)
	rePlaceholder  = regexp.MustCompile(`@p(\d+)`)
	reTrailingSemi = regexp.MustCompile(`;\s*$`)
)

// translateSQL traduce una query T-SQL in SQL MariaDB e restituisce l'ordine degli argomenti.
func translateSQL(q string) (string, []int, error) {
	s := q
	s = reBracketIdent.ReplaceAllString(s, "`$1`")
	s = reNLiteral.ReplaceAllString(s, "${1}'")
	s = reDbo.ReplaceAllString(s, "")
	s = reUTCNow.ReplaceAllString(s, "UTC_TIMESTAMP(3)")
	s = reISNULL.ReplaceAllString(s, "COALESCE(")
	s = reCastNVarchar.ReplaceAllString(s, "AS CHAR($1)")
	s = reCastFloat.ReplaceAllString(s, "AS DOUBLE)")
	s = reCastBigint.ReplaceAllString(s, "AS SIGNED)")
	s = reConvertDate.ReplaceAllString(s, "DATE_FORMAT($1, '%Y-%m-%d')")

	s, err := rewriteDateAdd(s)
	if err != nil {
		return "", nil, err
	}

	// TOP (n): diventa LIMIT alla fine della query
	limit := ""
	if m := reTop.FindStringSubmatch(s); m != nil {
		if m[1] != "" {
			limit = m[1]
		} else {
			limit = m[2]
		}
		s = reTop.ReplaceAllString(s, "")
	}

	// hint di blocco: diventano FOR UPDATE alla fine della query
	forUpdate := false
	if reWithLock.MatchString(s) {
		for _, m := range reWithLock.FindAllStringSubmatch(s, -1) {
			if strings.Contains(strings.ToUpper(m[1]), "UPDLOCK") || strings.Contains(strings.ToUpper(m[1]), "HOLDLOCK") {
				forUpdate = true
			}
		}
		s = reWithLock.ReplaceAllString(s, "")
	}

	// OUTPUT inserted.c: diventa RETURNING c alla fine dell'istruzione
	returning := ""
	if m := reOutput.FindStringSubmatch(s); m != nil {
		var cols []string
		for _, c := range reInsertedCol.FindAllStringSubmatch(m[1], -1) {
			cols = append(cols, c[1])
		}
		returning = strings.Join(cols, ", ")
		s = reOutput.ReplaceAllString(s, "")
	}

	s = strings.TrimSpace(s)
	s = reTrailingSemi.ReplaceAllString(s, "")
	if limit != "" {
		s += " LIMIT " + limit
	}
	if forUpdate {
		s += " FOR UPDATE"
	}
	if returning != "" {
		s += " RETURNING " + returning
	}

	// segnaposto @pN -> ?, con l'ordine degli argomenti
	var order []int
	var b strings.Builder
	last := 0
	for _, idx := range rePlaceholder.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:idx[0]])
		b.WriteByte('?')
		n, err := strconv.Atoi(s[idx[2]:idx[3]])
		if err != nil {
			return "", nil, err
		}
		order = append(order, n)
		last = idx[1]
	}
	b.WriteString(s[last:])
	return b.String(), order, nil
}

// rewriteDateAdd trasforma DATEADD(unità, n, data) in DATE_ADD(data, INTERVAL (n) unità),
// gestendo le parentesi annidate negli argomenti.
func rewriteDateAdd(s string) (string, error) {
	const key = "DATEADD("
	for {
		i := strings.Index(strings.ToUpper(s), key)
		if i < 0 {
			return s, nil
		}
		open := i + len(key) - 1
		end, err := matchParen(s, open)
		if err != nil {
			return "", err
		}
		args := splitTopLevel(s[open+1 : end])
		if len(args) != 3 {
			return "", fmt.Errorf("DATEADD con %d argomenti", len(args))
		}
		unit := strings.ToUpper(strings.TrimSpace(args[0]))
		repl := fmt.Sprintf("DATE_ADD(%s, INTERVAL (%s) %s)", strings.TrimSpace(args[2]), strings.TrimSpace(args[1]), unit)
		s = s[:i] + repl + s[end+1:]
	}
}

// matchParen restituisce la posizione della parentesi di chiusura corrispondente a s[open].
func matchParen(s string, open int) (int, error) {
	depth, quoted := 0, false
	for i := open; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, errors.New("parentesi non bilanciate nella query")
}

// splitTopLevel divide su ',' fuori da parentesi e stringhe.
func splitTopLevel(s string) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// ---------------------------------------------------------------------------
// Errori e blocco applicativo

// dbErrNumber restituisce il codice d'errore nella numerazione usata dal backend
// (2627 unicità, 547 vincoli di integrità e dati non validi), oppure 0 se non è un errore di MariaDB.
func dbErrNumber(err error) int32 {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return 0
	}
	switch me.Number {
	case 1062: // ER_DUP_ENTRY
		return 2627
	case 1451, 1452, 1048, 1364, 1366, 1406, 3819, 4025, 1264, 1265, 1292, 1411: // FK, NOT NULL, CHECK, valore fuori intervallo o non valido
		return 547
	}
	return int32(me.Number)
}

// lockKey prende un blocco per la durata della transazione: una riga di app_lock bloccata con
// FOR UPDATE, che si rilascia al commit o al rollback come il vecchio sp_getapplock.
// La riga si crea solo se manca.
func lockKey(ctx context.Context, tx *sql.Tx, name string) error {
	var got string
	err := tx.QueryRowContext(ctx, `SELECT name FROM app_lock WHERE name = @p1 FOR UPDATE`, name).Scan(&got)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// ON DUPLICATE KEY UPDATE: prende subito il lock esclusivo sulla riga, cosi' le inserzioni
	// concorrenti si mettono in coda invece di andare in deadlock con INSERT IGNORE.
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_lock (name) VALUES (@p1) ON DUPLICATE KEY UPDATE name = name`, name); err != nil {
		return err
	}
	return tx.QueryRowContext(ctx, `SELECT name FROM app_lock WHERE name = @p1 FOR UPDATE`, name).Scan(&got)
}

// pairLock blocca le decisioni sulla stessa coppia agente-risorsa: su SQL Server il vecchio
// UPDLOCK bloccava anche l'intervallo vuoto; qui serve un blocco esplicito.
func pairLock(ctx context.Context, tx *sql.Tx, agentID, resourceID int) error {
	return lockKey(ctx, tx, "opengencut_approval_"+strconv.Itoa(agentID)+"_"+strconv.Itoa(resourceID))
}
