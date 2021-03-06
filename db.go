package entity

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"

	"github.com/jmoiron/sqlx"
)

var (
	selectStatements = map[reflect.Type]string{}
	insertStatements = map[reflect.Type]string{}
	updateStatements = map[reflect.Type]string{}
	deleteStatements = map[reflect.Type]string{}

	driverMysql    = "mysql"
	driverPostgres = "postgres"
	driverSqlite3  = "sqlite3"

	driverAlias = map[string]string{
		"pgx": driverPostgres,
	}
)

// DB 数据库接口
// sqlx.DB 和 sqlx.Tx 公共方法
type DB interface {
	sqlx.Queryer
	sqlx.QueryerContext
	sqlx.Execer
	sqlx.ExecerContext
	Get(dest interface{}, query string, args ...interface{}) error
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	Select(dest interface{}, query string, args ...interface{}) error
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	NamedExec(query string, arg interface{}) (sql.Result, error)
	NamedExecContext(ctx context.Context, query string, arg interface{}) (sql.Result, error)
	NamedQuery(query string, arg interface{}) (*sqlx.Rows, error)
	DriverName() string
	Rebind(string) string
	BindNamed(string, interface{}) (string, []interface{}, error)
}

func dbDriver(db DB) string {
	dv := db.DriverName()
	if v, ok := driverAlias[dv]; ok {
		return v
	}
	return dv
}

func isConflictError(db DB, err error) bool {
	driver := dbDriver(db)

	s := err.Error()
	if driver == driverPostgres {
		return strings.Contains(s, "duplicate key value violates unique constraint")
	} else if driver == driverMysql {
		return strings.Contains(s, "Duplicate entry")
	} else if driver == driverSqlite3 {
		return strings.Contains(s, "UNIQUE constraint failed")
	}
	return false
}

func doLoad(ctx context.Context, ent Entity, db DB) error {
	md, err := getMetadata(ent)
	if err != nil {
		return fmt.Errorf("get metadata, %w", err)
	}

	stmt, ok := selectStatements[md.Type]
	if !ok {
		stmt = selectStatement(ent, md, dbDriver(db))
		selectStatements[md.Type] = stmt
	}

	rows, err := sqlx.NamedQueryContext(ctx, db, stmt, ent)
	if err != nil {
		return err
	}
	defer rows.Close()

	if !rows.Next() {
		return sql.ErrNoRows
	}

	if err := rows.StructScan(ent); err != nil {
		return fmt.Errorf("scan struct, %w", err)
	}

	return rows.Err()
}

func doInsert(ctx context.Context, ent Entity, db DB) (int64, error) {
	md, err := getMetadata(ent)
	if err != nil {
		return 0, fmt.Errorf("get metadata, %w", err)
	}

	stmt, ok := insertStatements[md.Type]
	if !ok {
		stmt = insertStatement(ent, md, dbDriver(db))
		insertStatements[md.Type] = stmt
	}

	if md.hasReturningInsert {
		rows, err := sqlx.NamedQueryContext(ctx, db, stmt, ent)
		if err != nil {
			return 0, err
		}
		defer rows.Close()

		if !rows.Next() {
			return 0, sql.ErrNoRows
		}

		if err := rows.StructScan(ent); err != nil {
			return 0, fmt.Errorf("scan struct, %w", err)
		}

		return 0, rows.Err()
	}

	result, err := db.NamedExecContext(ctx, stmt, ent)
	if err != nil {
		return 0, err
	}

	// postgresql不支持LastInsertId特性
	if dbDriver(db) == driverPostgres {
		return 0, nil
	}

	lastID, err := result.LastInsertId()
	return lastID, fmt.Errorf("get last insert id, %w", err)
}

func doUpdate(ctx context.Context, ent Entity, db DB) error {
	md, err := getMetadata(ent)
	if err != nil {
		return fmt.Errorf("get metadata, %w", err)
	}

	stmt, ok := updateStatements[md.Type]
	if !ok {
		stmt = updateStatement(ent, md, dbDriver(db))
		updateStatements[md.Type] = stmt
	}

	if md.hasReturningUpdate {
		rows, err := sqlx.NamedQueryContext(ctx, db, stmt, ent)
		if err != nil {
			return err
		}
		defer rows.Close()

		if !rows.Next() {
			return sql.ErrNoRows
		}

		if err := rows.StructScan(ent); err != nil {
			return fmt.Errorf("scan struct, %w", err)
		}

		return rows.Err()
	}

	result, err := db.NamedExecContext(ctx, stmt, ent)
	if err != nil {
		return err
	}

	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("get affected rows, %w", err)
	} else if n == 0 {
		return sql.ErrNoRows
	}

	return nil

}

func doDelete(ctx context.Context, ent Entity, db DB) error {
	md, err := getMetadata(ent)
	if err != nil {
		return fmt.Errorf("get metadata, %w", err)
	}

	stmt, ok := deleteStatements[md.Type]
	if !ok {
		stmt = deleteStatement(ent, md, dbDriver(db))
		deleteStatements[md.Type] = stmt
	}

	_, err = db.NamedExecContext(ctx, stmt, ent)
	return err
}

func selectStatement(ent Entity, md *Metadata, driver string) string {
	columns := []string{}
	for _, col := range md.Columns {
		columns = append(columns, quoteColumn(col.DBField, driver))
	}
	stmt := fmt.Sprintf("SELECT %s FROM %s WHERE", strings.Join(columns, ", "), quoteIdentifier(md.TableName, driver))

	for i, col := range md.PrimaryKeys {
		if i == 0 {
			stmt += fmt.Sprintf(" %s = :%s", quoteColumn(col.DBField, driver), col.DBField)
		} else {
			stmt += fmt.Sprintf(" AND %s = :%s", quoteColumn(col.DBField, driver), col.DBField)
		}
	}
	stmt += " LIMIT 1"

	return stmt
}

func insertStatement(ent Entity, md *Metadata, driver string) string {
	columns := []string{}
	returnings := []string{}
	placeholder := []string{}

	for _, col := range md.Columns {
		c := quoteColumn(col.DBField, driver)
		if col.ReturningInsert {
			returnings = append(returnings, c)
		} else if !col.AutoIncrement {
			columns = append(columns, c)
			placeholder = append(placeholder, fmt.Sprintf(":%s", col.DBField))
		}
	}

	stmt := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		quoteIdentifier(md.TableName, driver),
		strings.Join(columns, ", "),
		strings.Join(placeholder, ", "),
	)

	if len(returnings) > 0 {
		stmt += fmt.Sprintf(" RETURNING %s", strings.Join(returnings, ", "))
	}

	return stmt
}

func updateStatement(ent Entity, md *Metadata, driver string) string {
	returnings := []string{}
	stmt := fmt.Sprintf("UPDATE %s SET", quoteIdentifier(md.TableName, driver))

	set := false
	for _, col := range md.Columns {
		if col.ReturningUpdate {
			returnings = append(returnings, quoteColumn(col.DBField, driver))
		} else if !col.RefuseUpdate {
			if set {
				stmt += fmt.Sprintf(", %s = :%s", quoteColumn(col.DBField, driver), col.DBField)
			} else {
				stmt += fmt.Sprintf(" %s = :%s", quoteColumn(col.DBField, driver), col.DBField)
				set = true
			}
		}
	}

	for i, col := range md.PrimaryKeys {
		if i == 0 {
			stmt += fmt.Sprintf(" WHERE %s = :%s", quoteColumn(col.DBField, driver), col.DBField)
		} else {
			stmt += fmt.Sprintf(" AND %s = :%s", quoteColumn(col.DBField, driver), col.DBField)
		}
	}

	if len(returnings) > 0 {
		stmt += fmt.Sprintf(" RETURNING %s", strings.Join(returnings, ", "))
	}

	return stmt
}

func deleteStatement(ent Entity, md *Metadata, driver string) string {
	stmt := fmt.Sprintf("DELETE FROM %s WHERE", quoteIdentifier(md.TableName, driver))
	for i, col := range md.PrimaryKeys {
		if i == 0 {
			stmt += fmt.Sprintf(" %s = :%s", quoteColumn(col.DBField, driver), col.DBField)
		} else {
			stmt += fmt.Sprintf(" AND %s = :%s", quoteColumn(col.DBField, driver), col.DBField)
		}
	}

	return stmt
}

func quoteColumn(name string, driver string) string {
	if driver == driverMysql {
		return fmt.Sprintf("`%s`", name)
	}
	return fmt.Sprintf("%q", name)
}

func quoteIdentifier(name string, driver string) string {
	symbol := `"`
	if driver == driverMysql {
		symbol = "`"
	}

	result := []string{}
	name = strings.ReplaceAll(name, symbol, "")
	for _, s := range strings.Split(name, ".") {
		if s != "*" {
			s = fmt.Sprintf("%s%s%s", symbol, s, symbol)
		}
		result = append(result, s)
	}

	return strings.Join(result, ".")
}
