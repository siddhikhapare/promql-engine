// Copyright (c) The Thanos Community Authors.
// Licensed under the Apache License 2.0.

package function

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/efficientgo/core/errors"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/thanos-io/promql-engine/execution/model"
	"github.com/thanos-io/promql-engine/execution/parse"
	"github.com/thanos-io/promql-engine/parser"
	"github.com/thanos-io/promql-engine/query"
)

// functionOperator returns []model.StepVector after processing input with desired function.
type functionOperator struct {
	funcExpr *parser.Call
	series   []labels.Labels
	once     sync.Once

	vectorIndex int
	nextOps     []model.VectorOperator

	call         functionCall
	scalarPoints [][]float64
}

func NewFunctionOperator(funcExpr *parser.Call, nextOps []model.VectorOperator, stepsBatch int, opts *query.Options) (model.VectorOperator, error) {
	// Some functions need to be handled in special operators
	switch funcExpr.Func.Name {
	case "scalar":
		return &scalarFunctionOperator{
			next: nextOps[0],
			pool: model.NewVectorPoolWithSize(stepsBatch, 1),
		}, nil
	case "label_join", "label_replace":
		return &relabelFunctionOperator{
			next:     nextOps[0],
			funcExpr: funcExpr,
		}, nil
	case "absent":
		return &absentOperator{
			next:     nextOps[0],
			pool:     model.NewVectorPool(stepsBatch),
			funcExpr: funcExpr,
		}, nil
	case "histogram_quantile":
		return &histogramOperator{
			pool:         model.NewVectorPool(stepsBatch),
			funcArgs:     funcExpr.Args,
			once:         sync.Once{},
			scalarOp:     nextOps[0],
			vectorOp:     nextOps[1],
			scalarPoints: make([]float64, stepsBatch),
		}, nil
	}

	// Short-circuit functions that take no args. Their only input is the step's timestamp.
	if len(nextOps) == 0 {
		return newNoArgsFunctionOperator(funcExpr, stepsBatch, opts)
	}
	// All remaining functions
	return newInstantVectorFunctionOperator(funcExpr, nextOps, stepsBatch, opts)
}

func newNoArgsFunctionOperator(funcExpr *parser.Call, stepsBatch int, opts *query.Options) (model.VectorOperator, error) {
	call, ok := noArgFuncs[funcExpr.Func.Name]
	if !ok {
		return nil, parse.UnknownFunctionError(funcExpr.Func)
	}

	interval := opts.Step.Milliseconds()
	// We set interval to be at least 1.
	if interval == 0 {
		interval = 1
	}

	op := &noArgFunctionOperator{
		currentStep: opts.Start.UnixMilli(),
		mint:        opts.Start.UnixMilli(),
		maxt:        opts.End.UnixMilli(),
		step:        interval,
		stepsBatch:  stepsBatch,
		funcExpr:    funcExpr,
		call:        call,
		vectorPool:  model.NewVectorPool(stepsBatch),
	}

	switch funcExpr.Func.Name {
	case "pi", "time":
		op.sampleIDs = []uint64{0}
	default:
		// Other functions require non-nil labels.
		op.series = []labels.Labels{{}}
		op.sampleIDs = []uint64{0}
	}
	return op, nil
}

func newInstantVectorFunctionOperator(funcExpr *parser.Call, nextOps []model.VectorOperator, stepsBatch int, opts *query.Options) (model.VectorOperator, error) {
	call, ok := instantVectorFuncs[funcExpr.Func.Name]
	if !ok {
		return nil, parse.UnknownFunctionError(funcExpr.Func)
	}

	scalarPoints := make([][]float64, stepsBatch)
	for i := 0; i < stepsBatch; i++ {
		scalarPoints[i] = make([]float64, len(nextOps)-1)
	}
	f := &functionOperator{
		nextOps:      nextOps,
		call:         call,
		funcExpr:     funcExpr,
		vectorIndex:  0,
		scalarPoints: scalarPoints,
	}

	for i := range funcExpr.Args {
		if funcExpr.Args[i].Type() == parser.ValueTypeVector {
			f.vectorIndex = i
			break
		}
	}

	// Check selector type.
	switch funcExpr.Args[f.vectorIndex].Type() {
	case parser.ValueTypeVector, parser.ValueTypeScalar:
		return f, nil
	default:
		return nil, errors.Wrapf(parse.ErrNotImplemented, "got %s:", funcExpr.String())
	}
}

func (o *functionOperator) Explain() (me string, next []model.VectorOperator) {
	return fmt.Sprintf("[*functionOperator] %v(%v)", o.funcExpr.Func.Name, o.funcExpr.Args), o.nextOps
}

func (o *functionOperator) Series(ctx context.Context) ([]labels.Labels, error) {
	if err := o.loadSeries(ctx); err != nil {
		return nil, err
	}

	return o.series, nil
}

func (o *functionOperator) GetPool() *model.VectorPool {
	return o.nextOps[o.vectorIndex].GetPool()
}

func (o *functionOperator) Next(ctx context.Context) ([]model.StepVector, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if err := o.loadSeries(ctx); err != nil {
		return nil, err
	}

	// Process non-variadic single/multi-arg instant vector and scalar input functions.
	// Call next on vector input.
	vectors, err := o.nextOps[o.vectorIndex].Next(ctx)
	if err != nil {
		return nil, err
	}

	if len(vectors) == 0 {
		return nil, nil
	}
	scalarIndex := 0
	for i := range o.nextOps {
		if i == o.vectorIndex {
			continue
		}

		scalarVectors, err := o.nextOps[i].Next(ctx)
		if err != nil {
			return nil, err
		}

		for batchIndex := range vectors {
			val := math.NaN()
			if len(scalarVectors) > 0 && len(scalarVectors[batchIndex].Samples) > 0 {
				val = scalarVectors[batchIndex].Samples[0]
				o.nextOps[i].GetPool().PutStepVector(scalarVectors[batchIndex])
			}
			o.scalarPoints[batchIndex][scalarIndex] = val
		}
		o.nextOps[i].GetPool().PutVectors(scalarVectors)
		scalarIndex++
	}
	for batchIndex, vector := range vectors {
		i := 0
		for i < len(vectors[batchIndex].Samples) {
			if v, ok := o.call(vector.Samples[i], nil, o.scalarPoints[batchIndex]...); ok {
				vector.Samples[i] = v
				i++
			} else {
				// This operator modifies samples directly in the input vector to avoid allocations.
				// In case of an invalid output sample, we need to do an in-place removal of the input sample.
				vectors[batchIndex].RemoveSample(i)
			}
		}

		i = 0
		for i < len(vectors[batchIndex].Histograms) {
			v, ok := o.call(0., vector.Histograms[i], o.scalarPoints[batchIndex]...)
			// This operator modifies samples directly in the input vector to avoid allocations.
			// All current functions for histograms produce a float64 sample. It's therefore safe to
			// always remove the input histogram so that it does not propagate to the output.
			sampleID := vectors[batchIndex].HistogramIDs[i]
			vectors[batchIndex].RemoveHistogram(i)
			if ok {
				vectors[batchIndex].AppendSample(o.GetPool(), sampleID, v)
			}
		}
	}

	return vectors, nil
}

func (o *functionOperator) loadSeries(ctx context.Context) error {
	var err error
	o.once.Do(func() {
		if o.funcExpr.Func.Name == "vector" {
			o.series = []labels.Labels{labels.New()}
			return
		}

		series, loadErr := o.nextOps[o.vectorIndex].Series(ctx)
		if loadErr != nil {
			err = loadErr
			return
		}
		o.series = make([]labels.Labels, len(series))

		b := labels.ScratchBuilder{}
		for i, s := range series {
			lbls, _ := DropMetricName(s, b)
			o.series[i] = lbls
		}
	})

	return err
}

func DropMetricName(l labels.Labels, b labels.ScratchBuilder) (labels.Labels, labels.Label) {
	return dropLabel(l, labels.MetricName, b)
}

// dropLabel removes the label with name from l and returns the dropped label.
func dropLabel(l labels.Labels, name string, b labels.ScratchBuilder) (labels.Labels, labels.Label) {
	var ret labels.Label

	if l.IsEmpty() {
		return l, labels.Label{}
	}

	b.Reset()

	l.Range(func(l labels.Label) {
		if l.Name == name {
			ret = l
			return
		}

		b.Add(l.Name, l.Value)
	})

	return b.Labels(), ret
}
