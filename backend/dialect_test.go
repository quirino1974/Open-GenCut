package main

import (
	"reflect"
	"testing"
)

// Query reali del backend (T-SQL) e la traduzione MariaDB attesa.
func TestTranslateSQL(t *testing.T) {
	cases := []struct {
		name, in, want string
		order          []int
	}{
		{
			name:  "segnaposto fuori ordine e ripetuti",
			in:    `SELECT 1 FROM dbo.agent WHERE (@p2 = 0 OR agent_id = @p2) AND (@p1 = N'' OR status = @p1)`,
			want:  `SELECT 1 FROM agent WHERE (? = 0 OR agent_id = ?) AND (? = '' OR status = ?)`,
			order: []int{2, 2, 1, 1},
		},
		{
			name:  "DATEADD annidato con SYSUTCDATETIME",
			in:    `SELECT COUNT(*) FROM dbo.login_attempt WHERE username = @p1 AND success = 0 AND at >= DATEADD(MINUTE, -10, SYSUTCDATETIME())`,
			want:  `SELECT COUNT(*) FROM login_attempt WHERE username = ? AND success = 0 AND at >= DATE_ADD(UTC_TIMESTAMP(3), INTERVAL (-10) MINUTE)`,
			order: []int{1},
		},
		{
			name:  "DATEADD con segnaposto e CAST",
			in:    `WHERE called_at < DATEADD(DAY, 1, CAST(@p4 AS DATE)))`,
			want:  `WHERE called_at < DATE_ADD(CAST(? AS DATE), INTERVAL (1) DAY))`,
			order: []int{4},
		},
		{
			name:  "TOP con hint di blocco",
			in:    "SELECT TOP (1) id FROM dbo.integration_approval WITH (UPDLOCK, ROWLOCK)\n\t\t\t WHERE agent_id = @p1 ORDER BY id",
			want:  "SELECT  id FROM integration_approval\n\t\t\t WHERE agent_id = ? ORDER BY id LIMIT 1 FOR UPDATE",
			order: []int{1},
		},
		{
			name:  "TOP con parametro",
			in:    `SELECT TOP (@p1) id FROM dbo.x WHERE a = @p2`,
			want:  `SELECT  id FROM x WHERE a = ? LIMIT ?`,
			order: []int{2, 1},
		},
		{
			name:  "lock su conteggio",
			in:    `SELECT COUNT(*) FROM dbo.integration_approval WITH (UPDLOCK, ROWLOCK)\n WHERE agent_id = @p1`,
			want:  `SELECT COUNT(*) FROM integration_approval\n WHERE agent_id = ? FOR UPDATE`,
			order: []int{1},
		},
		{
			name:  "OUTPUT inserted su piu' colonne",
			in:    "INSERT INTO dbo.agent_deployment (agent_id, version_label)\n\t\t OUTPUT inserted.id, inserted.agent_id, inserted.at\n\t\t VALUES (@p1, @p2)",
			want:  "INSERT INTO agent_deployment (agent_id, version_label)\n\t\t VALUES (?, ?) RETURNING id, agent_id, at",
			order: []int{1, 2},
		},
		{
			name:  "OUTPUT inserted id semplice",
			in:    "INSERT INTO dbo.eval_rule (type, params, enabled) OUTPUT inserted.id VALUES (@p1, @p2, @p3)",
			want:  "INSERT INTO eval_rule (type, params, enabled) VALUES (?, ?, ?) RETURNING id",
			order: []int{1, 2, 3},
		},
		{
			name:  "ISNULL, CAST e CONVERT",
			in:    `SELECT kind, ISNULL(endpoint, N'') FROM dbo.integration_resource WHERE id = @p1`,
			want:  `SELECT kind, COALESCE(endpoint, '') FROM integration_resource WHERE id = ?`,
			order: []int{1},
		},
		{
			name:  "identificatori tra parentesi quadre e CONVERT",
			in:    "SELECT [key], CONVERT(CHAR(10), called_at, 23) AS day FROM dbo.usage_log WHERE a = @p1",
			want:  "SELECT `key`, DATE_FORMAT(called_at, '%Y-%m-%d') AS day FROM usage_log WHERE a = ?",
			order: []int{1},
		},
		{
			name:  "CAST di tipi",
			in:    `SELECT CAST(identity_id AS NVARCHAR(36)), AVG(CAST(score AS FLOAT)), SUM(CAST(tokens AS BIGINT)) FROM dbo.agent`,
			want:  `SELECT CAST(identity_id AS CHAR(36)), AVG(CAST(score AS DOUBLE)), SUM(CAST(tokens AS SIGNED)) FROM agent`,
			order: nil,
		},
		{
			name:  "punto e virgola finale",
			in:    `UPDATE dbo.agent SET name = @p1 WHERE id = @p2;`,
			want:  `UPDATE agent SET name = ? WHERE id = ?`,
			order: []int{1, 2},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, order, err := translateSQL(c.in)
			if err != nil {
				t.Fatalf("errore: %v", err)
			}
			if got != c.want {
				t.Errorf("query\n got: %q\nwant: %q", got, c.want)
			}
			if !reflect.DeepEqual(order, c.order) {
				t.Errorf("ordine: got %v want %v", order, c.order)
			}
		})
	}
}
