// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sql

import (
	"container/heap"

	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// sortNode represents a node that sorts the rows returned by its
// sub-node.
type sortNode struct {
	p        *planner
	plan     planNode
	columns  sqlbase.ResultColumns
	ordering sqlbase.ColumnOrdering

	needSort     bool
	sortStrategy sortingStrategy
	valueIter    valueIterator
}

func ensureColumnOrderable(c sqlbase.ResultColumn) error {
	if _, ok := c.Typ.(types.TArray); ok {
		return errors.Errorf("can't order by column type %s", c.Typ)
	}
	return nil
}

// orderBy constructs a sortNode based on the ORDER BY clause.
//
// In the general case (SELECT/UNION/VALUES), we can sort by a column index or a
// column name.
//
// However, for a SELECT, we can also sort by the pre-alias column name (SELECT
// a AS b ORDER BY b) as well as expressions (SELECT a, b, ORDER BY a+b). In
// this case, construction of the sortNode might adjust the number of render
// targets in the renderNode if any ordering expressions are specified.
//
// TODO(dan): SQL also allows sorting a VALUES or UNION by an expression.
// Support this. It will reduce some of the special casing below, but requires a
// generalization of how to add derived columns to a SelectStatement.
func (p *planner) orderBy(
	ctx context.Context, orderBy tree.OrderBy, n planNode,
) (*sortNode, error) {
	if orderBy == nil {
		return nil, nil
	}

	// Multiple tests below use renderNode as a special case.
	// So factor the cast.
	s, _ := n.(*renderNode)

	// We grab a copy of columns here because we might add new render targets
	// below. This is the set of columns requested by the query.
	columns := planColumns(n)
	numOriginalCols := len(columns)
	if s != nil {
		numOriginalCols = s.numOriginalCols
	}
	var ordering sqlbase.ColumnOrdering

	var err error
	orderBy, err = p.rewriteIndexOrderings(ctx, orderBy)
	if err != nil {
		return nil, err
	}

	for _, o := range orderBy {
		direction := encoding.Ascending
		if o.Direction == tree.Descending {
			direction = encoding.Descending
		}

		// Unwrap parenthesized expressions like "((a))" to "a".
		expr := tree.StripParens(o.Expr)

		// The logical data source for ORDER BY is the list of render
		// expressions for a SELECT, as specified in the input SQL text
		// (or an entire UNION or VALUES clause).  Alas, SQL has some
		// historical baggage from SQL92 and there are some special cases:
		//
		// SQL92 rules:
		//
		// 1) if the expression is the aliased (AS) name of a render
		//    expression in a SELECT clause, then use that
		//    render as sort key.
		//    e.g. SELECT a AS b, b AS c ORDER BY b
		//    this sorts on the first render.
		//
		// 2) column ordinals. If a simple integer literal is used,
		//    optionally enclosed within parentheses but *not subject to
		//    any arithmetic*, then this refers to one of the columns of
		//    the data source. Then use the render expression at that
		//    ordinal position as sort key.
		//
		// SQL99 rules:
		//
		// 3) otherwise, if the expression is already in the render list,
		//    then use that render as sort key.
		//    e.g. SELECT b AS c ORDER BY b
		//    this sorts on the first render.
		//    (this is an optimization)
		//
		// 4) if the sort key is not dependent on the data source (no
		//    IndexedVar) then simply do not sort. (this is an optimization)
		//
		// 5) otherwise, add a new render with the ORDER BY expression
		//    and use that as sort key.
		//    e.g. SELECT a FROM t ORDER by b
		//    e.g. SELECT a, b FROM t ORDER by a+b

		index := -1

		// First, deal with render aliases.
		if vBase, ok := expr.(tree.VarName); ok {
			v, err := vBase.NormalizeVarName()
			if err != nil {
				return nil, err
			}

			if c, ok := v.(*tree.ColumnItem); ok && c.TableName.Table() == "" {
				// Look for an output column that matches the name. This
				// handles cases like:
				//
				//   SELECT a AS b FROM t ORDER BY b
				target := string(c.ColumnName)
				for j, col := range columns {
					if col.Name == target {
						if index != -1 {
							// There is more than one render alias that matches the ORDER BY
							// clause. Here, SQL92 is specific as to what should be done:
							// if the underlying expression is known (we're on a renderNode)
							// and it is equivalent, then just accept that and ignore the ambiguity.
							// This plays nice with `SELECT b, * FROM t ORDER BY b`. Otherwise,
							// reject with an ambiguity error.
							if s == nil || !s.equivalentRenders(j, index) {
								return nil, errors.Errorf("ORDER BY \"%s\" is ambiguous", target)
							}
							// Note that in this case we want to use the index of the first
							// matching column. This is because
							// renderNode.computePhysicalProps also prefers the first column,
							// and we want the orderings to match as much as possible.
							continue
						}
						index = j
					}
				}
			}
		}

		// So Then, deal with column ordinals.
		if index == -1 {
			col, err := p.colIndex(numOriginalCols, expr, "ORDER BY")
			if err != nil {
				return nil, err
			}
			if col != -1 {
				index = col
			}
		}

		// If we're ordering by using one of the existing renders, ensure it's a
		// column type we can order on.
		if index != -1 {
			if err := ensureColumnOrderable(columns[index]); err != nil {
				return nil, err
			}
		}

		// Finally, if we haven't found anything so far, we really
		// need a new render.
		// TODO(knz/dan): currently this is only possible for renderNode.
		// If we are dealing with a UNION or something else we would need
		// to fabricate an intermediate renderNode to add the new render.
		if index == -1 && s != nil {
			cols, exprs, hasStar, err := p.computeRenderAllowingStars(
				ctx, tree.SelectExpr{Expr: expr}, types.Any,
				s.sourceInfo, s.ivarHelper, autoGenerateRenderOutputName)
			if err != nil {
				return nil, err
			}
			p.hasStar = p.hasStar || hasStar

			if len(cols) == 0 {
				// Nothing was expanded! No order here.
				continue
			}

			// ORDER BY (a, b) -> ORDER BY a, b
			cols, exprs = flattenTuples(cols, exprs, &s.ivarHelper)

			colIdxs := s.addOrReuseRenders(cols, exprs, true)
			for i := 0; i < len(colIdxs)-1; i++ {
				// If more than 1 column were expanded, turn them into sort columns too.
				// Except the last one, which will be added below.
				ordering = append(ordering,
					sqlbase.ColumnOrderInfo{ColIdx: colIdxs[i], Direction: direction})
			}
			index = colIdxs[len(colIdxs)-1]
			// Ensure our newly rendered columns are ok to order by.
			renderCols := planColumns(s)
			for _, colIdx := range colIdxs {
				if err := ensureColumnOrderable(renderCols[colIdx]); err != nil {
					return nil, err
				}
			}
		}

		if index == -1 {
			return nil, errors.Errorf("column %s does not exist", expr)
		}
		ordering = append(ordering,
			sqlbase.ColumnOrderInfo{ColIdx: index, Direction: direction})
	}

	if ordering == nil {
		// No ordering; simply drop the sort node.
		return nil, nil
	}
	return &sortNode{p: p, columns: columns, ordering: ordering}, nil
}

// flattenTuples extracts the members of tuples into a list of columns.
func flattenTuples(
	cols sqlbase.ResultColumns, exprs []tree.TypedExpr, ivarHelper *tree.IndexedVarHelper,
) (sqlbase.ResultColumns, []tree.TypedExpr) {
	// We want to avoid allocating new slices unless strictly necessary.
	var newExprs []tree.TypedExpr
	var newCols sqlbase.ResultColumns
	for i, e := range exprs {
		if t, ok := e.(*tree.Tuple); ok {
			if newExprs == nil {
				// All right, it was necessary to allocate the slices after all.
				newExprs = make([]tree.TypedExpr, i, len(exprs))
				newCols = make(sqlbase.ResultColumns, i, len(cols))
				copy(newExprs, exprs[:i])
				copy(newCols, cols[:i])
			}

			newCols, newExprs = flattenTuple(t, newCols, newExprs, ivarHelper)
		} else if newExprs != nil {
			newExprs = append(newExprs, e)
			newCols = append(newCols, cols[i])
		}
	}
	if newExprs != nil {
		return newCols, newExprs
	}
	return cols, exprs
}

// flattenTuple extracts the members of one tuple into a list of columns.
func flattenTuple(
	t *tree.Tuple,
	cols sqlbase.ResultColumns,
	exprs []tree.TypedExpr,
	ivarHelper *tree.IndexedVarHelper,
) (sqlbase.ResultColumns, []tree.TypedExpr) {
	for _, e := range t.Exprs {
		if eT, ok := e.(*tree.Tuple); ok {
			cols, exprs = flattenTuple(eT, cols, exprs, ivarHelper)
		} else {
			expr := e.(tree.TypedExpr)
			exprs = append(exprs, expr)
			cols = append(cols, sqlbase.ResultColumn{
				Name: expr.String(),
				Typ:  expr.ResolvedType(),
			})
		}
	}
	return cols, exprs
}

// rewriteIndexOrderings rewrites an ORDER BY clause that uses the
// extended INDEX or PRIMARY KEY syntax into an ORDER BY clause that
// doesn't: each INDEX or PRIMARY KEY order specification is replaced
// by the list of columns in the specified index.
// For example,
//   ORDER BY PRIMARY KEY kv -> ORDER BY kv.k ASC
//   ORDER BY INDEX kv@primary -> ORDER BY kv.k ASC
// With an index foo(a DESC, b ASC):
//   ORDER BY INDEX t@foo ASC -> ORDER BY t.a DESC, t.a ASC
//   ORDER BY INDEX t@foo DESC -> ORDER BY t.a ASC, t.b DESC
func (p *planner) rewriteIndexOrderings(
	ctx context.Context, orderBy tree.OrderBy,
) (tree.OrderBy, error) {
	// The loop above *may* allocate a new slice, but this is only
	// needed if the INDEX / PRIMARY KEY syntax is used. In case the
	// ORDER BY clause only uses the column syntax, we should reuse the
	// same slice. So we start with an empty slice whose underlying
	// array is the same as the original specification.
	newOrderBy := orderBy[:0]
	for _, o := range orderBy {
		switch o.OrderType {
		case tree.OrderByColumn:
			// Nothing to do, just propagate the setting.
			newOrderBy = append(newOrderBy, o)

		case tree.OrderByIndex:
			tn, err := p.QualifyWithDatabase(ctx, &o.Table)
			if err != nil {
				return nil, err
			}
			desc, err := p.getTableDesc(ctx, tn)
			if err != nil {
				return nil, err
			}
			var idxDesc *sqlbase.IndexDescriptor
			if o.Index == "" || string(o.Index) == desc.PrimaryIndex.Name {
				// ORDER BY PRIMARY KEY / ORDER BY INDEX t@primary
				idxDesc = &desc.PrimaryIndex
			} else {
				// ORDER BY INDEX t@somename
				// We need to search for the index with that name.
				for i := range desc.Indexes {
					if string(o.Index) == desc.Indexes[i].Name {
						idxDesc = &desc.Indexes[i]
						break
					}
				}
				if idxDesc == nil {
					return nil, errors.Errorf("index %q not found", tree.ErrString(o.Index))
				}
			}

			// Now, expand the ORDER BY clause by an equivalent clause using that
			// index's columns.

			// First, make the final slice bigger.
			prevNewOrderBy := newOrderBy
			newOrderBy = make(tree.OrderBy,
				len(newOrderBy),
				cap(newOrderBy)+len(idxDesc.ColumnNames)-1)
			copy(newOrderBy, prevNewOrderBy)

			// Now expand the clause.
			for k, colName := range idxDesc.ColumnNames {
				newOrderBy = append(newOrderBy, &tree.Order{
					OrderType: tree.OrderByColumn,
					Expr:      &tree.ColumnItem{TableName: *tn, ColumnName: tree.Name(colName)},
					Direction: chooseDirection(o.Direction == tree.Descending, idxDesc.ColumnDirections[k]),
				})
			}

		default:
			return nil, errors.Errorf("unknown ORDER BY specification: %s", tree.AsString(orderBy))
		}
	}

	if log.V(2) {
		log.Infof(ctx, "rewritten ORDER BY clause: %s", tree.AsString(newOrderBy))
	}
	return newOrderBy, nil
}

// chooseDirection translates the specified IndexDescriptor_Direction
// into a tree.Direction. If invert is true, the idxDir is inverted.
func chooseDirection(invert bool, idxDir sqlbase.IndexDescriptor_Direction) tree.Direction {
	if (idxDir == sqlbase.IndexDescriptor_ASC) != invert {
		return tree.Ascending
	}
	return tree.Descending
}

// colIndex takes an expression that refers to a column using an integer, verifies it refers to a
// valid render target and returns the corresponding column index. For example:
//    SELECT a from T ORDER by 1
// Here "1" refers to the first render target "a". The returned index is 0.
func (p *planner) colIndex(numOriginalCols int, expr tree.Expr, context string) (int, error) {
	ord := int64(-1)
	switch i := expr.(type) {
	case *tree.NumVal:
		if i.ShouldBeInt64() {
			val, err := i.AsInt64()
			if err != nil {
				return -1, err
			}
			ord = val
		} else {
			return -1, errors.Errorf("non-integer constant in %s: %s", context, expr)
		}
	case *tree.DInt:
		if *i >= 0 {
			ord = int64(*i)
		}
	case *tree.StrVal:
		return -1, errors.Errorf("non-integer constant in %s: %s", context, expr)
	case tree.Datum:
		return -1, errors.Errorf("non-integer constant in %s: %s", context, expr)
	}
	if ord != -1 {
		if ord < 1 || ord > int64(numOriginalCols) {
			return -1, errors.Errorf("%s position %s is not in select list", context, expr)
		}
		ord--
	}
	return int(ord), nil
}

func (n *sortNode) Values() tree.Datums {
	// If an ordering expression was used the number of columns in each row might
	// differ from the number of columns requested, so trim the result.
	return n.valueIter.Values()[:len(n.columns)]
}

func (n *sortNode) Start(params runParams) error {
	return n.plan.Start(params)
}

func (n *sortNode) Next(params runParams) (bool, error) {
	cancelChecker := params.p.cancelChecker

	for n.needSort {
		if v, ok := n.plan.(*valuesNode); ok {
			// The plan we wrap is already a values node. Just sort it.
			v.ordering = n.ordering
			n.sortStrategy = newSortAllStrategy(v)
			n.sortStrategy.Finish(params.ctx, cancelChecker)
			n.needSort = false
			break
		} else if n.sortStrategy == nil {
			v := n.p.newContainerValuesNode(planColumns(n.plan), 0)
			v.ordering = n.ordering
			n.sortStrategy = newSortAllStrategy(v)
		}

		if err := cancelChecker.Check(); err != nil {
			return false, err
		}

		// TODO(andrei): If we're scanning an index with a prefix matching an
		// ordering prefix, we should only accumulate values for equal fields
		// in this prefix, then sort the accumulated chunk and output.
		// TODO(irfansharif): matching column ordering speed-ups from distsql,
		// when implemented, could be repurposed and used here.
		next, err := n.plan.Next(params)
		if err != nil {
			return false, err
		}
		if !next {
			n.sortStrategy.Finish(params.ctx, cancelChecker)
			n.valueIter = n.sortStrategy
			n.needSort = false
			break
		}

		values := n.plan.Values()
		if err := n.sortStrategy.Add(params.ctx, values); err != nil {
			return false, err
		}
	}

	// Check again, in case sort returned early due to a cancellation.
	if err := cancelChecker.Check(); err != nil {
		return false, err
	}

	if n.valueIter == nil {
		n.valueIter = n.plan
	}
	return n.valueIter.Next(params)
}

func (n *sortNode) Close(ctx context.Context) {
	n.plan.Close(ctx)
	if n.sortStrategy != nil {
		n.sortStrategy.Close(ctx)
	}
	// n.valueIter points to either n.plan or n.sortStrategy and thus has already
	// been closed.
}

// valueIterator provides iterative access to a value source's values and
// debug values. It is a subset of the planNode interface, so all methods
// should conform to the comments expressed in the planNode definition.
type valueIterator interface {
	Next(runParams) (bool, error)
	Values() tree.Datums
	Close(ctx context.Context)
}

type sortingStrategy interface {
	valueIterator
	// Add adds a single value to the sortingStrategy. It guarantees that
	// if it decided to store the provided value, that it will make a deep
	// copy of it.
	Add(context.Context, tree.Datums) error
	// Finish terminates the sorting strategy, allowing for postprocessing
	// after all values have been provided to the strategy. The method should
	// not be called more than once, and should only be called after all Add
	// calls have occurred.
	Finish(context.Context, *sqlbase.CancelChecker)
}

// sortAllStrategy reads in all values into the wrapped valuesNode and
// uses sort.Sort to sort all values in-place. It has a worst-case time
// complexity of O(n*log(n)) and a worst-case space complexity of O(n).
//
// The strategy is intended to be used when all values need to be sorted.
type sortAllStrategy struct {
	vNode *valuesNode
}

func newSortAllStrategy(vNode *valuesNode) sortingStrategy {
	return &sortAllStrategy{
		vNode: vNode,
	}
}

func (ss *sortAllStrategy) Add(ctx context.Context, values tree.Datums) error {
	_, err := ss.vNode.rows.AddRow(ctx, values)
	return err
}

func (ss *sortAllStrategy) Finish(ctx context.Context, cancelChecker *sqlbase.CancelChecker) {
	ss.vNode.SortAll(cancelChecker)
}

func (ss *sortAllStrategy) Next(params runParams) (bool, error) {
	return ss.vNode.Next(params)
}

func (ss *sortAllStrategy) Values() tree.Datums {
	return ss.vNode.Values()
}

func (ss *sortAllStrategy) Close(ctx context.Context) {
	ss.vNode.Close(ctx)
}

// iterativeSortStrategy reads in all values into the wrapped valuesNode
// and turns the underlying slice into a min-heap. It then pops a value
// off of the heap for each call to Next, meaning that it only needs to
// sort the number of values needed, instead of the entire slice. If the
// underlying value source provides n rows and the strategy produce only
// k rows, it has a worst-case time complexity of O(n + k*log(n)) and a
// worst-case space complexity of O(n).
//
// The strategy is intended to be used when an unknown number of values
// need to be sorted, but that most likely not all values need to be sorted.
type iterativeSortStrategy struct {
	vNode      *valuesNode
	lastVal    tree.Datums
	nextRowIdx int
}

func newIterativeSortStrategy(vNode *valuesNode) sortingStrategy {
	return &iterativeSortStrategy{
		vNode: vNode,
	}
}

func (ss *iterativeSortStrategy) Add(ctx context.Context, values tree.Datums) error {
	_, err := ss.vNode.rows.AddRow(ctx, values)
	return err
}

func (ss *iterativeSortStrategy) Finish(context.Context, *sqlbase.CancelChecker) {
	ss.vNode.InitMinHeap()
}

func (ss *iterativeSortStrategy) Next(runParams) (bool, error) {
	if ss.vNode.Len() == 0 {
		return false, nil
	}
	ss.lastVal = ss.vNode.PopValues()
	ss.nextRowIdx++
	return true, nil
}

func (ss *iterativeSortStrategy) Values() tree.Datums {
	return ss.lastVal
}

func (ss *iterativeSortStrategy) Close(ctx context.Context) {
	ss.vNode.Close(ctx)
}

// sortTopKStrategy creates a max-heap in its wrapped valuesNode and keeps
// this heap populated with only the top k values seen. It accomplishes this
// by comparing new values (before the deep copy) with the top of the heap.
// If the new value is less than the current top, the top will be replaced
// and the heap will be fixed. If not, the new value is dropped. When finished,
// all values in the heap are popped, sorting the values correctly in-place.
// It has a worst-case time complexity of O(n*log(k)) and a worst-case space
// complexity of O(k).
//
// The strategy is intended to be used when exactly k values need to be sorted,
// where k is known before sorting begins.
//
// TODO(nvanbenschoten): There are better algorithms that can achieve a sorted
// top k in a worst-case time complexity of O(n + k*log(k)) while maintaining
// a worst-case space complexity of O(k). For instance, the top k can be found
// in linear time, and then this can be sorted in linearithmic time.
type sortTopKStrategy struct {
	vNode *valuesNode
	topK  int64
}

func newSortTopKStrategy(vNode *valuesNode, topK int64) sortingStrategy {
	ss := &sortTopKStrategy{
		vNode: vNode,
		topK:  topK,
	}
	ss.vNode.InitMaxHeap()
	return ss
}

func (ss *sortTopKStrategy) Add(ctx context.Context, values tree.Datums) error {
	switch {
	case int64(ss.vNode.Len()) < ss.topK:
		// The first k values all go into the max-heap.
		if err := ss.vNode.PushValues(ctx, values); err != nil {
			return err
		}
	case ss.vNode.ValuesLess(values, ss.vNode.rows.At(0)):
		// Once the heap is full, only replace the top
		// value if a new value is less than it. If so
		// replace and fix the heap.
		if err := ss.vNode.rows.Replace(ctx, 0, values); err != nil {
			return err
		}
		heap.Fix(ss.vNode, 0)
	}
	return nil
}

func (ss *sortTopKStrategy) Finish(ctx context.Context, cancelChecker *sqlbase.CancelChecker) {
	// Pop all values in the heap, resulting in the inverted ordering
	// being sorted in reverse. Therefore, the slice is ordered correctly
	// in-place.
	for ss.vNode.Len() > 0 {
		if cancelChecker.Check() != nil {
			return
		}
		heap.Pop(ss.vNode)
	}
	ss.vNode.ResetLen()
}

func (ss *sortTopKStrategy) Next(params runParams) (bool, error) {
	return ss.vNode.Next(params)
}

func (ss *sortTopKStrategy) Values() tree.Datums {
	return ss.vNode.Values()
}

func (ss *sortTopKStrategy) Close(ctx context.Context) {
	ss.vNode.Close(ctx)
}

// TODO(pmattis): If the result set is large, we might need to perform the
// sort on disk. There is no point in doing this while we're buffering the
// entire result set in memory. If/when we start streaming results back to
// the client we should revisit.
//
// type onDiskSortStrategy struct{}
