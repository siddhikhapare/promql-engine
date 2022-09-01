package executionplan

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
)

type VectorOperator interface {
	Next(ctx context.Context) (promql.Vector, error)
}

type MatrixOperator interface {
	Next(ctx context.Context) (promql.Matrix, error)
}

func New(expr parser.Expr, storage storage.Storage, mint, maxt time.Time, step time.Duration) (VectorOperator, error) {
	return newOperator(expr, storage, mint, maxt, step)
}

func newOperator(expr parser.Expr, storage storage.Storage, mint, maxt time.Time, step time.Duration) (VectorOperator, error) {
	switch e := expr.(type) {
	case *parser.AggregateExpr:
		next, err := newOperator(e.Expr, storage, mint, maxt, step)
		if err != nil {
			return nil, err
		}
		aggregate, err := NewAggregate(next, e.Op, !e.Without, e.Grouping)
		if err != nil {
			return nil, err
		}
		return concurrent(aggregate), nil

	case *parser.VectorSelector:
		selector := NewVectorSelector(storage, e.LabelMatchers, nil, mint, maxt, step)
		return concurrent(selector), nil
	case *parser.Call:
		switch t := e.Args[0].(type) {
		case *parser.MatrixSelector:
			vs := t.VectorSelector.(*parser.VectorSelector)
			selector := NewMatrixSelector(storage, vs.LabelMatchers, nil, mint, maxt, step, t.Range)
			return concurrent(selector), nil
		}
		return nil, fmt.Errorf("unsupported expression %s", e)
	default:
		return nil, fmt.Errorf("unsupported expression %s", e)
	}
}
