package elf

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type joinPart struct {
	sql  string
	args []any
}

type query struct {
	table     string
	selecteds []string
	joins     []joinPart
	where     []Cond // top-level conditions, joined with and
	orderBy   []string
	limit     int // -1 means "not set"
	offset    int // -1 means "not set"
	err       error

	execer Execer
}

// Select sets the columns to return. With no columns, SELECT * is used.
//
// When you join, list the columns explicitly and alias the ones whose names
// collide, because the result is mapped by column name:
//
//	Select("users.id", "users.name", "ad.id AS address_id", "ad.city")
func (q *query) Select(columns ...string) *query {
	q.selecteds = append(q.selecteds, columns...)

	return q
}

// Join adds a raw join clause. "?" placeholders are rewritten to $n and use
// the given arguments ("??" is a literal "?").
//
//	Join("LEFT JOIN addresses ad ON ad.user_id = users.id")
//	Join("LEFT JOIN addresses ad ON ad.user_id = users.id AND ad.active = ?", true)
//
// Joins are written in call order, after FROM and before WHERE.
func (q *query) Join(clause string, args ...any) *query {
	if strings.TrimSpace(clause) == "" {
		q.setErr(errors.New("elephant: empty join clause"))

		return q
	}

	q.joins = append(q.joins, joinPart{sql: clause, args: args})

	return q
}

// Where adds a condition, joined to previous ones with AND. The first
// argument is either:
//
//   - a string with "?" placeholders, followed by its arguments:
//     Where("age > ?", 18)
//   - a Cond built with C, And, Or or Not (no extra arguments):
//     Where(Or(C("a = ?", 1), C("b = ?", 2)))
func (q *query) Where(cond any, args ...any) *query {
	c, err := toCond(cond, args)
	if err != nil {
		q.setErr(err)

		return q
	}

	q.where = append(q.where, c)

	return q
}

// And is an alias of Where that reads better in a chain.
func (q *query) And(cond any, args ...any) *query {
	return q.Where(cond, args...)
}

// OrderBy appends ORDER BY expressions, e.g. OrderBy("created_at DESC").
func (q *query) OrderBy(exprs ...string) *query {
	q.orderBy = append(q.orderBy, exprs...)

	return q
}

// Limit sets the LIMIT value.
func (q *query) Limit(n int) *query {
	if n < 0 {
		q.setErr(fmt.Errorf("elephant: negative limit %d", n))

		return q
	}
	q.limit = n

	return q
}

// Offset sets the OFFSET value.
func (q *query) Offset(n int) *query {
	if n < 0 {
		q.setErr(fmt.Errorf("elephant: negative offset %d", n))

		return q
	}
	q.offset = n

	return q
}

// QueryRow runs the query and maps the first row to M.
// It returns pgx.ErrNoRows if there are no rows.
//
// It is a function and not a method because Go methods cannot have type
// parameters.
func (q *query) QueryRow[M any](ctx context.Context) (M, error) {
	rows, err := q.query(ctx)
	if err != nil {
		var zero M

		return zero, err
	}

	return pgx.CollectOneRow(rows, pgx.RowToStructByName[M])
}

// Query runs the query and maps every row to M.
func (q *query) Query[M any](ctx context.Context) ([]M, error) {
	rows, err := q.query(ctx)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, pgx.RowToStructByName[M])
}

// Build returns the final SQL and its arguments.
func (q *query) build() (string, []any, error) {
	if q.err != nil {
		return "", nil, q.err
	}

	var sb strings.Builder
	sb.Grow(128)              //nolint:mnd
	args := make([]any, 0, 8) //nolint:mnd

	sb.WriteString("SELECT ")
	if len(q.selecteds) == 0 {
		sb.WriteByte('*')
	} else {
		sb.WriteString(strings.Join(q.selecteds, ", "))
	}
	sb.WriteString(" FROM ")
	sb.WriteString(q.table)

	// Joins go before WHERE so $n numbering follows the argument order.
	for _, j := range q.joins {
		sb.WriteByte(' ')
		if err := writeSQL(&sb, &args, j.sql, j.args); err != nil {
			return "", nil, err
		}
	}

	// Top-level chain: AND-joined, without outer parentheses.
	top := group{op: "AND", conds: q.where}
	if !top.empty() {
		sb.WriteString(" WHERE ")
		if err := top.write(&sb, &args, false); err != nil {
			return "", nil, err
		}
	}

	if len(q.orderBy) > 0 {
		sb.WriteString(" ORDER BY ")
		sb.WriteString(strings.Join(q.orderBy, ", "))
	}

	if q.limit >= 0 {
		args = append(args, q.limit)
		sb.WriteString(" LIMIT $")
		sb.WriteString(strconv.Itoa(len(args)))
	}
	if q.offset >= 0 {
		args = append(args, q.offset)
		sb.WriteString(" OFFSET $")
		sb.WriteString(strconv.Itoa(len(args)))
	}

	return sb.String(), args, nil
}

// writeSQL appends sql to sb, rewriting each "?" to $n. The numbering comes
// from the length of args, so it always matches the argument order.
func writeSQL(sb *strings.Builder, args *[]any, sql string, vals []any) error {
	used := 0

	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if ch != '?' {
			sb.WriteByte(ch)

			continue
		}

		// "??" is an escaped literal "?".
		if i+1 < len(sql) && sql[i+1] == '?' {
			sb.WriteByte('?')
			i++

			continue
		}

		if used >= len(vals) {
			return fmt.Errorf("elephant: not enough arguments for %q", sql)
		}

		*args = append(*args, vals[used])
		used++

		sb.WriteByte('$')
		sb.WriteString(strconv.Itoa(len(*args)))
	}

	if used != len(vals) {
		return fmt.Errorf("elephant: too many arguments for %q", sql)
	}

	return nil
}

func (q *query) query(ctx context.Context) (pgx.Rows, error) {
	qr, args, err := q.build()
	if err != nil {
		return nil, err
	}

	return q.execer.Query(ctx, qr, args...)
}

// setErr records the first error; Build reports it.
func (q *query) setErr(err error) {
	if q.err == nil {
		q.err = err
	}
}

// toCond converts a Where argument into a Cond.
func toCond(v any, args []any) (Cond, error) {
	switch c := v.(type) {
	case string:
		return raw{sql: c, args: args}, nil
	case Cond:
		if len(args) > 0 {
			return nil, fmt.Errorf("elephant: extra arguments are not allowed with a Cond")
		}

		return c, nil
	default:
		return nil, fmt.Errorf("elephant: unsupported condition type %T", v)
	}
}
