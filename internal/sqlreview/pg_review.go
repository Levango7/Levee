package sqlreview

// pg_review.go audits PostgreSQL with PostgreSQL's own AST. The default Review
// entry point remains MySQL-only; only an explicit pg.query route reaches this
// file. A parse failure is still SR-01/fail, never a clean pass.
//
// MySQL-only rules are deliberately not copied: SR-10 (InnoDB row id), SR-11
// (ENGINE), and SR-16 (SET GLOBAL) have no PostgreSQL meaning. SR-14/SR-15
// keep their warning but name PostgreSQL's actual batching mechanism rather
// than recommending UPDATE/DELETE ... LIMIT, which PostgreSQL does not accept.

import (
	"fmt"
	"strings"

	pg_query "github.com/sqlc-dev/oliphant"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ReviewPostgres scores one PostgreSQL batch. It never changes the semantics
// of the existing MySQL Review entry point.
func ReviewPostgres(batch string) (*ReviewResult, error) {
	if strings.TrimSpace(batch) == "" {
		return nil, fmt.Errorf("sqlreview: empty review input")
	}
	tree, err := pg_query.Parse(batch)
	if err != nil {
		sr := StatementReview{Index: 1, Text: batch, Kind: KindUnknown, Impact: unreadImpact(1)}
		sr.Findings = append(sr.Findings, Finding{
			Rule: "SR-01", Severity: SeverityFail, Statement: 1, Dialect: DialectPostgres,
			Message: fmt.Sprintf("PostgreSQL statement does not parse; it was not audited and must not run: %v", err),
		})
		res := &ReviewResult{Statements: []StatementReview{sr}, Findings: sr.Findings, FailCount: 1, Verdict: VerdictFail}
		res.Impact = aggregateImpact(res.Statements)
		return res, nil
	}

	res := &ReviewResult{Findings: []Finding{}}
	var sawDDL, sawDML bool
	for i, raw := range tree.GetStmts() {
		text := pgStatementText(batch, raw)
		sr := reviewPGStatement(raw, text, i+1)
		res.Statements = append(res.Statements, sr)
		switch sr.Kind {
		case KindDDL, KindDBDDL:
			sawDDL = true
		case KindInsert, KindUpdate, KindDelete:
			sawDML = true
		}
	}
	if len(res.Statements) == 0 {
		return nil, fmt.Errorf("sqlreview: empty review input")
	}
	for _, sr := range res.Statements {
		res.Findings = append(res.Findings, sr.Findings...)
	}
	res.Findings = append(res.Findings, pgBatchRules(res.Statements, sawDDL, sawDML)...)
	res.Impact = aggregateImpact(res.Statements)
	for _, f := range res.Findings {
		switch f.Severity {
		case SeverityFail:
			res.FailCount++
		case SeverityWarn:
			res.WarnCount++
		}
	}
	switch {
	case res.FailCount > 0:
		res.Verdict = VerdictFail
	case res.WarnCount > 0:
		res.Verdict = VerdictWarn
	default:
		res.Verdict = VerdictPass
	}
	return res, nil
}

func pgStatementText(batch string, raw *pg_query.RawStmt) string {
	start := int(raw.GetStmtLocation())
	if start < 0 || start > len(batch) {
		return strings.TrimSpace(batch)
	}
	end := start + int(raw.GetStmtLen())
	if end <= start || end > len(batch) {
		end = len(batch)
	}
	return strings.TrimSpace(batch[start:end])
}

func reviewPGStatement(raw *pg_query.RawStmt, text string, index int) StatementReview {
	node := raw.GetStmt()
	sr := StatementReview{Index: index, Text: text, Kind: pgClassify(node), Findings: []Finding{}}
	sr.Impact = pgStatementImpact(node, sr.Kind, index)
	sr.Findings = append(sr.Findings, pgStatementRules(node, sr.Kind, index)...)
	return sr
}

func pgClassify(node *pg_query.Node) StatementKind {
	switch node.GetNode().(type) {
	case *pg_query.Node_InsertStmt:
		return KindInsert
	case *pg_query.Node_UpdateStmt:
		return KindUpdate
	case *pg_query.Node_DeleteStmt:
		return KindDelete
	case *pg_query.Node_SelectStmt:
		return KindSelect
	case *pg_query.Node_CreateStmt, *pg_query.Node_AlterTableStmt, *pg_query.Node_IndexStmt,
		*pg_query.Node_DropStmt, *pg_query.Node_TruncateStmt, *pg_query.Node_RenameStmt,
		*pg_query.Node_CreateTableAsStmt, *pg_query.Node_ViewStmt, *pg_query.Node_CreateSeqStmt:
		return KindDDL
	case *pg_query.Node_CreatedbStmt, *pg_query.Node_DropdbStmt:
		return KindDBDDL
	case *pg_query.Node_VariableSetStmt:
		return KindSet
	case *pg_query.Node_VariableShowStmt:
		return KindShow
	case *pg_query.Node_TransactionStmt:
		return KindTxn
	default:
		// A parsed but unsupported administrative statement is not a clean pass:
		// SR-17 names the human confirmation it still needs.
		return KindOtherAdmin
	}
}

func pgStatementRules(node *pg_query.Node, kind StatementKind, index int) []Finding {
	var out []Finding
	add := func(id RuleID, sev Severity, message string) {
		out = append(out, Finding{Rule: id, Severity: sev, Statement: index, Message: message})
	}

	switch s := node.GetNode().(type) {
	case *pg_query.Node_DeleteStmt:
		where := s.DeleteStmt.GetWhereClause()
		if where == nil {
			add("SR-02", SeverityFail, "DELETE without WHERE deletes every row of the table; add a WHERE predicate, or run it inside an irreversible-marked run with an explicit row-count estimate and a verified backup")
		} else {
			if pgTautology(where) {
				add("SR-04", SeverityFail, "WHERE clause is always true (constant tautology such as 1=1, or an OR arm that is always true), so the statement matches every row; rewrite it with a predicate that actually selects rows")
			}
			add("SR-14", SeverityWarn, "DELETE has a WHERE but no batch bound; PostgreSQL has no DELETE LIMIT, so bound the keys in a subquery (ctid or a unique key) or split the change into batches")
		}
	case *pg_query.Node_UpdateStmt:
		where := s.UpdateStmt.GetWhereClause()
		if where == nil {
			add("SR-03", SeverityFail, "UPDATE without WHERE rewrites every row of the table; add a WHERE predicate, or run it inside an irreversible-marked run with an explicit row-count estimate and a verified backup")
		} else {
			if pgTautology(where) {
				add("SR-04", SeverityFail, "WHERE clause is always true (constant tautology such as 1=1, or an OR arm that is always true), so the statement matches every row; rewrite it with a predicate that actually selects rows")
			}
			add("SR-15", SeverityWarn, "UPDATE has a WHERE but no batch bound; PostgreSQL has no UPDATE LIMIT, so bound the keys in a subquery (ctid or a unique key) or split the change into batches")
		}
	case *pg_query.Node_InsertStmt:
		if len(s.InsertStmt.GetCols()) == 0 && pgIsValuesInsert(s.InsertStmt) {
			add("SR-13", SeverityWarn, "INSERT without a column list binds values by table order, so adding a column later silently reorders the insert; name the columns explicitly")
		}
	case *pg_query.Node_SelectStmt:
		if pgSelectHasStar(s.SelectStmt) {
			add("SR-12", SeverityWarn, "SELECT * returns every column including ones added later; list the columns the consumer actually reads so schema drift cannot change the result shape")
		}
		if len(s.SelectStmt.GetLockingClause()) > 0 {
			add("SR-18", SeverityWarn, "locking read (FOR UPDATE / FOR SHARE) holds row locks until the transaction ends and blocks concurrent writers; confirm the transaction is short and the isolation level is intended")
		}
	case *pg_query.Node_CreateTableAsStmt:
		if pgSelectHasStar(s.CreateTableAsStmt.GetQuery().GetSelectStmt()) {
			add("SR-12", SeverityWarn, "CREATE TABLE AS SELECT * copies every current and future source column; list the columns the new table actually needs")
		}
	case *pg_query.Node_TruncateStmt:
		add("SR-05", SeverityFail, "TRUNCATE removes every row and cannot be rolled back row-by-row; it belongs in an irreversible-marked run with a verified backup")
	case *pg_query.Node_DropdbStmt:
		add("SR-06", SeverityFail, "DROP DATABASE destroys every object in the database; refuse it in ordinary change runs and require a dedicated irreversible-marked run")
	case *pg_query.Node_DropStmt:
		add("SR-07", SeverityWarn, "DROP removes a schema object permanently; require a verified backup and an irreversible-marked run before execution")
	case *pg_query.Node_AlterTableStmt, *pg_query.Node_IndexStmt:
		add("SR-08", SeverityWarn, "this schema change rewrites an existing object or its access path; review the clause body, lock window and rollback evidence before approval")
	case *pg_query.Node_RenameStmt:
		add("SR-09", SeverityWarn, "RENAME changes the name every caller references; confirm downstream queries, views and scheduled jobs before execution")
	default:
		if kind == KindOtherAdmin {
			add("SR-17", SeverityWarn, "administrative statement; the engine parsed it but does not claim to classify its semantics — a human must confirm intent and the maintenance window")
		}
	}
	// SR-10/SR-11 (InnoDB row id / ENGINE), SR-16 (SET GLOBAL) are MySQL-only.
	return out
}

func pgIsValuesInsert(stmt *pg_query.InsertStmt) bool {
	sel := stmt.GetSelectStmt().GetSelectStmt()
	return sel != nil && len(sel.GetValuesLists()) > 0
}

func pgSelectHasStar(stmt *pg_query.SelectStmt) bool {
	if stmt == nil {
		return false
	}
	for _, target := range stmt.GetTargetList() {
		res := target.GetResTarget()
		if res == nil {
			continue
		}
		col := res.GetVal().GetColumnRef()
		if col == nil {
			continue
		}
		for _, field := range col.GetFields() {
			if _, ok := field.GetNode().(*pg_query.Node_AStar); ok {
				return true
			}
		}
	}
	return false
}

func pgTautology(node *pg_query.Node) bool {
	if node == nil {
		return false
	}
	switch v := node.GetNode().(type) {
	case *pg_query.Node_BoolExpr:
		args := v.BoolExpr.GetArgs()
		switch v.BoolExpr.GetBoolop() {
		case pg_query.BoolExprType_OR_EXPR:
			for _, arg := range args {
				if pgTautology(arg) {
					return true
				}
			}
		case pg_query.BoolExprType_AND_EXPR:
			if len(args) == 0 {
				return false
			}
			for _, arg := range args {
				if !pgTautology(arg) {
					return false
				}
			}
			return true
		}
	case *pg_query.Node_AExpr:
		if v.AExpr.GetKind() != pg_query.A_Expr_Kind_AEXPR_OP || pgOperatorName(v.AExpr.GetName()) != "=" {
			return false
		}
		left, lok := pgConstKey(v.AExpr.GetLexpr())
		right, rok := pgConstKey(v.AExpr.GetRexpr())
		return lok && rok && left == right
	case *pg_query.Node_AConst:
		return v.AConst.GetBoolval() != nil && v.AConst.GetBoolval().GetBoolval()
	case *pg_query.Node_TypeCast:
		return pgTautology(v.TypeCast.GetArg())
	}
	return false
}

func pgOperatorName(nodes []*pg_query.Node) string {
	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if s := node.GetString_(); s != nil {
			parts = append(parts, s.GetSval())
		}
	}
	return strings.Join(parts, ".")
}

func pgConstKey(node *pg_query.Node) (string, bool) {
	c := node.GetAConst()
	if c == nil || c.GetIsnull() {
		return "", false
	}
	switch v := c.GetVal().(type) {
	case *pg_query.A_Const_Ival:
		return fmt.Sprintf("i:%d", v.Ival.GetIval()), true
	case *pg_query.A_Const_Fval:
		return fmt.Sprintf("f:%s", v.Fval.GetFval()), true
	case *pg_query.A_Const_Boolval:
		return fmt.Sprintf("b:%t", v.Boolval.GetBoolval()), true
	case *pg_query.A_Const_Sval:
		return fmt.Sprintf("s:%s", v.Sval.GetSval()), true
	case *pg_query.A_Const_Bsval:
		return fmt.Sprintf("bits:%s", v.Bsval.GetBsval()), true
	default:
		return "", false
	}
}

func pgStatementImpact(node *pg_query.Node, kind StatementKind, index int) StatementImpact {
	imp := StatementImpact{Statement: index, Operation: pgOperation(node, kind), Complete: true}
	imp.Writes = pgWriteTargets(node)
	all := pgRangeNames(node)
	imp.Reads = subtract(all, imp.Writes)
	imp.WholeTable = pgWholeTable(node)
	return imp
}

func pgOperation(node *pg_query.Node, kind StatementKind) string {
	switch node.GetNode().(type) {
	case *pg_query.Node_CreateStmt, *pg_query.Node_CreateTableAsStmt:
		return "create"
	case *pg_query.Node_AlterTableStmt:
		return "alter"
	case *pg_query.Node_IndexStmt:
		return "create index"
	case *pg_query.Node_DropStmt:
		return "drop"
	case *pg_query.Node_TruncateStmt:
		return "truncate"
	case *pg_query.Node_RenameStmt:
		return "rename"
	case *pg_query.Node_CreatedbStmt:
		return "create database"
	case *pg_query.Node_DropdbStmt:
		return "drop database"
	default:
		return string(kind)
	}
}

func pgWriteTargets(node *pg_query.Node) []string {
	switch s := node.GetNode().(type) {
	case *pg_query.Node_InsertStmt:
		return pgRangeName(s.InsertStmt.GetRelation())
	case *pg_query.Node_UpdateStmt:
		return pgRangeName(s.UpdateStmt.GetRelation())
	case *pg_query.Node_DeleteStmt:
		return pgRangeName(s.DeleteStmt.GetRelation())
	case *pg_query.Node_CreateStmt:
		return pgRangeName(s.CreateStmt.GetRelation())
	case *pg_query.Node_AlterTableStmt:
		return pgRangeName(s.AlterTableStmt.GetRelation())
	case *pg_query.Node_IndexStmt:
		return pgRangeName(s.IndexStmt.GetRelation())
	case *pg_query.Node_CreateTableAsStmt:
		return pgRangeName(s.CreateTableAsStmt.GetInto().GetRel())
	case *pg_query.Node_TruncateStmt:
		var out []string
		for _, rel := range s.TruncateStmt.GetRelations() {
			out = append(out, pgRangeName(rel.GetRangeVar())...)
		}
		return dedupSorted(out)
	case *pg_query.Node_DropStmt:
		return pgDropObjectNames(s.DropStmt)
	case *pg_query.Node_RenameStmt, *pg_query.Node_ViewStmt, *pg_query.Node_CreateSeqStmt:
		// These DDL statements act on the relations they name. Plain SELECT and
		// administrative statements have no write target and must not land here.
		return pgRangeNames(node)
	default:
		return nil
	}
}

func pgWholeTable(node *pg_query.Node) bool {
	switch s := node.GetNode().(type) {
	case *pg_query.Node_UpdateStmt:
		return s.UpdateStmt.GetWhereClause() == nil
	case *pg_query.Node_DeleteStmt:
		return s.DeleteStmt.GetWhereClause() == nil
	case *pg_query.Node_SelectStmt:
		return s.SelectStmt.GetWhereClause() == nil
	case *pg_query.Node_InsertStmt:
		return !pgIsValuesInsert(s.InsertStmt)
	case *pg_query.Node_AlterTableStmt, *pg_query.Node_IndexStmt, *pg_query.Node_DropStmt,
		*pg_query.Node_TruncateStmt, *pg_query.Node_RenameStmt, *pg_query.Node_DropdbStmt:
		return true
	case *pg_query.Node_CreateStmt, *pg_query.Node_CreateTableAsStmt, *pg_query.Node_CreatedbStmt,
		*pg_query.Node_ViewStmt, *pg_query.Node_CreateSeqStmt:
		// A newly created object has no existing row set to be unbounded over.
		return false
	default:
		return false
	}
}

func pgRangeName(rel *pg_query.RangeVar) []string {
	if rel == nil {
		return nil
	}
	parts := make([]string, 0, 3)
	for _, part := range []string{rel.GetCatalogname(), rel.GetSchemaname(), rel.GetRelname()} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{strings.Join(parts, ".")}
}

func pgRangeNames(node *pg_query.Node) []string {
	ctes := map[string]bool{}
	pgWalk(node, func(n *pg_query.Node) {
		if cte := n.GetCommonTableExpr(); cte != nil {
			ctes[cte.GetCtename()] = true
		}
	})
	var out []string
	pgWalk(node, func(n *pg_query.Node) {
		if rel := n.GetRangeVar(); rel != nil && !ctes[rel.GetRelname()] {
			out = append(out, pgRangeName(rel)...)
		}
	})
	return dedupSorted(out)
}

func pgDropObjectNames(stmt *pg_query.DropStmt) []string {
	var out []string
	for _, object := range stmt.GetObjects() {
		var parts []string
		pgWalk(object, func(n *pg_query.Node) {
			if s := n.GetString_(); s != nil {
				parts = append(parts, s.GetSval())
			}
		})
		if len(parts) > 0 {
			out = append(out, strings.Join(parts, "."))
		}
	}
	return dedupSorted(out)
}

// pgWalk traverses every protobuf message rather than maintaining a second,
// hand-written copy of the PostgreSQL node hierarchy. Some fields are *Node,
// while others (CTAS's SelectStmt, for example) are concrete messages; both
// must be descended into or the source table disappears from the impact.
func pgWalk(node *pg_query.Node, visit func(*pg_query.Node)) {
	if node == nil {
		return
	}
	pgWalkMessage(node.ProtoReflect(), visit)
}

func pgWalkMessage(message protoreflect.Message, visit func(*pg_query.Node)) {
	if message == nil {
		return
	}
	if node, ok := message.Interface().(*pg_query.Node); ok {
		visit(node)
	}
	message.Range(func(fd protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
			return true
		}
		if fd.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				pgWalkMessage(list.Get(i).Message(), visit)
			}
			return true
		}
		pgWalkMessage(value.Message(), visit)
		return true
	})
}

func pgBatchRules(stmts []StatementReview, sawDDL, sawDML bool) []Finding {
	var out []Finding
	if sawDDL && sawDML {
		out = append(out, Finding{
			Rule: "SR-19", Severity: SeverityWarn, Statement: 0,
			Message: "batch mixes DDL and DML; PostgreSQL can roll back many DDL statements, but object locks and side effects still differ by statement — split schema and data changes when one atomic unit is required",
		})
	}
	if len(stmts) > 1 {
		out = append(out, Finding{
			Rule: "SR-20", Severity: SeverityWarn, Statement: 0,
			Message: fmt.Sprintf("batch carries %d statements; each was scored on its own and cross-statement atomicity is not asserted — wrap the batch in an explicit transaction if it must be atomic", len(stmts)),
		})
	}
	return out
}
