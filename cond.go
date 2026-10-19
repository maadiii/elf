package elf

import (
	"fmt"
	"strconv"
	"strings"
)

// Cond is a node in the condition tree. Parentheses are generated from the
// tree structure, so callers never have to write them by hand.
type Cond interface {
	// build appends the SQL for this node to sb and its arguments to args.
	// Placeholders are numbered from len(*args)+1, so numbering always matches
	// the argument order regardless of nesting.
	build(sb *strings.Builder, args *[]any) error

	// empty reports whether the node would produce no SQL at all.
	empty() bool
}

// C creates a raw condition. Use "?" as the placeholder; it is rewritten to
// $1, $2, ... at build time. Use "??" to emit a literal "?" (for example the
// jsonb "?" operator).
//
//	C("age > ?", 18)
func C(sql string, args ...any) Cond { return raw{sql: sql, args: args} }

// And joins conditions with AND.
func And(conds ...Cond) Cond { return group{op: "AND", conds: conds} }

// Or joins conditions with OR.
func Or(conds ...Cond) Cond { return group{op: "OR", conds: conds} }

// Not negates a condition: NOT (...).
func Not(c Cond) Cond { return not{c: c} }

// In matches rows where col equals any of vals.
func In[T any](col string, vals []T) Cond {
	if vals == nil {
		// A nil slice would be sent as SQL NULL; use an empty array instead.
		vals = []T{}
	}

	return C(col+" = ANY(?)", vals)
}

// NotIn matches rows where col differs from every value in vals.
func NotIn[T any](col string, vals []T) Cond {
	if vals == nil {
		vals = []T{}
	}

	return C(col+" <> ALL(?)", vals)
}

// raw is a user-written SQL fragment with "?" placeholders.
type raw struct {
	sql  string
	args []any
}

func (r raw) empty() bool { return strings.TrimSpace(r.sql) == "" }

func (r raw) build(sb *strings.Builder, args *[]any) error {
	// A raw fragment containing a top-level-looking OR is wrapped so it can
	// never change the meaning of the AND/OR chain around it.
	wrap := containsOr(r.sql)
	if wrap {
		sb.WriteByte('(')
	}

	argIdx := 0
	for i := 0; i < len(r.sql); i++ {
		ch := r.sql[i]
		if ch != '?' {
			sb.WriteByte(ch)

			continue
		}

		// "??" is an escaped literal "?".
		if i+1 < len(r.sql) && r.sql[i+1] == '?' {
			sb.WriteByte('?')
			i++

			continue
		}

		if argIdx >= len(r.args) {
			return fmt.Errorf("elf: not enough arguments for %q", r.sql)
		}
		*args = append(*args, r.args[argIdx])
		argIdx++
		sb.WriteByte('$')
		sb.WriteString(strconv.Itoa(len(*args)))
	}

	if argIdx != len(r.args) {
		return fmt.Errorf("elf: too many arguments for %q", r.sql)
	}

	if wrap {
		sb.WriteByte(')')
	}

	return nil
}

// group combines several conditions with the same operator.
type group struct {
	op    string // "AND" or "OR"
	conds []Cond
}

func (g group) empty() bool {
	for _, c := range g.conds {
		if c != nil && !c.empty() {
			return false
		}
	}

	return true
}

func (g group) build(sb *strings.Builder, args *[]any) error {
	return g.write(sb, args, true)
}

// write renders the group. When parens is false the outer parentheses are
// omitted (used for the top-level WHERE chain).
func (g group) write(sb *strings.Builder, args *[]any, parens bool) error { //nolint:cyclop
	live := make([]Cond, 0, len(g.conds))
	for _, c := range g.conds {
		if c != nil && !c.empty() {
			live = append(live, c)
		}
	}

	switch len(live) {
	case 0:
		return nil
	case 1:
		// A single child needs no grouping.
		return live[0].build(sb, args)
	}

	if parens {
		sb.WriteByte('(')
	}
	for i, c := range live {
		if i > 0 {
			sb.WriteByte(' ')
			sb.WriteString(g.op)
			sb.WriteByte(' ')
		}
		if err := c.build(sb, args); err != nil {
			return err
		}
	}
	if parens {
		sb.WriteByte(')')
	}

	return nil
}

// not renders NOT (...).
type not struct{ c Cond }

func (n not) empty() bool { return n.c == nil || n.c.empty() }

func (n not) build(sb *strings.Builder, args *[]any) error {
	if n.empty() {
		return nil
	}
	sb.WriteString("NOT (")
	if err := n.c.build(sb, args); err != nil {
		return err
	}
	sb.WriteByte(')')

	return nil
}

// containsOr reports whether s contains the keyword OR as a standalone word
// (case-insensitive). It is a heuristic: a false positive only adds a
// harmless pair of parentheses.
func containsOr(s string) bool {
	for i := 0; i+i < len(s); i++ {
		if s[i]|0x20 != 'o' || s[i+1]|0x20 != 'r' {
			continue
		}
		if i > 0 && isIdentByte(s[i-1]) {
			continue
		}
		if i+2 < len(s) && isIdentByte(s[i+2]) {
			continue
		}

		return true
	}

	return false
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}
