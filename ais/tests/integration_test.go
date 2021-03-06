// Package ais_test contains AIS integration tests.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais_test

import (
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/containers"
	"github.com/NVIDIA/aistore/tutils"
	"github.com/NVIDIA/aistore/tutils/tassert"
	jsoniter "github.com/json-iterator/go"
)

const rebalanceObjectDistributionTestCoef = 0.3

type repFile struct {
	repetitions int
	filename    string
}

type ioContext struct {
	t                   *testing.T
	smap                *cluster.Smap
	semaphore           chan struct{}
	controlCh           chan struct{}
	stopCh              chan struct{}
	repFilenameCh       chan repFile
	wg                  *sync.WaitGroup
	bucket              string
	fileSize            uint64
	numGetErrs          atomic.Uint64
	getsCompleted       atomic.Uint64
	proxyURL            string
	otherTasksToTrigger int
	originalTargetCount int
	originalProxyCount  int
	num                 int
	numGetsEachFile     int
	getErrIsFatal       bool
}

func (m *ioContext) saveClusterState() {
	m.init()
	m.smap = getClusterMap(m.t, m.proxyURL)
	m.originalTargetCount = len(m.smap.Tmap)
	m.originalProxyCount = len(m.smap.Pmap)
	tutils.Logf("targets: %d, proxies: %d\n", m.originalTargetCount, m.originalProxyCount)
}

func (m *ioContext) init() {
	m.proxyURL = getPrimaryURL(m.t, proxyURLReadOnly)
	if m.fileSize == 0 {
		m.fileSize = cmn.KiB
	}
	if m.num > 0 {
		m.repFilenameCh = make(chan repFile, m.num)
	}
	if m.otherTasksToTrigger > 0 {
		m.controlCh = make(chan struct{}, m.otherTasksToTrigger)
	}
	m.semaphore = make(chan struct{}, 10) // 10 concurrent GET requests at a time
	m.wg = &sync.WaitGroup{}
	if m.bucket == "" {
		m.bucket = m.t.Name() + "Bucket"
	}
	m.stopCh = make(chan struct{})
}

func (m *ioContext) assertClusterState() {
	smap, err := tutils.WaitForPrimaryProxy(
		m.proxyURL,
		"to check cluster state",
		m.smap.Version, testing.Verbose(),
		m.originalProxyCount,
		m.originalTargetCount,
	)
	tassert.CheckFatal(m.t, err)

	proxyCount := smap.CountProxies()
	targetCount := smap.CountTargets()
	if targetCount != m.originalTargetCount ||
		proxyCount != m.originalProxyCount {
		m.t.Errorf(
			"cluster state is not preserved. targets (before: %d, now: %d); proxies: (before: %d, now: %d)",
			targetCount, m.originalTargetCount,
			proxyCount, m.originalProxyCount,
		)
	}
}

func (m *ioContext) checkObjectDistribution(t *testing.T) {
	var (
		requiredCount     = int64(rebalanceObjectDistributionTestCoef * (float64(m.num) / float64(m.originalTargetCount)))
		targetObjectCount = make(map[string]int64)
	)
	tutils.Logf("Checking if each target has a required number of object in bucket %s...\n", m.bucket)
	baseParams := tutils.BaseAPIParams(m.proxyURL)
	bucketList, err := api.ListBucket(baseParams, m.bucket, &cmn.SelectMsg{Props: cmn.GetTargetURL}, 0)
	tassert.CheckFatal(t, err)
	for _, obj := range bucketList.Entries {
		targetObjectCount[obj.TargetURL]++
	}
	if len(targetObjectCount) != m.originalTargetCount {
		t.Fatalf("Rebalance error, %d/%d targets received no objects from bucket %s\n",
			m.originalTargetCount-len(targetObjectCount), m.originalTargetCount, m.bucket)
	}
	for targetURL, objCount := range targetObjectCount {
		if objCount < requiredCount {
			t.Fatalf("Rebalance error, target %s didn't receive required number of objects\n", targetURL)
		}
	}
}

func (m *ioContext) puts(dontFail ...bool) int {
	sgl := tutils.Mem2.NewSGL(int64(m.fileSize))
	defer sgl.Free()

	filenameCh := make(chan string, m.num)
	errCh := make(chan error, m.num)

	tutils.Logf("PUT %d objects into bucket %s...\n", m.num, m.bucket)
	tutils.PutRandObjs(m.proxyURL, m.bucket, SmokeDir, readerType, SmokeStr, m.fileSize, m.num, errCh, filenameCh, sgl)
	if len(dontFail) == 0 {
		selectErr(errCh, "put", m.t, false)
	}
	close(filenameCh)
	close(errCh)
	for f := range filenameCh {
		m.repFilenameCh <- repFile{repetitions: m.numGetsEachFile, filename: f}
	}
	return len(errCh)
}

func (m *ioContext) cloudPuts() {
	var (
		baseParams = tutils.DefaultBaseAPIParams(m.t)
		msg        = &cmn.SelectMsg{}
	)

	tutils.Logf("cloud PUT %d objects into bucket %s...\n", m.num, m.bucket)

	objList, err := api.ListBucket(baseParams, m.bucket, msg, 0)
	tassert.CheckFatal(m.t, err)

	leftToFill := m.num - len(objList.Entries)
	if leftToFill <= 0 {
		tutils.Logf("cloud PUT %d (%d) objects already in bucket %s...\n", m.num, len(objList.Entries), m.bucket)
		m.num = len(objList.Entries)
		return
	}

	// Not enough objects in cloud bucket, need to create more.
	var (
		errCh = make(chan error, leftToFill)
		wg    = &sync.WaitGroup{}
	)
	for i := 0; i < leftToFill; i++ {
		r, err := tutils.NewRandReader(512 /*size*/, true /*withHash*/)
		tassert.CheckFatal(m.t, err)
		objName := fmt.Sprintf("%s%s%d", "copy/cloud_", cmn.RandString(4), i)
		wg.Add(1)
		go tutils.PutAsync(wg, m.proxyURL, m.bucket, objName, r, errCh)
	}
	wg.Wait()
	selectErr(errCh, "put", m.t, true)
	tutils.Logln("cloud PUT done")

	objList, err = api.ListBucket(baseParams, m.bucket, msg, 0)
	tassert.CheckFatal(m.t, err)
	if len(objList.Entries) != m.num {
		m.t.Fatalf("list-bucket err: %d != %d", len(objList.Entries), m.num)
	}

	tutils.Logf("cloud bucket %s: %d cached objects\n", m.bucket, m.num)
}

func (m *ioContext) cloudPrefetch(prefetchCnt int) {
	var (
		baseParams = tutils.DefaultBaseAPIParams(m.t)
		msg        = &cmn.SelectMsg{}
	)

	objList, err := api.ListBucket(baseParams, m.bucket, msg, 0)
	tassert.CheckFatal(m.t, err)

	tutils.Logf("cloud PREFETCH %d objects...\n", prefetchCnt)

	wg := &sync.WaitGroup{}
	for idx, obj := range objList.Entries {
		if idx >= prefetchCnt {
			break
		}

		wg.Add(1)
		go func(obj *cmn.BucketEntry) {
			_, err := tutils.GetDiscard(m.proxyURL, m.bucket, cmn.Cloud, obj.Name, false /*validate*/, 0, 0)
			tassert.CheckError(m.t, err)
			wg.Done()
		}(obj)
	}
	wg.Wait()
}

func (m *ioContext) cloudDelete() {
	var (
		baseParams = tutils.DefaultBaseAPIParams(m.t)
		msg        = &cmn.SelectMsg{}
		sema       = make(chan struct{}, 40)
	)

	objList, err := api.ListBucket(baseParams, m.bucket, msg, 0)
	tassert.CheckFatal(m.t, err)

	tutils.Logln("deleting cloud objects...")

	wg := &sync.WaitGroup{}
	for _, obj := range objList.Entries {
		wg.Add(1)
		go func(obj *cmn.BucketEntry) {
			sema <- struct{}{}
			defer func() {
				<-sema
			}()
			err := api.DeleteObject(baseParams, m.bucket, obj.Name, cmn.Cloud)
			tassert.CheckError(m.t, err)
			wg.Done()
		}(obj)
	}
	wg.Wait()
}

func (m *ioContext) gets() {
	for i := 0; i < 10; i++ {
		m.semaphore <- struct{}{}
	}
	if m.numGetsEachFile == 1 {
		tutils.Logf("GET each of the %d objects from bucket %s...\n", m.num, m.bucket)
	} else {
		tutils.Logf("GET each of the %d objects %d times from bucket %s...\n", m.num, m.numGetsEachFile, m.bucket)
	}
	baseParams := tutils.DefaultBaseAPIParams(m.t)
	for i := 0; i < m.num*m.numGetsEachFile; i++ {
		go func() {
			<-m.semaphore
			defer func() {
				m.semaphore <- struct{}{}
				m.wg.Done()
			}()
			repFile := <-m.repFilenameCh
			if repFile.repetitions > 0 {
				repFile.repetitions--
				m.repFilenameCh <- repFile
			}
			_, err := api.GetObject(baseParams, m.bucket, path.Join(SmokeStr, repFile.filename))
			if err != nil {
				if m.getErrIsFatal {
					m.t.Error(err)
				}
				m.numGetErrs.Inc()
			}
			if m.getErrIsFatal && m.numGetErrs.Load() > 0 {
				return
			}
			g := m.getsCompleted.Inc()

			if g%5000 == 0 {
				tutils.Logf(" %d/%d GET requests completed...\n", g, m.num*m.numGetsEachFile)
			}

			// Tell other tasks they can begin to do work in parallel
			if int(g) == m.num*m.numGetsEachFile/2 {
				for i := 0; i < m.otherTasksToTrigger; i++ {
					m.controlCh <- struct{}{}
				}
			}
		}()
	}
}

func (m *ioContext) getsUntilStop() {
	for i := 0; i < 10; i++ {
		m.semaphore <- struct{}{}
	}
	baseParams := tutils.DefaultBaseAPIParams(m.t)
	i := 0
	for {
		select {
		case <-m.stopCh:
			return
		default:
			m.wg.Add(1)
			go func() {
				<-m.semaphore
				defer func() {
					m.semaphore <- struct{}{}
					m.wg.Done()
				}()
				repFile := <-m.repFilenameCh
				m.repFilenameCh <- repFile
				_, err := api.GetObject(baseParams, m.bucket, path.Join(SmokeStr, repFile.filename))
				if err != nil {
					if m.getErrIsFatal {
						m.t.Error(err)
					}
					m.numGetErrs.Inc()
				}
				if m.getErrIsFatal && m.numGetErrs.Load() > 0 {
					return
				}
				g := m.getsCompleted.Inc()
				if g%5000 == 0 {
					tutils.Logf(" %d GET requests completed...\n", g)
				}
			}()

			i++
			if i%5000 == 0 {
				time.Sleep(500 * time.Millisecond) // prevents generating too many GET requests
			}
		}
	}
}

func (m *ioContext) stopGets() {
	m.stopCh <- struct{}{}
}

func (m *ioContext) ensureNumCopies(expectedCopies int) {
	var (
		total      int
		baseParams = tutils.DefaultBaseAPIParams(m.t)
	)

	time.Sleep(3 * time.Second)
	waitForBucketXactionToComplete(m.t, cmn.ActMakeNCopies /*kind*/, m.bucket, baseParams, rebalanceTimeout)

	// List Bucket - primarily for the copies
	query := make(url.Values)
	msg := &cmn.SelectMsg{Cached: true}
	msg.AddProps(cmn.GetPropsCopies, cmn.GetPropsAtime, cmn.GetPropsStatus)
	objectList, err := api.ListBucket(baseParams, m.bucket, msg, 0, query)
	tassert.CheckFatal(m.t, err)

	copiesToNumObjects := make(map[int]int)
	for _, entry := range objectList.Entries {
		if entry.Atime == "" {
			m.t.Errorf("%s/%s: access time is empty", m.bucket, entry.Name)
		}
		total++
		copiesToNumObjects[int(entry.Copies)]++
	}
	tutils.Logf("objects (total, copies) = (%d, %v)\n", total, copiesToNumObjects)
	if total != m.num {
		m.t.Fatalf("listbucket: expecting %d objects, got %d", m.num, total)
	}

	if len(copiesToNumObjects) != 1 {
		s, _ := jsoniter.MarshalIndent(copiesToNumObjects, "", " ")
		m.t.Fatalf("some objects do not have expected number of copies: %s", s)
	}

	for copies := range copiesToNumObjects {
		if copies != expectedCopies {
			m.t.Fatalf("Expecting %d objects all to have %d replicas, got: %d", total, expectedCopies, copies)
		}
	}
}

func (m *ioContext) ensureNoErrors() {
	if m.numGetErrs.Load() > 0 {
		m.t.Fatalf("Number of get errors is non-zero: %d\n", m.numGetErrs.Load())
	}
}

func (m *ioContext) reregisterTarget(target *cluster.Snode) {
	const (
		timeout    = time.Second * 10
		interval   = time.Millisecond * 10
		iterations = int(timeout / interval)
	)

	// T1
	tutils.Logf("Registering target %s...\n", target.DaemonID)
	smap := getClusterMap(m.t, m.proxyURL)
	err := tutils.RegisterNode(m.proxyURL, target, smap)
	tassert.CheckFatal(m.t, err)
	baseParams := tutils.BaseAPIParams(target.URL(cmn.NetworkPublic))
	for i := 0; i < iterations; i++ {
		time.Sleep(interval)
		if _, ok := smap.Tmap[target.DaemonID]; !ok {
			// T2
			smap = getClusterMap(m.t, m.proxyURL)
			if _, ok := smap.Tmap[target.DaemonID]; ok {
				tutils.Logf("T2: registered target %s\n", target.DaemonID)
			}
		} else {
			baseParams.URL = m.proxyURL
			proxyLBNames, err := api.GetBucketNames(baseParams, cmn.AIS)
			tassert.CheckFatal(m.t, err)

			baseParams.URL = target.URL(cmn.NetworkPublic)
			targetLBNames, err := api.GetBucketNames(baseParams, cmn.AIS)
			tassert.CheckFatal(m.t, err)
			// T3
			if cmn.StrSlicesEqual(proxyLBNames.AIS, targetLBNames.AIS) {
				tutils.Logf("T3: registered target %s got updated with the new BMD\n", target.DaemonID)
				return
			}
		}
	}

	m.t.Fatalf("failed to register target %s: not in the Smap or did not receive BMD", target.DaemonID)
}

func (m *ioContext) setRandBucketProps() {
	baseParams := tutils.DefaultBaseAPIParams(m.t)

	// Set some weird bucket props to see if they were changed or not.
	props := cmn.BucketPropsToUpdate{
		LRU: &cmn.LRUConfToUpdate{
			LowWM:  api.Int64(int64(rand.Intn(35) + 1)),
			HighWM: api.Int64(int64(rand.Intn(15) + 40)),
		},
	}
	err := api.SetBucketProps(baseParams, m.bucket, props)
	tassert.CheckFatal(m.t, err)
}

// Intended for a deployment with multiple targets
// 1. Unregister target T
// 2. Create ais bucket
// 3. PUT large amount of objects into the ais bucket
// 4. GET the objects while simultaneously registering the target T
func TestGetAndReRegisterInParallel(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             50000,
			numGetsEachFile: 3,
			fileSize:        10 * cmn.KiB,
		}
	)

	// Step 1.
	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Fatalf("Must have 2 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	// Step 2.
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	target := tutils.ExtractTargetNodes(m.smap)[0]
	err := tutils.UnregisterNode(m.proxyURL, target.DaemonID)
	tassert.CheckFatal(t, err)

	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}
	tutils.Logf("Unregistered target %s: the cluster now has %d targets\n", target.URL(cmn.NetworkPublic), n)

	// Step 3.
	m.puts()

	// Step 4.
	m.wg.Add(m.num*m.numGetsEachFile + 2)
	go func() {
		// without defer, if gets crashes Done is not called resulting in test hangs
		defer m.wg.Done()
		m.gets()
	}()

	time.Sleep(time.Second * 3) // give gets some room to breathe
	go func() {
		// without defer, if reregister crashes Done is not called resulting in test hangs
		defer m.wg.Done()
		m.reregisterTarget(target)
	}()

	m.wg.Wait()

	m.ensureNoErrors()
	m.assertClusterState()
}

// All of the above PLUS proxy failover/failback sequence in parallel
// Namely:
// 1. Unregister a target
// 2. Create an ais bucket
// 3. Crash the primary proxy and PUT in parallel
// 4. Failback to the original primary proxy, register target, and GET in parallel
func TestProxyFailbackAndReRegisterInParallel(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:                   t,
			otherTasksToTrigger: 1,
			num:                 150000,
			numGetsEachFile:     1,
		}
	)

	// Step 1.
	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Fatalf("Must have 2 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	if m.originalProxyCount < 3 {
		t.Fatalf("Must have 3 or more proxies/gateways in the cluster, have only %d", m.originalProxyCount)
	}

	// Step 2.
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	target := tutils.ExtractTargetNodes(m.smap)[0]
	err := tutils.UnregisterNode(m.proxyURL, target.DaemonID)
	tassert.CheckFatal(t, err)
	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}
	tutils.Logf("Unregistered target %s: the cluster now has %d targets\n", target.URL(cmn.NetworkPublic), n)

	// Step 3.
	_, newPrimaryURL, err := chooseNextProxy(m.smap)
	// use a new proxyURL because primaryCrashElectRestart has a side-effect:
	// it changes the primary proxy. Without the change tutils.PutRandObjs is
	// failing while the current primary is restarting and rejoining
	m.proxyURL = newPrimaryURL
	tassert.CheckFatal(t, err)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		primaryCrashElectRestart(t)
	}()

	// PUT phase is timed to ensure it doesn't finish before primaryCrashElectRestart() begins
	time.Sleep(5 * time.Second)
	m.puts()
	m.wg.Wait()

	// Step 4.

	// m.num*m.numGetsEachFile is for `gets` and +2 is for goroutines
	// below (one for reregisterTarget and second for primarySetToOriginal)
	m.wg.Add(m.num*m.numGetsEachFile + 2)

	go func() {
		defer m.wg.Done()

		m.reregisterTarget(target)
	}()

	go func() {
		defer m.wg.Done()

		<-m.controlCh
		primarySetToOriginal(t)
	}()

	m.gets()

	m.wg.Wait()
	m.ensureNoErrors()
	m.assertClusterState()
}

// Similar to TestGetAndReRegisterInParallel, but instead of unregister, we kill the target
// 1. Kill registered target and wait for Smap to updated
// 2. Create ais bucket
// 3. PUT large amounts of objects into ais bucket
// 4. Get the objects while simultaneously registering the target
func TestGetAndRestoreInParallel(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             20000,
			numGetsEachFile: 5,
			fileSize:        cmn.KiB * 2,
		}
		targetURL  string
		targetPort string
		targetID   string
	)

	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	// Step 1
	// Select a random target
	for _, v := range m.smap.Tmap {
		targetURL = v.PublicNet.DirectURL
		targetPort = v.PublicNet.DaemonPort
		targetID = v.DaemonID
		break
	}
	tutils.Logf("Killing target: %s - %s\n", targetURL, targetID)
	tcmd, targs, err := kill(targetID, targetPort)
	tassert.CheckFatal(t, err)

	primaryProxy := getPrimaryURL(m.t, proxyURLReadOnly)
	m.smap, err = tutils.WaitForPrimaryProxy(primaryProxy, "to update smap", m.smap.Version, testing.Verbose(), m.originalProxyCount, m.originalTargetCount-1)
	tassert.CheckError(t, err)

	// Step 2
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Step 3
	m.puts()

	// Step 4
	m.wg.Add(m.num*m.numGetsEachFile + 1)
	go func() {
		defer m.wg.Done()

		time.Sleep(4 * time.Second)
		restore(tcmd, targs, false, "target")
	}()

	m.gets()

	m.wg.Wait()
	m.ensureNoErrors()
	m.assertClusterState()
}

func TestUnregisterPreviouslyUnregisteredTarget(t *testing.T) {
	var (
		m = ioContext{
			t: t,
		}
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Fatalf("Must have 2 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	tutils.Logf("Num targets %d, num proxies %d\n", m.originalTargetCount, m.originalProxyCount)

	target := tutils.ExtractTargetNodes(m.smap)[0]
	// Unregister target
	err := tutils.UnregisterNode(m.proxyURL, target.DaemonID)
	tassert.CheckFatal(t, err)
	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}
	tutils.Logf("Unregistered target %s: the cluster now has %d targets\n", target.URL(cmn.NetworkPublic), n)

	// Unregister same target again
	err = tutils.UnregisterNode(m.proxyURL, target.DaemonID)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal("Unregistering the same target twice must return error 404")
	}
	n = getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}

	// Register target (bring cluster to normal state)
	m.reregisterTarget(target)
	m.assertClusterState()
}

func TestRegisterAndUnregisterTargetAndPutInParallel(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:   t,
			num: 10000,
		}
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	targets := tutils.ExtractTargetNodes(m.smap)

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Unregister target 0
	err := tutils.UnregisterNode(m.proxyURL, targets[0].DaemonID)
	tassert.CheckFatal(t, err)
	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}

	// Do puts in parallel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		m.puts()
	}()

	// Register target 0 in parallel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		tutils.Logf("Register target %s\n", targets[0].URL(cmn.NetworkPublic))
		err = tutils.RegisterNode(m.proxyURL, targets[0], m.smap)
		tassert.CheckFatal(t, err)
	}()

	// Unregister target 1 in parallel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		tutils.Logf("Unregister target %s\n", targets[1].URL(cmn.NetworkPublic))
		err = tutils.UnregisterNode(m.proxyURL, targets[1].DaemonID)
		tassert.CheckFatal(t, err)
	}()

	// Wait for everything to end
	m.wg.Wait()

	// Register target 1 to bring cluster to original state
	m.reregisterTarget(targets[1])

	// wait for rebalance to complete
	baseParams := tutils.BaseAPIParams(m.proxyURL)
	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)

	m.assertClusterState()
}

func TestAckRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		md = ioContext{
			t:               t,
			num:             30000,
			numGetsEachFile: 1,
			getErrIsFatal:   true,
		}
	)

	// Init. ioContext
	md.saveClusterState()

	if md.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", md.originalTargetCount)
	}
	target := tutils.ExtractTargetNodes(md.smap)[0]

	// Create ais bucket
	tutils.CreateFreshBucket(t, md.proxyURL, md.bucket)
	defer tutils.DestroyBucket(t, md.proxyURL, md.bucket)

	// Unregister a target
	tutils.Logf("Unregister target: %s\n", target.URL(cmn.NetworkPublic))
	err := tutils.UnregisterNode(md.proxyURL, target.DaemonID)
	tassert.CheckFatal(t, err)
	n := len(getClusterMap(t, md.proxyURL).Tmap)
	if n != md.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", md.originalTargetCount-1, n)
	}

	// Start putting files into bucket
	md.puts()

	tutils.Logf("Register target %s\n", target.URL(cmn.NetworkPublic))
	err = tutils.RegisterNode(md.proxyURL, target, md.smap)
	tassert.CheckFatal(t, err)

	// wait for everything to finish
	baseParams := tutils.BaseAPIParams(md.proxyURL)
	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)
	md.wg.Wait()

	md.wg.Add(md.num * md.numGetsEachFile)
	md.gets()
	md.wg.Wait()

	md.ensureNoErrors()
	md.assertClusterState()
}

func TestStressRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}
	rand := rand.New(rand.NewSource(time.Now().UnixNano()))
	max := 3
	for i := 1; i <= max; i++ {
		tutils.Logf("Test #%d ======\n", i)
		testStressRebalance(t, rand, i == 1, i == max)
	}
}

func testStressRebalance(t *testing.T, rand *rand.Rand, createlb, destroylb bool) {
	var (
		md = ioContext{
			t:               t,
			num:             50000,
			numGetsEachFile: 1,
			getErrIsFatal:   true,
		}
	)
	md.saveClusterState()
	if md.originalTargetCount < 4 {
		t.Fatalf("Must have 4 or more targets in the cluster, have only %d", md.originalTargetCount)
	}
	tgts := tutils.ExtractTargetNodes(md.smap)
	i1 := rand.Intn(len(tgts))
	i2 := (i1 + 1) % len(tgts)
	target1, target2 := tgts[i1], tgts[i2]

	// Create ais bucket
	if createlb {
		tutils.CreateFreshBucket(t, md.proxyURL, md.bucket)
	}
	if destroylb {
		defer tutils.DestroyBucket(t, md.proxyURL, md.bucket)
	}

	// Unregister a target
	tutils.Logf("Unregister targets: %s and %s\n", target1.URL(cmn.NetworkPublic), target2.URL(cmn.NetworkPublic))
	err := tutils.UnregisterNode(md.proxyURL, target1.DaemonID)
	tassert.CheckFatal(t, err)
	time.Sleep(time.Second)
	err = tutils.UnregisterNode(md.proxyURL, target2.DaemonID)
	tassert.CheckFatal(t, err)
	n := len(getClusterMap(t, md.proxyURL).Tmap)
	if n != md.originalTargetCount-2 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", md.originalTargetCount-2, n)
	}

	// Start putting files into bucket
	md.puts()

	// read in parallel
	md.wg.Add(md.num * md.numGetsEachFile)
	md.gets() // TODO: add m.getAll() method to GET both already existing and recently added

	// and join 2 targets in parallel
	tutils.Logf("Register 1st target %s\n", target1.URL(cmn.NetworkPublic))
	err = tutils.RegisterNode(md.proxyURL, target1, md.smap)
	tassert.CheckFatal(t, err)

	// random sleep between the first and the second join
	time.Sleep(time.Duration(rand.Intn(8)) * time.Second)

	tutils.Logf("Register 2nd target %s\n", target2.URL(cmn.NetworkPublic))
	err = tutils.RegisterNode(md.proxyURL, target2, md.smap)
	tassert.CheckFatal(t, err)

	// wait for the rebalance to finish
	baseParams := tutils.BaseAPIParams(md.proxyURL)
	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)

	// wait for the reads to run out
	md.wg.Wait()

	md.ensureNoErrors()
	md.assertClusterState()
}

func TestRebalanceAfterUnregisterAndReregister(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             10000,
			numGetsEachFile: 1,
		}
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	targets := tutils.ExtractTargetNodes(m.smap)

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Unregister target 0
	err := tutils.UnregisterNode(m.proxyURL, targets[0].DaemonID)
	tassert.CheckFatal(t, err)
	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}

	// Put some files
	m.puts()

	// Register target 0 in parallel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		tutils.Logf("Register target %s\n", targets[0].URL(cmn.NetworkPublic))
		err = tutils.RegisterNode(m.proxyURL, targets[0], m.smap)
		tassert.CheckFatal(t, err)
	}()

	// Unregister target 1 in parallel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		tutils.Logf("Unregister target %s\n", targets[1].URL(cmn.NetworkPublic))
		err = tutils.UnregisterNode(m.proxyURL, targets[1].DaemonID)
		tassert.CheckFatal(t, err)
	}()

	// Wait for everything to end
	m.wg.Wait()

	// Register target 1 to bring cluster to original state
	m.reregisterTarget(targets[1])

	baseParams := tutils.BaseAPIParams(m.proxyURL)
	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)

	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()

	m.ensureNoErrors()
	m.assertClusterState()
}

func TestPutDuringRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             10000,
			numGetsEachFile: 1,
		}
	)

	// Init. ioContext
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	target := tutils.ExtractTargetNodes(m.smap)[0]

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Unregister a target
	tutils.Logf("Unregister target %s\n", target.URL(cmn.NetworkPublic))
	err := tutils.UnregisterNode(m.proxyURL, target.DaemonID)
	tassert.CheckFatal(t, err)
	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}

	// Start putting files and register target in parallel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		// sleep some time to wait for PUT operations to begin
		time.Sleep(3 * time.Second)
		tutils.Logf("Register target %s\n", target.URL(cmn.NetworkPublic))
		err = tutils.RegisterNode(m.proxyURL, target, m.smap)
		tassert.CheckFatal(t, err)
	}()

	m.puts()

	// Wait for everything to finish
	m.wg.Wait()
	baseParams := tutils.BaseAPIParams(m.proxyURL)
	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)

	// main check - try to read all objects
	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()

	m.checkObjectDistribution(t)
	m.assertClusterState()
}

func TestGetDuringLocalAndGlobalRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             10000,
			numGetsEachFile: 3,
		}
		baseParams     = tutils.DefaultBaseAPIParams(t)
		selectedTarget *cluster.Snode
		killTarget     *cluster.Snode
	)

	// Init. ioContext
	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Fatalf("Must have at least 2 target in the cluster")
	}

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// select a random target to disable one of its mountpaths,
	// and another random target to unregister
	for _, target := range m.smap.Tmap {
		if selectedTarget == nil {
			selectedTarget = target
		} else {
			killTarget = target
			break
		}
	}
	mpList, err := api.GetMountpaths(baseParams, selectedTarget)
	tassert.CheckFatal(t, err)

	if len(mpList.Available) < 2 {
		t.Fatalf("Must have at least 2 mountpaths")
	}

	// Disable mountpaths temporarily
	mpath := mpList.Available[0]
	tutils.Logf("Disable mountpath on target %s\n", selectedTarget.ID())
	err = api.DisableMountpath(baseParams, selectedTarget.ID(), mpath)
	tassert.CheckFatal(t, err)

	// Unregister another target
	tutils.Logf("Unregister target %s\n", killTarget.URL(cmn.NetworkPublic))
	err = tutils.UnregisterNode(m.proxyURL, killTarget.DaemonID)
	tassert.CheckFatal(t, err)
	smap, err := tutils.WaitForPrimaryProxy(
		m.proxyURL,
		"target is gone",
		m.smap.Version, testing.Verbose(),
		m.originalProxyCount,
		m.originalTargetCount-1,
	)
	tassert.CheckFatal(m.t, err)

	m.puts()

	// Start getting objects
	m.wg.Add(m.num * m.numGetsEachFile)
	go func() {
		m.gets()
	}()

	// Let's give gets some momentum
	time.Sleep(time.Second * 4)

	// register a new target
	err = tutils.RegisterNode(m.proxyURL, killTarget, m.smap)
	tassert.CheckFatal(t, err)

	// enable mountpath
	err = api.EnableMountpath(baseParams, selectedTarget, mpath)
	tassert.CheckFatal(t, err)

	// wait until GETs are done while 2 rebalance are running
	m.wg.Wait()

	// make sure that the cluster has all targets enabled
	_, err = tutils.WaitForPrimaryProxy(
		m.proxyURL,
		"to join target back",
		smap.Version, testing.Verbose(),
		m.originalProxyCount,
		m.originalTargetCount,
	)
	tassert.CheckFatal(m.t, err)

	mpListAfter, err := api.GetMountpaths(baseParams, selectedTarget)
	tassert.CheckFatal(t, err)
	if len(mpList.Available) != len(mpListAfter.Available) {
		t.Fatalf("Some mountpaths failed to enable: the number before %d, after %d",
			len(mpList.Available), len(mpListAfter.Available))
	}

	// wait for rebalance to complete
	baseParams = tutils.BaseAPIParams(m.proxyURL)
	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)

	m.ensureNoErrors()
	m.assertClusterState()
}

func TestGetDuringLocalRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             20000,
			numGetsEachFile: 1,
		}
		baseParams = tutils.DefaultBaseAPIParams(t)
	)

	// Init. ioContext
	m.saveClusterState()
	if m.originalTargetCount < 1 {
		t.Fatalf("Must have at least 1 target in the cluster")
	}

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	target := tutils.ExtractTargetNodes(m.smap)[0]
	mpList, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)

	if len(mpList.Available) < 2 {
		t.Fatalf("Must have at least 2 mountpaths")
	}

	// select up to 2 mountpath
	mpaths := []string{mpList.Available[0]}
	if len(mpList.Available) > 2 {
		mpaths = append(mpaths, mpList.Available[1])
	}

	// Disable mountpaths temporarily
	for _, mp := range mpaths {
		err = api.DisableMountpath(baseParams, target.ID(), mp)
		tassert.CheckFatal(t, err)
	}

	m.puts()

	// Start getting objects and enable mountpaths in parallel
	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()

	for _, mp := range mpaths {
		// sleep for a while before enabling another mountpath
		time.Sleep(50 * time.Millisecond)
		err = api.EnableMountpath(baseParams, target, mp)
		tassert.CheckFatal(t, err)
	}

	m.wg.Wait()

	mpListAfter, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)
	if len(mpList.Available) != len(mpListAfter.Available) {
		t.Fatalf("Some mountpaths failed to enable: the number before %d, after %d",
			len(mpList.Available), len(mpListAfter.Available))
	}

	m.ensureNoErrors()
}

func TestGetDuringRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		md = ioContext{
			t:               t,
			num:             10000,
			numGetsEachFile: 1,
		}
		mdAfterRebalance = ioContext{
			t:               t,
			num:             10000,
			numGetsEachFile: 1,
		}
	)

	// Init. ioContext
	md.saveClusterState()
	mdAfterRebalance.saveClusterState()

	if md.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", md.originalTargetCount)
	}
	target := tutils.ExtractTargetNodes(md.smap)[0]

	// Create ais bucket
	tutils.CreateFreshBucket(t, md.proxyURL, md.bucket)
	defer tutils.DestroyBucket(t, md.proxyURL, md.bucket)

	// Unregister a target
	err := tutils.UnregisterNode(md.proxyURL, target.DaemonID)
	tassert.CheckFatal(t, err)
	n := len(getClusterMap(t, md.proxyURL).Tmap)
	if n != md.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", md.originalTargetCount-1, n)
	}

	// Start putting files into bucket
	md.puts()
	mdAfterRebalance.puts()

	// Start getting objects and register target in parallel
	md.wg.Add(md.num * md.numGetsEachFile)
	md.gets()

	tutils.Logf("Register target %s\n", target.URL(cmn.NetworkPublic))
	err = tutils.RegisterNode(md.proxyURL, target, md.smap)
	tassert.CheckFatal(t, err)

	// wait for everything to finish
	baseParams := tutils.BaseAPIParams(md.proxyURL)
	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)
	md.wg.Wait()

	// read files once again
	mdAfterRebalance.wg.Add(mdAfterRebalance.num * mdAfterRebalance.numGetsEachFile)
	mdAfterRebalance.gets()
	mdAfterRebalance.wg.Wait()

	mdAfterRebalance.ensureNoErrors()
	md.assertClusterState()
}

func TestRegisterTargetsAndCreateBucketsInParallel(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	const (
		unregisterTargetCount = 2
		newBucketCount        = 3
	)

	var (
		m = ioContext{
			t:  t,
			wg: &sync.WaitGroup{},
		}
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	tutils.Logf("Num targets %d\n", m.originalTargetCount)
	targets := tutils.ExtractTargetNodes(m.smap)

	// Unregister targets
	for i := 0; i < unregisterTargetCount; i++ {
		err := tutils.UnregisterNode(m.proxyURL, targets[i].DaemonID)
		tassert.CheckError(t, err)
		n := getClusterMap(t, m.proxyURL).CountTargets()
		if n != m.originalTargetCount-(i+1) {
			t.Errorf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-(i+1), n)
		}
		tutils.Logf("Unregistered target %s: the cluster now has %d targets\n", targets[i].URL(cmn.NetworkPublic), n)
	}

	m.wg.Add(unregisterTargetCount)
	for i := 0; i < unregisterTargetCount; i++ {
		go func(number int) {
			defer m.wg.Done()

			err := tutils.RegisterNode(m.proxyURL, targets[number], m.smap)
			tassert.CheckError(t, err)
		}(i)
	}

	m.wg.Add(newBucketCount)
	for i := 0; i < newBucketCount; i++ {
		go func(number int) {
			defer m.wg.Done()

			tutils.CreateFreshBucket(t, m.proxyURL, m.bucket+strconv.Itoa(number))
		}(i)

		defer tutils.DestroyBucket(t, m.proxyURL, m.bucket+strconv.Itoa(i))
	}
	m.wg.Wait()
	m.assertClusterState()
}

func TestAddAndRemoveMountpath(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             5000,
			numGetsEachFile: 2,
		}
		baseParams = tutils.DefaultBaseAPIParams(t)
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Fatalf("Must have 2 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	target := tutils.ExtractTargetNodes(m.smap)[0]
	// Remove all mountpaths for one target
	oldMountpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)

	for _, mpath := range oldMountpaths.Available {
		err = api.RemoveMountpath(baseParams, target.ID(), mpath)
		tassert.CheckFatal(t, err)
	}

	// Check if mountpaths were actually removed
	mountpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)

	if len(mountpaths.Available) != 0 {
		t.Fatalf("Target should not have any paths available")
	}

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Add target mountpath again
	for _, mpath := range oldMountpaths.Available {
		err = api.AddMountpath(baseParams, target, mpath)
		tassert.CheckFatal(t, err)
	}

	// Check if mountpaths were actually added
	mountpaths, err = api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)

	if len(mountpaths.Available) != len(oldMountpaths.Available) {
		t.Fatalf("Target should have old mountpath available restored")
	}

	// Put and read random files
	m.puts()

	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()
	m.ensureNoErrors()
}

func TestLocalRebalanceAfterAddingMountpath(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	const newMountpath = "/tmp/ais/mountpath"

	var (
		m = ioContext{
			t:               t,
			num:             5000,
			numGetsEachFile: 2,
		}
		baseParams = tutils.DefaultBaseAPIParams(t)
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 1 {
		t.Fatalf("Must have 1 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	target := tutils.ExtractTargetNodes(m.smap)[0]

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)

	if containers.DockerRunning() {
		err := containers.DockerCreateMpathDir(0, newMountpath)
		tassert.CheckFatal(t, err)
	} else {
		err := cmn.CreateDir(newMountpath)
		tassert.CheckFatal(t, err)
	}

	defer func() {
		if !containers.DockerRunning() {
			os.RemoveAll(newMountpath)
		}
		tutils.DestroyBucket(t, m.proxyURL, m.bucket)
	}()

	// Put random files
	m.puts()

	// Add new mountpath to target
	err := api.AddMountpath(baseParams, target, newMountpath)
	tassert.CheckFatal(t, err)

	waitForRebalanceToComplete(t, tutils.BaseAPIParams(m.proxyURL), rebalanceTimeout)

	// Read files after rebalance
	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()

	// Remove new mountpath from target
	if containers.DockerRunning() {
		if err := api.RemoveMountpath(baseParams, target.ID(), newMountpath); err != nil {
			t.Error(err.Error())
		}
	} else {
		err = api.RemoveMountpath(baseParams, target.ID(), newMountpath)
		tassert.CheckFatal(t, err)
	}

	m.ensureNoErrors()
}

func TestLocalAndGlobalRebalanceAfterAddingMountpath(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	const (
		newMountpath = "/tmp/ais/mountpath"
	)

	var (
		m = ioContext{
			t:               t,
			num:             10000,
			numGetsEachFile: 5,
		}
		baseParams = tutils.DefaultBaseAPIParams(t)
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 1 {
		t.Fatalf("Must have 1 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	targets := tutils.ExtractTargetNodes(m.smap)

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)

	defer func() {
		if !containers.DockerRunning() {
			os.RemoveAll(newMountpath)
		}
		tutils.DestroyBucket(t, m.proxyURL, m.bucket)
	}()

	// Put random files
	m.puts()

	if containers.DockerRunning() {
		err := containers.DockerCreateMpathDir(0, newMountpath)
		tassert.CheckFatal(t, err)
		for _, target := range targets {
			err = api.AddMountpath(baseParams, target, newMountpath)
			tassert.CheckFatal(t, err)
		}
	} else {
		// Add new mountpath to all targets
		for idx, target := range targets {
			mountpath := filepath.Join(newMountpath, fmt.Sprintf("%d", idx))
			cmn.CreateDir(mountpath)
			err := api.AddMountpath(baseParams, target, mountpath)
			tassert.CheckFatal(t, err)
		}
	}

	waitForRebalanceToComplete(t, tutils.BaseAPIParams(m.proxyURL), rebalanceTimeout)

	// Read after rebalance
	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()

	// Remove new mountpath from all targets
	if containers.DockerRunning() {
		err := containers.DockerRemoveMpathDir(0, newMountpath)
		tassert.CheckFatal(t, err)
		for _, target := range targets {
			if err := api.RemoveMountpath(baseParams, target.ID(), newMountpath); err != nil {
				t.Error(err.Error())
			}
		}
	} else {
		for idx, target := range targets {
			mountpath := filepath.Join(newMountpath, fmt.Sprintf("%d", idx))
			os.RemoveAll(mountpath)
			if err := api.RemoveMountpath(baseParams, target.ID(), mountpath); err != nil {
				t.Error(err.Error())
			}
		}
	}

	m.ensureNoErrors()
}

func TestDisableAndEnableMountpath(t *testing.T) {
	var (
		m = ioContext{
			t:               t,
			num:             5000,
			numGetsEachFile: 2,
		}
		baseParams = tutils.DefaultBaseAPIParams(t)
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 1 {
		t.Fatalf("Must have 1 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	target := tutils.ExtractTargetNodes(m.smap)[0]
	// Remove all mountpaths for one target
	oldMountpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)

	for _, mpath := range oldMountpaths.Available {
		err := api.DisableMountpath(baseParams, target.ID(), mpath)
		tassert.CheckFatal(t, err)
	}

	// Check if mountpaths were actually disabled
	mountpaths, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)

	if len(mountpaths.Available) != 0 {
		t.Fatalf("Target should not have any paths available")
	}

	if len(mountpaths.Disabled) != len(oldMountpaths.Available) {
		t.Fatalf("Not all mountpaths were added to disabled paths")
	}

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Add target mountpath again
	for _, mpath := range oldMountpaths.Available {
		err := api.EnableMountpath(baseParams, target, mpath)
		tassert.CheckFatal(t, err)
	}

	// Check if mountpaths were actually enabled
	mountpaths, err = api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)

	if len(mountpaths.Available) != len(oldMountpaths.Available) {
		t.Fatalf("Target should have old mountpath available restored")
	}

	if len(mountpaths.Disabled) != 0 {
		t.Fatalf("Not all disabled mountpaths were enabled")
	}

	tutils.Logf("waiting for ais bucket %s to appear on all targets\n", m.bucket)
	err = tutils.WaitForBucket(m.proxyURL, m.bucket, true /*exists*/)
	tassert.CheckFatal(t, err)

	// Put and read random files
	m.puts()

	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()
	m.ensureNoErrors()
}

func TestForwardCP(t *testing.T) {
	var (
		m = ioContext{
			t:               t,
			num:             10000,
			numGetsEachFile: 2,
			fileSize:        128,
		}
	)

	// Step 1.
	m.saveClusterState()
	if m.originalProxyCount < 2 {
		t.Fatalf("Must have 2 or more proxies in the cluster, have only %d", m.originalProxyCount)
	}

	// Step 2.
	origID, origURL := m.smap.ProxySI.DaemonID, m.smap.ProxySI.PublicNet.DirectURL
	nextProxyID, nextProxyURL, _ := chooseNextProxy(m.smap)

	tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	tutils.CreateFreshBucket(t, nextProxyURL, m.bucket)
	tutils.Logf("Created bucket %s via non-primary %s\n", m.bucket, nextProxyID)

	// Step 3.
	m.puts()

	// Step 4. in parallel: run GETs and designate a new primary=nextProxyID
	m.wg.Add(m.num*m.numGetsEachFile + 1)
	m.gets()

	go func() {
		defer m.wg.Done()

		setPrimaryTo(t, m.proxyURL, m.smap, nextProxyURL, nextProxyID)
		m.proxyURL = nextProxyURL
	}()

	m.wg.Wait()
	m.ensureNoErrors()

	// Step 5. destroy ais bucket via original primary which is not primary at this point
	tutils.DestroyBucket(t, origURL, m.bucket)
	tutils.Logf("Destroyed bucket %s via non-primary %s/%s\n", m.bucket, origID, origURL)
}

func TestAtimeRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             2000,
			numGetsEachFile: 2,
		}
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Fatalf("Must have 2 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	target := tutils.ExtractTargetNodes(m.smap)[0]

	// Unregister a target
	tutils.Logf("Unregister target %s\n", target.URL(cmn.NetworkPublic))
	err := tutils.UnregisterNode(m.proxyURL, target.DaemonID)
	tassert.CheckFatal(t, err)

	// Put random files
	m.puts()

	// Get atime in a format that includes nanoseconds to properly check if it
	// was updated in atime cache (if it wasn't, then the returned atime would
	// be different from the original one, but the difference could be very small).
	msg := &cmn.SelectMsg{TimeFormat: time.StampNano}
	msg.AddProps(cmn.GetPropsAtime, cmn.GetPropsStatus)
	baseParams := tutils.BaseAPIParams(m.proxyURL)
	bucketList, err := api.ListBucket(baseParams, m.bucket, msg, 0)
	tassert.CheckFatal(t, err)

	objNames := make(cmn.SimpleKVs, 10)
	for _, entry := range bucketList.Entries {
		objNames[entry.Name] = entry.Atime
	}

	// register target
	err = tutils.RegisterNode(m.proxyURL, target, m.smap)
	tassert.CheckFatal(t, err)

	// make sure that the cluster has all targets enabled
	_, err = tutils.WaitForPrimaryProxy(
		m.proxyURL,
		"to join target back",
		m.smap.Version, testing.Verbose(),
		m.originalProxyCount,
		m.originalTargetCount,
	)
	tassert.CheckFatal(t, err)

	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)

	msg = &cmn.SelectMsg{TimeFormat: time.StampNano}
	msg.AddProps(cmn.GetPropsAtime, cmn.GetPropsStatus)
	bucketListReb, err := api.ListBucket(baseParams, m.bucket, msg, 0)
	tassert.CheckFatal(t, err)

	itemCount, itemCountOk := len(bucketListReb.Entries), 0
	l := len(bucketList.Entries)
	if itemCount != l {
		t.Errorf("The number of objects mismatch: before %d, after %d", len(bucketList.Entries), itemCount)
	}
	for _, entry := range bucketListReb.Entries {
		atime, ok := objNames[entry.Name]
		if !ok {
			t.Errorf("Object %q not found", entry.Name)
			continue
		}
		if atime != entry.Atime {
			t.Errorf("Atime mismatched for %s: before %q, after %q", entry.Name, atime, entry.Atime)
		}
		if entry.IsStatusOK() {
			itemCountOk++
		}
	}
	if itemCountOk != l {
		t.Errorf("Wrong number of objects with status OK: %d (expecting %d)", itemCountOk, l)
	}
}

func TestAtimeLocalGet(t *testing.T) {
	var (
		proxyURL      = getPrimaryURL(t, proxyURLReadOnly)
		baseParams    = tutils.DefaultBaseAPIParams(t)
		bucket        = t.Name()
		objectName    = t.Name()
		objectContent = tutils.NewBytesReader([]byte("file content"))
	)

	tutils.CreateFreshBucket(t, proxyURL, bucket)
	defer tutils.DestroyBucket(t, proxyURL, bucket)

	err := api.PutObject(api.PutObjectArgs{BaseParams: baseParams, Bucket: bucket, Object: objectName, Reader: objectContent})
	tassert.CheckFatal(t, err)

	timeAfterPut := tutils.GetObjectAtime(t, baseParams, objectName, bucket, time.RFC3339Nano)

	// Get object so that atime is updated
	_, err = api.GetObject(baseParams, bucket, objectName)
	tassert.CheckFatal(t, err)

	timeAfterGet := tutils.GetObjectAtime(t, baseParams, objectName, bucket, time.RFC3339Nano)

	if !(timeAfterGet.After(timeAfterPut)) {
		t.Errorf("Expected PUT atime (%s) to be before subsequent GET atime (%s).", timeAfterGet.Format(time.RFC3339Nano), timeAfterPut.Format(time.RFC3339Nano))
	}
}

func TestAtimeColdGet(t *testing.T) {
	var (
		proxyURL      = getPrimaryURL(t, proxyURLReadOnly)
		baseParams    = tutils.DefaultBaseAPIParams(t)
		bucket        = clibucket
		objectName    = t.Name()
		objectContent = tutils.NewBytesReader([]byte("file content"))
	)

	if !isCloudBucket(t, proxyURL, bucket) {
		t.Skip("test requires a cloud bucket")
	}
	tutils.CleanCloudBucket(t, proxyURL, bucket, objectName)
	defer tutils.CleanCloudBucket(t, proxyURL, bucket, objectName)

	tutils.PutObjectInCloudBucketWithoutCachingLocally(t, objectName, bucket, proxyURL, objectContent)

	timeAfterPut := time.Now()

	// Perform the COLD get
	_, err := api.GetObject(baseParams, bucket, objectName)
	tassert.CheckFatal(t, err)

	timeAfterGet := tutils.GetObjectAtime(t, baseParams, objectName, bucket, time.RFC3339Nano)

	if !(timeAfterGet.After(timeAfterPut)) {
		t.Errorf("Expected PUT atime (%s) to be before subsequent GET atime (%s).", timeAfterGet.Format(time.RFC3339Nano), timeAfterPut.Format(time.RFC3339Nano))
	}
}

func TestAtimePrefetch(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		proxyURL      = getPrimaryURL(t, proxyURLReadOnly)
		baseParams    = tutils.DefaultBaseAPIParams(t)
		bucket        = clibucket
		objectName    = t.Name()
		objectContent = tutils.NewBytesReader([]byte("file content"))
	)

	if !isCloudBucket(t, proxyURL, bucket) {
		t.Skip("test requires a cloud bucket")
	}
	tutils.CleanCloudBucket(t, proxyURL, bucket, objectName)
	defer tutils.CleanCloudBucket(t, proxyURL, bucket, objectName)

	tutils.PutObjectInCloudBucketWithoutCachingLocally(t, objectName, bucket, proxyURL, objectContent)

	timeAfterPut := time.Now()

	err := api.PrefetchList(baseParams, bucket, cmn.Cloud, []string{objectName}, true, 0)
	tassert.CheckFatal(t, err)

	timeAfterGet := tutils.GetObjectAtime(t, baseParams, objectName, bucket, time.RFC3339Nano)

	if !(timeAfterGet.Before(timeAfterPut)) {
		t.Errorf("Atime should not be updated after prefetch (got: atime after PUT: %s, atime after GET: %s).",
			timeAfterPut.Format(time.RFC3339Nano), timeAfterGet.Format(time.RFC3339Nano))
	}
}

func TestAtimeLocalPut(t *testing.T) {
	var (
		proxyURL      = getPrimaryURL(t, proxyURLReadOnly)
		baseParams    = tutils.DefaultBaseAPIParams(t)
		bucket        = t.Name()
		objectName    = t.Name()
		objectContent = tutils.NewBytesReader([]byte("file content"))
	)

	tutils.CreateFreshBucket(t, proxyURL, bucket)
	defer tutils.DestroyBucket(t, proxyURL, bucket)

	timeBeforePut := time.Now()
	err := api.PutObject(api.PutObjectArgs{BaseParams: baseParams, Bucket: bucket, Object: objectName, Reader: objectContent})
	tassert.CheckFatal(t, err)

	timeAfterPut := tutils.GetObjectAtime(t, baseParams, objectName, bucket, time.RFC3339Nano)

	if !(timeAfterPut.After(timeBeforePut)) {
		t.Errorf("Expected atime after PUT (%s) to be after atime before PUT (%s).", timeAfterPut.Format(time.RFC3339Nano), timeBeforePut.Format(time.RFC3339Nano))
	}
}

// 1. Unregister target
// 2. Add bucket - unregistered target should miss the update
// 3. Reregister target
// 4. Put objects
// 5. Get objects - everything should succeed
func TestGetAndPutAfterReregisterWithMissedBucketUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             10000,
			numGetsEachFile: 5,
		}
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Fatalf("Must have 2 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	// Unregister target 0
	targets := tutils.ExtractTargetNodes(m.smap)
	err := tutils.UnregisterNode(m.proxyURL, targets[0].DaemonID)
	tassert.CheckFatal(t, err)
	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Reregister target 0
	m.reregisterTarget(targets[0])

	// Do puts
	m.puts()

	// Do gets
	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()

	m.ensureNoErrors()
	m.assertClusterState()
}

// 1. Unregister target
// 2. Add bucket - unregistered target should miss the update
// 3. Put objects
// 4. Reregister target - rebalance kicks in
// 5. Get objects - everything should succeed
func TestGetAfterReregisterWithMissedBucketUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:               t,
			num:             10000,
			fileSize:        1024,
			numGetsEachFile: 5,
		}
	)

	// Initialize ioContext
	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Fatalf("Must have 2 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	targets := tutils.ExtractTargetNodes(m.smap)

	// Unregister target 0
	err := tutils.UnregisterNode(m.proxyURL, targets[0].DaemonID)
	tassert.CheckFatal(t, err)
	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	m.puts()

	// Reregister target 0
	m.reregisterTarget(targets[0])

	// Wait for rebalance and do gets
	baseParams := tutils.BaseAPIParams(m.proxyURL)
	waitForRebalanceToComplete(t, baseParams)

	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()

	m.ensureNoErrors()
	m.assertClusterState()
}

func TestRenewRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		m = ioContext{
			t:                   t,
			num:                 10000,
			numGetsEachFile:     5,
			otherTasksToTrigger: 1,
		}
	)

	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Fatalf("Must have 2 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	// Step 1: Unregister a target
	target := tutils.ExtractTargetNodes(m.smap)[0]
	err := tutils.UnregisterNode(m.proxyURL, target.DaemonID)
	tassert.CheckFatal(t, err)

	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-1 {
		t.Fatalf("%d targets expected after unregister, actually %d targets", m.originalTargetCount-1, n)
	}
	tutils.Logf("Unregistered target %s: the cluster now has %d targets\n", target.URL(cmn.NetworkPublic), n)

	// Step 2: Create an ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Step 3: PUT objects in the bucket
	m.puts()

	baseParams := tutils.BaseAPIParams(m.proxyURL)

	// Step 4: Re-register target (triggers rebalance)
	m.reregisterTarget(target)
	waitForBucketXactionToStart(t, cmn.ActGlobalReb, "", baseParams, rebalanceStartTimeout)
	tutils.Logf("automatic global rebalance started\n")

	m.wg.Add(m.num*m.numGetsEachFile + 2)
	// Step 5: GET objects from the buket
	go func() {
		defer m.wg.Done()
		m.gets()
	}()

	// Step 6:
	//   - Start new rebalance manually after some time
	//   - TODO: Verify that new rebalance xaction has started
	go func() {
		defer m.wg.Done()

		<-m.controlCh // wait for half the GETs to complete

		err := api.ExecXaction(baseParams, cmn.ActGlobalReb, cmn.ActXactStart, "")
		tassert.CheckFatal(t, err)
		tutils.Logf("manually initiated global rebalance\n")
	}()

	m.wg.Wait()
	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)
	m.ensureNoErrors()
	m.assertClusterState()
}

func TestGetFromMirroredBucketWithLostMountpath(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}
	var (
		copies = 2
		m      = ioContext{
			t:               t,
			num:             5000,
			numGetsEachFile: 4,
		}
	)
	m.saveClusterState()
	baseParams := tutils.BaseAPIParams(m.proxyURL)

	// Select one target at random
	target := tutils.ExtractTargetNodes(m.smap)[0]
	mpList, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)
	if len(mpList.Available) < copies {
		t.Fatalf("%s requires at least %d mountpaths per target", t.Name(), copies)
	}

	// Step 1: Create a local bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Step 2: Make the bucket redundant
	err = api.SetBucketProps(baseParams, m.bucket, cmn.BucketPropsToUpdate{
		Mirror: &cmn.MirrorConfToUpdate{
			Enabled: api.Bool(true),
			Copies:  api.Int64(int64(copies)),
		},
	})
	if err != nil {
		t.Fatalf("Failed to make the bucket redundant: %v", err)
	}

	// Step 3: PUT objects in the bucket
	m.puts()
	m.ensureNumCopies(copies)

	// Step 4: Remove a mountpath (simulates disk loss)
	mpath := mpList.Available[0]
	tutils.Logf("Remove mountpath %s on target %s\n", mpath, target.ID())
	err = api.RemoveMountpath(baseParams, target.ID(), mpath)
	tassert.CheckFatal(t, err)

	// Step 5: GET objects from the bucket
	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()

	m.ensureNumCopies(copies)

	// Step 6: Add previously removed mountpath
	tutils.Logf("Add mountpath %s on target %s\n", mpath, target.ID())
	err = api.AddMountpath(baseParams, target, mpath)
	tassert.CheckFatal(t, err)

	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)

	m.ensureNumCopies(copies)
	m.ensureNoErrors()
}

func TestGetFromMirroredBucketWithLostAllMountpath(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	m := ioContext{
		t:               t,
		num:             10000,
		numGetsEachFile: 4,
	}
	m.saveClusterState()
	baseParams := tutils.BaseAPIParams(m.proxyURL)

	// Select one target at random
	target := tutils.ExtractTargetNodes(m.smap)[0]
	mpList, err := api.GetMountpaths(baseParams, target)
	mpathCount := len(mpList.Available)
	tassert.CheckFatal(t, err)
	if mpathCount < 3 {
		t.Fatalf("%s requires at least 3 mountpaths per target", t.Name())
	}

	// Step 1: Create a local bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)

	// Step 2: Make the bucket redundant
	err = api.SetBucketProps(baseParams, m.bucket, cmn.BucketPropsToUpdate{
		Mirror: &cmn.MirrorConfToUpdate{
			Enabled: api.Bool(true),
			Copies:  api.Int64(int64(mpathCount)),
		},
	})
	if err != nil {
		t.Fatalf("Failed to make the bucket redundant: %v", err)
	}

	// Step 3: PUT objects in the bucket
	m.puts()
	m.ensureNumCopies(mpathCount)

	// Step 4: Remove almost all mountpaths
	tutils.Logf("Remove mountpaths on target %s\n", target.ID())
	for _, mpath := range mpList.Available[1:] {
		err = api.RemoveMountpath(baseParams, target.ID(), mpath)
		tassert.CheckFatal(t, err)
	}

	// Step 5: GET objects from the bucket
	m.wg.Add(m.num * m.numGetsEachFile)
	m.gets()
	m.wg.Wait()

	// Step 6: Add previously removed mountpath
	tutils.Logf("Add mountpaths on target %s\n", target.ID())
	for _, mpath := range mpList.Available[1:] {
		err = api.AddMountpath(baseParams, target, mpath)
		tassert.CheckFatal(t, err)
	}

	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)

	m.ensureNumCopies(mpathCount)
	m.ensureNoErrors()
}
