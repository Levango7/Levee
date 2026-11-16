package sqlguard

// tables.go holds the other piece the two database modules share: how a
// `*.schema_snapshot` step names the tables it captures.
//
// It is here for the same reason the classifier is: two callers, one
// convention. mysql and pg generate completely different SQL for a snapshot
// (SHOW CREATE TABLE versus information_schema/pg_catalog introspection), but
// the ARGUMENT must mean the same thing on both surfaces — a workflow that
// moves from MySQL to PostgreSQL should not discover that `tables` accepted a
// different shape all along.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// tableNameRe matches a table reference that may be spliced into a quoted SQL
// identifier: letters, digits, underscore, dollar, and dots for a
// schema/database qualifier. Both engines allow far more (and MySQL allows
// backticks, PostgreSQL allows any quoted string), but the snapshot action
// QUOTES what it is given rather than trusting it, so the accepted set is the
// one whose quoting is unambiguous on both servers.
var tableNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*(\.[A-Za-z_][A-Za-z0-9_$]*)?$`)

// TableList reads the `tables` argument of a schema-snapshot action: a
// non-empty list of table names, each validated.
//
// The argument is REQUIRED and must be non-empty. An empty list would produce
// a snapshot that captures nothing and still exits 0 — an evidence row that
// reads exactly like "these tables have no definition", which is the one
// reading a pre-change snapshot must never invite.
//
// A single string is refused rather than accepted as a one-element list:
// `tables: users, orders` (a YAML scalar) would otherwise mean one table
// named `users, orders`, and the failure would surface as "relation does not
// exist" three steps later.
func TableList(args map[string]any, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("missing argument %q (the tables this snapshot captures)", key)
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be a list of table names, got %T", key, raw)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("argument %q is empty: a snapshot that captures nothing must not "+
			"report success", key)
	}
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for i, item := range items {
		name, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("argument %q[%d] must be a string, got %T", key, i, item)
		}
		name = strings.TrimSpace(name)
		if !tableNameRe.MatchString(name) {
			return nil, fmt.Errorf("invalid table name %q: letters, digits, underscore, dollar "+
				"and a single dot qualifier are allowed (the name is spliced into a quoted identifier)", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("table %q is listed twice: a duplicate captures the same "+
				"definition twice and hides a typo in the list", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// SplitQualifier splits a validated table reference into schema and name. The
// schema is empty when the reference did not carry one — the caller then
// decides what "unqualified" means on its engine (MySQL: the connection's
// default database; PostgreSQL: the session search_path).
func SplitQualifier(table string) (schema, name string) {
	if i := strings.IndexByte(table, '.'); i >= 0 {
		return table[:i], table[i+1:]
	}
	return "", table
}

// SortedUnique returns a sorted, de-duplicated copy — used where the OUTPUT of
// a comparison (not the authored order) is reported, so a message does not
// depend on iteration order.
func SortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
