package sqlreview

// impact.go is the STATIC impact surface of a batch: which tables the
// statements name, what they do to them, and whether anything is left
// unbounded.
//
// Why it is static, and why that is stated rather than hidden: what an
// operator actually wants before approving a change is "how long will this
// lock, and how many rows does it touch". Both need a live database —
// EXPLAIN, table statistics, the server's own lock model — and neither is
// knowable from the text. So this file reports what the text does say, says
// nothing about the rest, and the gap is registered instead of approximated:
// see docs/blocked-items.md D-7. A number invented here would be worse than
// no number, because an approver who sees "rows: ~1200" stops asking.
//
// It also never guesses beyond what was read. A statement the engine could
// only read partway (see sr01PartialFinding) contributes no tables at all and
// marks the surface Incomplete: the missing tables of a statement nobody read
// must not be mistaken for a statement that touches nothing. That is the same
// rule SR-01 carries — unread is unaudited — applied to the impact list.

import (
	"sort"

	"github.com/xwb1989/sqlparser"
)

// Impact is the aggregate static impact surface of a batch: one entry per
// table named anywhere in it, sorted by name.
//
// Tables may be empty (a batch of SET/USE statements names no table) and that
// is different from a missing Impact: built by the engine it is always
// present, so an absent structure means "not analysed", never "no tables".
type Impact struct {
	Tables []TableImpact `json:"tables"`
	// Incomplete is true when at least one statement was not read in full, so
	// the table list below cannot be claimed as the whole surface.
	Incomplete bool `json:"incomplete,omitempty"`
}

// TableImpact is one table and everything the batch does to it.
type TableImpact struct {
	// Table is the name as the statement wrote it, qualified when the
	// statement qualified it (`db1.orders` and `orders` are two entries on
	// purpose: the engine will not claim they are the same object).
	Table string `json:"table"`
	// Operations is the sorted set of verbs aimed at this table.
	Operations []string `json:"operations"`
	// WholeTable is true when at least one statement naming this table is not
	// narrowed by anything the engine can see (see StatementImpact.WholeTable).
	// A mechanical property, not a severity.
	WholeTable bool `json:"whole_table,omitempty"`
}

// StatementImpact is the impact of one statement, kept next to the statement
// it describes so a reader can check the engine's reading of it.
type StatementImpact struct {
	Statement int `json:"statement"`
	// Operation is the verb: the statement kind for DML and reads
	// (select/update/…), the DDL action for schema changes (create/alter/
	// drop/rename/truncate), and "unknown" for a statement that was not read.
	Operation string `json:"operation"`
	// Writes are the statement's targets (the table an INSERT writes, the
	// tables an UPDATE/DELETE modifies, the object a DDL acts on).
	Writes []string `json:"writes,omitempty"`
	// Reads are the remaining table names anywhere in the statement — the
	// FROM clauses, joins, subqueries and WHERE subselects, found by the
	// library's own walker rather than by a hand-rolled subset. Names only,
	// so a self-referencing statement (INSERT INTO t SELECT … FROM t) lists t
	// once, under Writes.
	Reads []string `json:"reads,omitempty"`
	// WholeTable is true when the statement is not narrowed by anything the
	// engine can see: no WHERE for a delete/update/select, a SELECT source for
	// an insert (INSERT … SELECT can add an unbounded number of rows, INSERT …
	// VALUES cannot), or — for a DDL acting on an existing object (alter,
	// drop, truncate, rename) — by definition, because the object is the unit.
	// A table-creating DDL is false: there are no rows to be unbounded over.
	WholeTable bool `json:"whole_table,omitempty"`
	// Complete is false when the engine did not read this statement in full;
	// the statement then contributes no tables, because a prefix is not the
	// statement (see the file comment).
	Complete bool `json:"complete"`
}

// statementImpact reads the impact of one fully parsed statement.
func statementImpact(stmt sqlparser.Statement, kind StatementKind, index int) StatementImpact {
	imp := StatementImpact{
		Statement:  index,
		Operation:  operationOf(stmt, kind),
		Writes:     writeTargets(stmt),
		WholeTable: wholeTable(stmt),
		Complete:   true,
	}
	all := tableRefs(stmt)
	imp.Reads = subtract(all, imp.Writes)
	return imp
}

// unreadImpact is the entry for a statement the engine did not read in full.
// It claims no tables and no finer verb than "unknown": both would be claims
// about text nobody read.
func unreadImpact(index int) StatementImpact {
	return StatementImpact{Statement: index, Operation: string(KindUnknown)}
}

// aggregateImpact lifts the per-statement impacts into the batch-level table
// list. Statements without tables contribute nothing (the per-statement
// entries remain as evidence that the engine looked at them).
func aggregateImpact(stmts []StatementReview) Impact {
	agg := Impact{Tables: []TableImpact{}}
	byTable := map[string]*TableImpact{}
	for _, sr := range stmts {
		imp := sr.Impact
		if !imp.Complete {
			agg.Incomplete = true
		}
		for _, name := range append(append([]string{}, imp.Writes...), imp.Reads...) {
			e, ok := byTable[name]
			if !ok {
				e = &TableImpact{Table: name}
				byTable[name] = e
			}
			if imp.WholeTable {
				e.WholeTable = true
			}
			if !containsString(e.Operations, imp.Operation) {
				e.Operations = append(e.Operations, imp.Operation)
			}
		}
	}
	for _, e := range byTable {
		sort.Strings(e.Operations)
		agg.Tables = append(agg.Tables, *e)
	}
	sort.Slice(agg.Tables, func(i, j int) bool { return agg.Tables[i].Table < agg.Tables[j].Table })
	return agg
}

// MergeImpact unions impact surfaces — the plan artifact carries one surface
// for the whole change, while the review engine reports one per SQL batch.
// The union is the honest combination: a table touched by any step is touched
// by the change, and Incomplete is sticky for the same reason (one unread
// statement makes the whole surface a lower bound).
func MergeImpact(impacts ...*Impact) *Impact {
	out := &Impact{Tables: []TableImpact{}}
	byTable := map[string]*TableImpact{}
	for _, in := range impacts {
		if in == nil {
			continue
		}
		if in.Incomplete {
			out.Incomplete = true
		}
		for _, t := range in.Tables {
			e, ok := byTable[t.Table]
			if !ok {
				e = &TableImpact{Table: t.Table}
				byTable[t.Table] = e
			}
			if t.WholeTable {
				e.WholeTable = true
			}
			for _, op := range t.Operations {
				if !containsString(e.Operations, op) {
					e.Operations = append(e.Operations, op)
				}
			}
		}
	}
	for _, e := range byTable {
		sort.Strings(e.Operations)
		out.Tables = append(out.Tables, *e)
	}
	sort.Slice(out.Tables, func(i, j int) bool { return out.Tables[i].Table < out.Tables[j].Table })
	return out
}

// operationOf names the verb. The statement kind is the right word for DML
// and reads; for a schema change the DDL action is the informative one
// ("drop" tells an approver what "ddl" does not).
func operationOf(stmt sqlparser.Statement, kind StatementKind) string {
	switch s := stmt.(type) {
	case *sqlparser.DDL:
		if s.Action != "" {
			return s.Action
		}
	case *sqlparser.DBDDL:
		if s.Action != "" {
			return s.Action + " database"
		}
	}
	return string(kind)
}

// writeTargets extracts the tables the statement modifies. Reads are what is
// left over, so a table that is both read and written lands under Writes.
func writeTargets(stmt sqlparser.Statement) []string {
	switch s := stmt.(type) {
	case *sqlparser.Insert:
		return tableRefs(s.Table)
	case *sqlparser.Update:
		return tableRefs(s.TableExprs)
	case *sqlparser.Delete:
		// `DELETE FROM t` names its table in the FROM list; the multi-table
		// form (`DELETE t1 FROM t1 JOIN t2`) names its targets explicitly.
		if len(s.Targets) > 0 {
			return tableRefs(s.Targets)
		}
		return tableRefs(s.TableExprs)
	case *sqlparser.DDL:
		out := tableRefs(s.Table)
		if !s.NewName.IsEmpty() {
			out = append(out, tableRefs(s.NewName)...)
		}
		return dedupSorted(out)
	default:
		return nil
	}
}

// wholeTable answers "is this statement narrowed by anything the engine can
// see" — see StatementImpact.WholeTable for the exact rule.
func wholeTable(stmt sqlparser.Statement) bool {
	switch s := stmt.(type) {
	case *sqlparser.Delete:
		return s.Where == nil
	case *sqlparser.Update:
		return s.Where == nil
	case *sqlparser.Select:
		return s.Where == nil
	case *sqlparser.Insert:
		// InsertRows is an interface (*Select, *Union, Values, *ParenSelect).
		// A literal value list is the only bounded form the engine can see;
		// everything else — INSERT … SELECT, UNION, a paren-select, or a shape
		// this grammar parsed without rows — can bring an unbounded number of
		// rows, so it reads as unbounded. The fork's Values is a value type,
		// hence the assertion below is not a pointer.
		_, literalValues := s.Rows.(sqlparser.Values)
		return !literalValues
	case *sqlparser.DDL:
		switch s.Action {
		case sqlparser.AlterStr, sqlparser.DropStr, sqlparser.TruncateStr, sqlparser.RenameStr:
			return true
		}
		return false
	default:
		return false
	}
}

// tableRefs collects the tables the node references, using the library's own
// walker so subqueries, joins and INSERT … SELECT sources are found by the
// traversal that reads the statement — not by a hand-rolled subset that would
// silently miss whatever it did not think of.
//
// One quirk of this fork has to be handled: a TableName node appears in two
// roles. In a table position it IS a table reference (`FROM orders`), but
// ColName.Qualifier is itself a TableName, so every column qualifier is one
// too (`o.id` reports a TableName named "o", alias and all). Only the first
// role is a table.
//
// The two roles are told apart by OCCURRENCE, not by name: the walker's
// TableName values are counted, the ones that are column qualifiers are
// subtracted one for one, and what is left is the table references. Name
// subtraction would be wrong in the plainest possible way — `DELETE FROM
// orders WHERE orders.id = 1` gives both roles the same name, and erasing the
// name would erase the table the statement is about.
func tableRefs(node sqlparser.SQLNode) []string {
	if node == nil {
		return nil
	}
	// The visit functions never return an error, so Walk's error is always
	// nil; it is discarded rather than handled at every call site.
	var mentioned []string
	_ = sqlparser.Walk(func(n sqlparser.SQLNode) (bool, error) {
		if tn, ok := n.(sqlparser.TableName); ok {
			if name := formatTable(tn); name != "" {
				mentioned = append(mentioned, name)
			}
		}
		return true, nil
	}, node)

	qualifiers := map[string]int{}
	_ = sqlparser.Walk(func(n sqlparser.SQLNode) (bool, error) {
		if cn, ok := n.(*sqlparser.ColName); ok {
			if name := formatTable(cn.Qualifier); name != "" {
				qualifiers[name]++
			}
		}
		return true, nil
	}, node)

	seen := map[string]bool{}
	out := make([]string, 0, len(mentioned))
	for _, name := range mentioned {
		if qualifiers[name] > 0 {
			qualifiers[name]--
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func formatTable(tn sqlparser.TableName) string {
	name := tn.Name.String()
	if name == "" {
		return ""
	}
	if q := tn.Qualifier.String(); q != "" {
		return q + "." + name
	}
	return name
}

func subtract(all, remove []string) []string {
	if len(all) == 0 {
		return nil
	}
	drop := map[string]bool{}
	for _, r := range remove {
		drop[r] = true
	}
	var out []string
	for _, a := range all {
		if !drop[a] {
			out = append(out, a)
		}
	}
	return out
}

func dedupSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
