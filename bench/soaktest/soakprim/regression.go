// Package soakprim provides the framework for running soak tests
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package soakprim

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/NVIDIA/aistore/bench/soaktest/report"
	"github.com/NVIDIA/aistore/bench/soaktest/soakcmn"
	"github.com/NVIDIA/aistore/cmn"

	"github.com/NVIDIA/aistore/tutils"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/bench/soaktest/stats"
)

const (
	regBucketPrefix = "soaktest-reg"
)

var (
	bckName = fmt.Sprintf("%s-%d", regBucketPrefix, os.Getpid())
)

type regressionContext struct {
	wg     *sync.WaitGroup
	stopCh chan struct{}
}

// Regression runs a constant get request throughout the testing
func cleanupRegression() {
	names, err := api.GetBucketNames(tutils.BaseAPIParams(primaryURL), cmn.AIS)
	cmn.AssertNoErr(err)

	for _, name := range names.AIS {
		if strings.HasPrefix(name, regBucketPrefix) && (!soakcmn.Params.LocalCleanup || name == bckName) {
			api.DestroyBucket(tutils.BaseAPIParams(primaryURL), bckName)
		}
	}
}

func setupRegression() *regressionContext {
	regctx := &regressionContext{}

	report.Writef(report.ConsoleLevel, "Setting up regression (maximum %v)...\n", regCapacity)

	aisStopCh := make(chan *stats.PrimitiveStat, 1)
	params := &AISLoaderExecParams{
		pctput:       100,
		totalputsize: regCapacity,
		duration:     soakcmn.Params.RegSetupDuration,
		minsize:      soakcmn.Params.RegMinFilesize,
		maxsize:      soakcmn.Params.RegMaxFilesize,
	}
	AISExec(aisStopCh, soakcmn.OpTypePut, bckName, soakcmn.Params.RegSetupWorkers, params)
	setupStat := <-aisStopCh
	close(aisStopCh)

	report.Writef(report.ConsoleLevel, "Done setting up regression (actual size %v) ...\n", setupStat.TotalSize)

	return regctx
}

// Worker function for regression. Must call in go func
func regressionWorker(tag string, bucket string, stopCh chan struct{}, wg *sync.WaitGroup, recordRegression func(*stats.PrimitiveStat)) {
	aisLoaderExecParams := &AISLoaderExecParams{
		pctput:     0,
		stopable:   true,
		stopCh:     make(chan struct{}),
		verifyhash: false,

		duration:     0,
		totalputsize: 0,
	}

	aisExecResultCh := make(chan *stats.PrimitiveStat, 1)

	aisExecWg := &sync.WaitGroup{}
	aisExecWg.Add(1)

	go func() {
		defer aisExecWg.Done()
		AISExec(aisExecResultCh, soakcmn.OpTypeGet, bucket, soakcmn.Params.RegWorkers, aisLoaderExecParams)
	}()

	<-stopCh

	aisLoaderExecParams.stopCh <- struct{}{}
	aisExecWg.Wait()

	stat := <-aisExecResultCh
	stat.ID = tag
	recordRegression(stat)

	wg.Done()
}

func (rctx *RecipeContext) StartRegression() {
	// Record system stats when we start
	updateSysInfo()

	if rctx.regCtx == nil {
		rctx.regCtx = setupRegression()
	}

	regCtx := rctx.regCtx
	regCtx.stopCh = make(chan struct{})
	regCtx.wg = &sync.WaitGroup{}
	if rctx.repCtx == nil {
		rctx.repCtx = report.NewReportContext()
	}
	for i := 0; i < soakcmn.Params.RegInstances; i++ {
		tag := fmt.Sprintf("regression %d", i+1)
		regCtx.wg.Add(1)
		go regressionWorker(tag, bckName, regCtx.stopCh, regCtx.wg, rctx.repCtx.RecordRegression)
	}
}

func (rctx *RecipeContext) FinishRegression() {
	regCtx := rctx.regCtx

	if regCtx == nil {
		return
	}

	// Record system stats when we finish
	updateSysInfo()

	if regCtx.wg != nil {
		close(regCtx.stopCh)
		regCtx.wg.Wait()
	}
}
