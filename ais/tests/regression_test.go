// Package ais_test contains AIS integration tests.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/containers"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/tutils"
	"github.com/NVIDIA/aistore/tutils/tassert"
	jsoniter "github.com/json-iterator/go"
)

type Test struct {
	name   string
	method func(*testing.T)
}

type regressionTestData struct {
	bucket        string
	renamedBucket string
	numBuckets    int
	rename        bool
	wait          bool
}

const (
	rootDir = "/tmp/ais"

	ListRangeStr   = "__listrange"
	TestBucketName = "TESTAISBUCKET"
)

var (
	HighWaterMark    = int32(80)
	LowWaterMark     = int32(60)
	UpdTime          = time.Second * 20
	configRegression = map[string]string{
		"periodic.stats_time":   fmt.Sprintf("%v", UpdTime),
		"lru.enabled":           "true",
		"lru.lowwm":             fmt.Sprintf("%d", LowWaterMark),
		"lru.highwm":            fmt.Sprintf("%d", HighWaterMark),
		"lru.capacity_upd_time": fmt.Sprintf("%v", UpdTime),
		"lru.dont_evict_time":   fmt.Sprintf("%v", UpdTime),
	}
)

func TestLocalListBucketGetTargetURL(t *testing.T) {
	const (
		num      = 1000
		filesize = 1024
		bucket   = TestBucketName
	)
	var (
		filenameCh = make(chan string, num)
		errCh      = make(chan error, num)
		sgl        *memsys.SGL
		targets    = make(map[string]struct{})
		proxyURL   = getPrimaryURL(t, proxyURLReadOnly)
	)
	smap := getClusterMap(t, proxyURL)
	if smap.CountTargets() == 1 {
		tutils.Logln("Warning: more than 1 target should deployed for best utility of this test.")
	}

	sgl = tutils.Mem2.NewSGL(filesize)
	defer sgl.Free()
	tutils.CreateFreshBucket(t, proxyURL, bucket)
	defer tutils.DestroyBucket(t, proxyURL, bucket)

	tutils.PutRandObjs(proxyURL, bucket, SmokeDir, readerType, SmokeStr, filesize, num, errCh, filenameCh, sgl, true)
	selectErr(errCh, "put", t, true)
	close(filenameCh)
	close(errCh)

	msg := &cmn.SelectMsg{PageSize: int(pagesize), Props: cmn.GetTargetURL}
	bl, err := api.ListBucket(tutils.DefaultBaseAPIParams(t), bucket, msg, num)
	tassert.CheckFatal(t, err)

	if len(bl.Entries) != num {
		t.Errorf("Expected %d bucket list entries, found %d\n", num, len(bl.Entries))
	}

	for _, e := range bl.Entries {
		if e.TargetURL == "" {
			t.Error("Target URL in response is empty")
		}
		if _, ok := targets[e.TargetURL]; !ok {
			targets[e.TargetURL] = struct{}{}
		}
		baseParams := tutils.BaseAPIParams(e.TargetURL)
		l, err := api.GetObject(baseParams, bucket, e.Name)
		tassert.CheckFatal(t, err)
		if uint64(l) != filesize {
			t.Errorf("Expected filesize: %d, actual filesize: %d\n", filesize, l)
		}
	}

	if smap.CountTargets() != len(targets) { // The objects should have been distributed to all targets
		t.Errorf("Expected %d different target URLs, actual: %d different target URLs", smap.CountTargets(), len(targets))
	}

	// Ensure no target URLs are returned when the property is not requested
	msg.Props = ""
	bl, err = api.ListBucket(tutils.DefaultBaseAPIParams(t), bucket, msg, num)
	tassert.CheckFatal(t, err)

	if len(bl.Entries) != num {
		t.Errorf("Expected %d bucket list entries, found %d\n", num, len(bl.Entries))
	}

	for _, e := range bl.Entries {
		if e.TargetURL != "" {
			t.Fatalf("Target URL: %s returned when empty target URL expected\n", e.TargetURL)
		}
	}
}

func TestCloudListBucketGetTargetURL(t *testing.T) {
	const (
		numberOfFiles = 100
		fileSize      = 1024
	)

	var (
		fileNameCh = make(chan string, numberOfFiles)
		errCh      = make(chan error, numberOfFiles)
		sgl        *memsys.SGL
		bucketName = clibucket
		targets    = make(map[string]struct{})
		proxyURL   = getPrimaryURL(t, proxyURLReadOnly)
		prefix     = tutils.GenRandomString(32)
	)

	if !isCloudBucket(t, proxyURL, clibucket) {
		t.Skipf("%s requires a cloud bucket", t.Name())
	}
	smap := getClusterMap(t, proxyURL)
	if smap.CountTargets() == 1 {
		tutils.Logln("Warning: more than 1 target should deployed for best utility of this test.")
	}

	sgl = tutils.Mem2.NewSGL(fileSize)
	defer sgl.Free()
	tutils.PutRandObjs(proxyURL, bucketName, SmokeDir, readerType, prefix, fileSize, numberOfFiles, errCh, fileNameCh, sgl, true)
	selectErr(errCh, "put", t, true)
	close(fileNameCh)
	close(errCh)
	defer func() {
		files := make([]string, numberOfFiles)
		for i := 0; i < numberOfFiles; i++ {
			files[i] = path.Join(prefix, <-fileNameCh)
		}
		err := api.DeleteList(tutils.BaseAPIParams(proxyURL), bucketName, cmn.Cloud, files, true, 0)
		if err != nil {
			t.Errorf("Failed to delete objects from bucket %s, err: %v", bucketName, err)
		}
	}()

	listBucketMsg := &cmn.SelectMsg{Prefix: prefix, PageSize: int(pagesize), Props: cmn.GetTargetURL}
	bucketList, err := api.ListBucket(tutils.DefaultBaseAPIParams(t), bucketName, listBucketMsg, 0)
	tassert.CheckFatal(t, err)

	if len(bucketList.Entries) != numberOfFiles {
		t.Errorf("Number of entries in bucket list [%d] must be equal to [%d].\n",
			len(bucketList.Entries), numberOfFiles)
	}

	for _, object := range bucketList.Entries {
		if object.TargetURL == "" {
			t.Errorf("Target URL in response is empty for object [%s]", object.Name)
		}
		if _, ok := targets[object.TargetURL]; !ok {
			targets[object.TargetURL] = struct{}{}
		}
		baseParams := tutils.BaseAPIParams(object.TargetURL)
		objectSize, err := api.GetObject(baseParams, bucketName, object.Name)
		tassert.CheckFatal(t, err)
		if uint64(objectSize) != fileSize {
			t.Errorf("Expected fileSize: %d, actual fileSize: %d\n", fileSize, objectSize)
		}
	}

	// The objects should have been distributed to all targets
	if smap.CountTargets() != len(targets) {
		t.Errorf("Expected %d different target URLs, actual: %d different target URLs",
			smap.CountTargets(), len(targets))
	}

	// Ensure no target URLs are returned when the property is not requested
	listBucketMsg.Props = ""
	bucketList, err = api.ListBucket(tutils.DefaultBaseAPIParams(t), bucketName, listBucketMsg, 0)
	tassert.CheckFatal(t, err)

	if len(bucketList.Entries) != numberOfFiles {
		t.Errorf("Expected %d bucket list entries, found %d\n", numberOfFiles, len(bucketList.Entries))
	}

	for _, object := range bucketList.Entries {
		if object.TargetURL != "" {
			t.Fatalf("Target URL: %s returned when empty target URL expected\n", object.TargetURL)
		}
	}
}

// 1. PUT file
// 2. Corrupt the file
// 3. GET file
func TestGetCorruptFileAfterPut(t *testing.T) {
	const filesize = 1024
	var (
		num        = 2
		filenameCh = make(chan string, num)
		errCh      = make(chan error, 100)
		sgl        *memsys.SGL
		fqn        string
		proxyURL   = getPrimaryURL(t, proxyURLReadOnly)
	)
	if containers.DockerRunning() {
		t.Skip(fmt.Sprintf("%q requires setting Xattrs, doesn't work with docker", t.Name()))
	}

	bucket := TestBucketName
	tutils.CreateFreshBucket(t, proxyURL, bucket)
	defer tutils.DestroyBucket(t, proxyURL, bucket)

	sgl = tutils.Mem2.NewSGL(filesize)
	defer sgl.Free()
	tutils.PutRandObjs(proxyURL, bucket, SmokeDir, readerType, SmokeStr, filesize, num, errCh, filenameCh, sgl)
	selectErr(errCh, "put", t, false)
	close(filenameCh)
	close(errCh)

	// Test corrupting the file contents
	// Note: The following tests can only work when running on a local setup(targets are co-located with
	//       where this test is running from, because it searches a local file system)
	var fName string
	fsWalkFunc := func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return err
		}
		if info.IsDir() && info.Name() == "cloud" {
			return filepath.SkipDir
		}
		if filepath.Base(path) == fName && strings.Contains(path, bucket) {
			fqn = path
		}
		return nil
	}

	fName = <-filenameCh
	filepath.Walk(rootDir, fsWalkFunc)
	tutils.Logf("Corrupting file data[%s]: %s\n", fName, fqn)
	err := ioutil.WriteFile(fqn, []byte("this file has been corrupted"), 0644)
	tassert.CheckFatal(t, err)
	_, err = api.GetObjectWithValidation(tutils.DefaultBaseAPIParams(t), bucket, path.Join(SmokeStr, fName))
	if err == nil {
		t.Error("Error is nil, expected non-nil error on a a GET for an object with corrupted contents")
	}
}

func TestRegressionBuckets(t *testing.T) {
	bucket := TestBucketName
	proxyURL := getPrimaryURL(t, proxyURLReadOnly)
	tutils.CreateFreshBucket(t, proxyURL, bucket)
	defer tutils.DestroyBucket(t, proxyURL, bucket)
	doBucketRegressionTest(t, proxyURL, regressionTestData{bucket: bucket})
}

func TestRenameBucket(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		proxyURL      = getPrimaryURL(t, proxyURLReadOnly)
		bucket        = TestBucketName
		guid, _       = cmn.GenUUID()
		renamedBucket = bucket + "_" + guid
	)
	for _, wait := range []bool{true, false} {
		t.Run(fmt.Sprintf("wait=%v", wait), func(t *testing.T) {
			tutils.CreateFreshBucket(t, proxyURL, bucket)
			tutils.DestroyBucket(t, proxyURL, renamedBucket) // cleanup post Ctrl-C etc.
			defer tutils.DestroyBucket(t, proxyURL, bucket)
			defer tutils.DestroyBucket(t, proxyURL, renamedBucket)

			b, err := api.GetBucketNames(tutils.DefaultBaseAPIParams(t), cmn.AIS)
			tassert.CheckFatal(t, err)

			doBucketRegressionTest(t, proxyURL, regressionTestData{
				bucket: bucket, renamedBucket: renamedBucket, numBuckets: len(b.AIS), rename: true, wait: wait,
			})
		})
	}
}

//
// doBucketRe*
//

func doBucketRegressionTest(t *testing.T, proxyURL string, rtd regressionTestData) {
	const filesize = 1024
	var (
		numPuts    = 2036
		filesPutCh = make(chan string, numPuts)
		errCh      = make(chan error, numPuts)
		bucket     = rtd.bucket
	)

	sgl := tutils.Mem2.NewSGL(filesize)
	defer sgl.Free()

	tutils.PutRandObjs(proxyURL, bucket, SmokeDir, readerType, SmokeStr, filesize, numPuts, errCh, filesPutCh, sgl)
	close(filesPutCh)
	filesPut := make([]string, 0, len(filesPutCh))
	for fname := range filesPutCh {
		filesPut = append(filesPut, fname)
	}
	selectErr(errCh, "put", t, true)
	if rtd.rename {
		baseParams := tutils.BaseAPIParams(proxyURL)
		err := api.RenameBucket(baseParams, rtd.bucket, rtd.renamedBucket)
		tassert.CheckFatal(t, err)
		tutils.Logf("Renamed %s(numobjs=%d) => %s\n", bucket, numPuts, rtd.renamedBucket)
		if rtd.wait {
			postRenameWaitAndCheck(t, proxyURL, rtd, numPuts, filesPut)
		}
		bucket = rtd.renamedBucket
	}
	sema := make(chan struct{}, 16)
	del := func() {
		tutils.Logf("Deleting %d objects\n", len(filesPut))
		wg := &sync.WaitGroup{}
		for _, fname := range filesPut {
			wg.Add(1)
			sema <- struct{}{}
			go func(fn string) {
				tutils.Del(proxyURL, bucket, "smoke/"+fn, "", wg, errCh, true /* silent */)
				<-sema
			}(fname)
		}
		wg.Wait()
		selectErr(errCh, "delete", t, abortonerr)
		close(errCh)
	}
	getRandomFiles(proxyURL, numPuts, bucket, SmokeStr+"/", t, errCh)
	selectErr(errCh, "get", t, false)
	if !rtd.rename || rtd.wait {
		del()
	} else {
		postRenameWaitAndCheck(t, proxyURL, rtd, numPuts, filesPut)
		del()
	}
}

func postRenameWaitAndCheck(t *testing.T, proxyURL string, rtd regressionTestData, numPuts int, filesPutCh []string) {
	baseParams := tutils.BaseAPIParams(proxyURL)
	waitForBucketXactionToComplete(t, cmn.ActRenameLB /* = kind */, rtd.bucket, baseParams, rebalanceTimeout)
	tutils.Logf("xaction (rename %s=>%s) done\n", rtd.bucket, rtd.renamedBucket)

	buckets, err := api.GetBucketNames(baseParams, cmn.AIS)
	tassert.CheckFatal(t, err)

	if len(buckets.AIS) != rtd.numBuckets {
		t.Fatalf("wrong number of ais buckets (names) before and after rename (before: %d. after: %+v)",
			rtd.numBuckets, buckets.AIS)
	}

	renamedBucketExists := false
	for _, b := range buckets.AIS {
		if b == rtd.renamedBucket {
			renamedBucketExists = true
		} else if b == rtd.bucket {
			t.Fatalf("original ais bucket %s still exists after rename", rtd.bucket)
		}
	}

	if !renamedBucketExists {
		t.Fatalf("renamed ais bucket %s does not exist after rename", rtd.renamedBucket)
	}

	bckList, err := api.ListBucket(baseParams, rtd.renamedBucket, &cmn.SelectMsg{}, 0)
	tassert.CheckFatal(t, err)
	unique := make(map[string]bool)
	for _, e := range bckList.Entries {
		base := filepath.Base(e.Name)
		unique[base] = true
	}
	if len(unique) != numPuts {
		for _, name := range filesPutCh {
			if _, ok := unique[name]; !ok {
				tutils.Logf("not found: %s\n", name)
			}
		}
		t.Fatalf("wrong number of objects in the bucket %s renamed as %s (before: %d. after: %d)",
			rtd.bucket, rtd.renamedBucket, numPuts, len(unique))
	}
}

func TestRenameObjects(t *testing.T) {
	var (
		renameStr    = "rename"
		bucket       = t.Name()
		numPuts      = 1000
		objsPutCh    = make(chan string, numPuts)
		errCh        = make(chan error, 2*numPuts)
		newBaseNames = make([]string, 0, numPuts) // new basenames
		proxyURL     = getPrimaryURL(t, proxyURLReadOnly)
		baseParams   = tutils.DefaultBaseAPIParams(t)
	)

	tutils.CreateFreshBucket(t, proxyURL, bucket)
	defer tutils.DestroyBucket(t, proxyURL, bucket)

	sgl := tutils.Mem2.NewSGL(1024 * 1024)
	defer sgl.Free()

	tutils.PutRandObjs(proxyURL, bucket, "", readerType, "", 0, numPuts, errCh, objsPutCh, sgl)
	selectErr(errCh, "put", t, false)
	close(objsPutCh)
	i := 0
	for objName := range objsPutCh {
		newObjName := path.Join(renameStr, objName) + ".renamed" // objname fqn
		newBaseNames = append(newBaseNames, newObjName)
		if err := api.RenameObject(baseParams, bucket, objName, newObjName); err != nil {
			t.Fatalf("Failed to rename object from %s => %s, err: %v", objName, newObjName, err)
		}
		i++
		if i%50 == 0 {
			tutils.Logf("Renamed %s => %s\n", objName, newObjName)
		}
	}

	// get renamed objects
	for _, newObjName := range newBaseNames {
		_, err := api.GetObject(baseParams, bucket, newObjName)
		if err != nil {
			errCh <- err
		}
	}
	selectErr(errCh, "get", t, false)
}

func TestObjectPrefix(t *testing.T) {
	proxyURL := getPrimaryURL(t, proxyURLReadOnly)
	if created := createBucketIfNotExists(t, proxyURL, clibucket); created {
		defer tutils.DestroyBucket(t, proxyURL, clibucket)
	}

	prefixFileNumber = numfiles
	prefixCreateFiles(t, proxyURL)
	prefixLookup(t, proxyURL)
	prefixCleanup(t, proxyURL)
}

func TestObjectsVersions(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	propsMainTest(t, true /*versioning enabled*/)
}

func TestReregisterMultipleTargets(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	var (
		filesSentOrig = make(map[string]int64)
		filesRecvOrig = make(map[string]int64)
		bytesSentOrig = make(map[string]int64)
		bytesRecvOrig = make(map[string]int64)
		filesSent     int64
		filesRecv     int64
		bytesSent     int64
		bytesRecv     int64
	)

	m := ioContext{
		t:   t,
		num: 10000,
	}
	m.saveClusterState()

	if m.originalTargetCount < 2 {
		t.Fatalf("Must have at least 2 targets in the cluster, have only %d", m.originalTargetCount)
	}
	targetsToUnregister := m.originalTargetCount - 1

	// Step 0: Collect rebalance stats
	clusterStats := getClusterStats(t, m.proxyURL)
	for targetID, targetStats := range clusterStats.Target {
		filesSentOrig[targetID] = getNamedTargetStats(targetStats, stats.TxRebCount)
		filesRecvOrig[targetID] = getNamedTargetStats(targetStats, stats.RxRebCount)
		bytesSentOrig[targetID] = getNamedTargetStats(targetStats, stats.TxRebSize)
		bytesRecvOrig[targetID] = getNamedTargetStats(targetStats, stats.RxRebSize)
	}

	// Step 1: Unregister multiple targets
	targets := tutils.ExtractTargetNodes(m.smap)
	for i := 0; i < targetsToUnregister; i++ {
		tutils.Logf("Unregistering target %s\n", targets[i].DaemonID)
		err := tutils.UnregisterNode(m.proxyURL, targets[i].DaemonID)
		tassert.CheckFatal(t, err)
	}

	n := getClusterMap(t, m.proxyURL).CountTargets()
	if n != m.originalTargetCount-targetsToUnregister {
		t.Fatalf("%d target(s) expected after unregister, actually %d target(s)", m.originalTargetCount-targetsToUnregister, n)
	}
	tutils.Logf("The cluster now has %d target(s)\n", n)

	// Step 2: PUT objects into a newly created bucket
	tutils.CreateFreshBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyBucket(t, m.proxyURL, m.bucket)
	m.puts()

	// Step 3: Start performing GET requests
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.getsUntilStop()
	}()

	// Step 4: Simultaneously reregister each
	wg := &sync.WaitGroup{}
	for i := 0; i < targetsToUnregister; i++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			m.reregisterTarget(targets[r])
		}(i)
		time.Sleep(5 * time.Second) // wait some time before reregistering next target
	}
	wg.Wait()
	tutils.Logf("Stopping GETs...\n")
	m.stopGets()

	m.wg.Wait()

	baseParams := tutils.BaseAPIParams(m.proxyURL)
	waitForRebalanceToComplete(t, baseParams, rebalanceTimeout)

	clusterStats = getClusterStats(t, m.proxyURL)
	for targetID, targetStats := range clusterStats.Target {
		filesSent += getNamedTargetStats(targetStats, stats.TxRebCount) - filesSentOrig[targetID]
		filesRecv += getNamedTargetStats(targetStats, stats.RxRebCount) - filesRecvOrig[targetID]
		bytesSent += getNamedTargetStats(targetStats, stats.TxRebSize) - bytesSentOrig[targetID]
		bytesRecv += getNamedTargetStats(targetStats, stats.RxRebSize) - bytesRecvOrig[targetID]
	}

	// Step 5: Log rebalance stats
	tutils.Logf("Rebalance sent     %s in %d files\n", cmn.B2S(bytesSent, 2), filesSent)
	tutils.Logf("Rebalance received %s in %d files\n", cmn.B2S(bytesRecv, 2), filesRecv)

	m.ensureNoErrors()
	m.assertClusterState()
}

func TestGetClusterStats(t *testing.T) {
	proxyURL := getPrimaryURL(t, proxyURLReadOnly)
	smap := getClusterMap(t, proxyURL)
	stats := getClusterStats(t, proxyURL)

	for k, v := range stats.Target {
		tdstats := getDaemonStats(t, smap.Tmap[k].PublicNet.DirectURL)
		tdcapstats := tdstats["capacity"].(map[string]interface{})
		dcapstats := v.Capacity
		for fspath, fstats := range dcapstats {
			tfstats := tdcapstats[fspath].(map[string]interface{})
			used, err := strconv.ParseInt(tfstats["used"].(string), 10, 64)
			if err != nil {
				t.Fatalf("Could not decode Target Stats: fstats.Used")
			}
			avail, err := strconv.ParseInt(tfstats["avail"].(string), 10, 64)
			if err != nil {
				t.Fatalf("Could not decode Target Stats: fstats.Avail")
			}
			usedpct, err := tfstats["usedpct"].(json.Number).Int64()
			if err != nil {
				t.Fatalf("Could not decode Target Stats: fstats.Usedpct")
			}
			if used != int64(fstats.Used) || avail != int64(fstats.Avail) || usedpct != int64(fstats.Usedpct) {
				t.Errorf("Stats are different when queried from Target and Proxy: "+
					"Used: %v, %v | Available:  %v, %v | Percentage: %v, %v",
					tfstats["used"], fstats.Used, tfstats["avail"], fstats.Avail, tfstats["usedpct"], fstats.Usedpct)
			}
			if fstats.Usedpct > HighWaterMark {
				t.Error("Used Percentage above High Watermark")
			}
		}
	}
}

func TestConfig(t *testing.T) {
	proxyURL := getPrimaryURL(t, proxyURLReadOnly)
	oconfig := getClusterConfig(t, proxyURL)
	olruconfig := oconfig.LRU
	operiodic := oconfig.Periodic

	setClusterConfig(t, proxyURL, configRegression)

	nconfig := getClusterConfig(t, proxyURL)
	nlruconfig := nconfig.LRU
	nperiodic := nconfig.Periodic

	if nperiodic.StatsTimeStr != configRegression["periodic.stats_time"] {
		t.Errorf("StatsTime was not set properly: %v, should be: %v",
			nperiodic.StatsTimeStr, configRegression["periodic.stats_time"])
	} else {
		o := operiodic.StatsTimeStr
		setClusterConfig(t, proxyURL, cmn.SimpleKVs{"periodic.stats_time": o})
	}
	if nlruconfig.DontEvictTimeStr != configRegression["lru.dont_evict_time"] {
		t.Errorf("DontEvictTime was not set properly: %v, should be: %v",
			nlruconfig.DontEvictTimeStr, configRegression["lru.dont_evict_time"])
	} else {
		setClusterConfig(t, proxyURL, cmn.SimpleKVs{"lru.dont_evict_time": olruconfig.DontEvictTimeStr})
	}
	if nlruconfig.CapacityUpdTimeStr != configRegression["lru.capacity_upd_time"] {
		t.Errorf("CapacityUpdTime was not set properly: %v, should be: %v",
			nlruconfig.CapacityUpdTimeStr, configRegression["lru.capacity_upd_time"])
	} else {
		setClusterConfig(t, proxyURL, cmn.SimpleKVs{"lru.capacity_upd_time": olruconfig.CapacityUpdTimeStr})
	}
	if hw, err := strconv.Atoi(configRegression["lru.highwm"]); err != nil {
		t.Fatalf("Error parsing HighWM: %v", err)
	} else if nlruconfig.HighWM != int64(hw) {
		t.Errorf("HighWatermark was not set properly: %d, should be: %d",
			nlruconfig.HighWM, hw)
	} else {
		oldhwmStr, err := cmn.ConvertToString(olruconfig.HighWM)
		if err != nil {
			t.Fatalf("Error parsing HighWM: %v", err)
		}
		setClusterConfig(t, proxyURL, cmn.SimpleKVs{"lru.highwm": oldhwmStr})
	}
	if lw, err := strconv.Atoi(configRegression["lru.lowwm"]); err != nil {
		t.Fatalf("Error parsing LowWM: %v", err)
	} else if nlruconfig.LowWM != int64(lw) {
		t.Errorf("LowWatermark was not set properly: %d, should be: %d",
			nlruconfig.LowWM, lw)
	} else {
		oldlwmStr, err := cmn.ConvertToString(olruconfig.LowWM)
		if err != nil {
			t.Fatalf("Error parsing LowWM: %v", err)
		}
		setClusterConfig(t, proxyURL, cmn.SimpleKVs{"lru.lowwm": oldlwmStr})
	}
	if pt, err := cmn.ParseBool(configRegression["lru.enabled"]); err != nil {
		t.Fatalf("Error parsing lru.enabled: %v", err)
	} else if nlruconfig.Enabled != pt {
		t.Errorf("lru.enabled was not set properly: %v, should be %v",
			nlruconfig.Enabled, pt)
	} else {
		setClusterConfig(t, proxyURL, cmn.SimpleKVs{"lru.enabled": fmt.Sprintf("%v", olruconfig.Enabled)})
	}
}

func TestLRU(t *testing.T) {
	var (
		errCh      = make(chan error, 100)
		usedPct    = int32(100)
		proxyURL   = getPrimaryURL(t, proxyURLReadOnly)
		baseParams = tutils.DefaultBaseAPIParams(t)
	)

	if !isCloudBucket(t, proxyURL, clibucket) {
		t.Skipf("%s requires a cloud bucket", t.Name())
	}

	getRandomFiles(proxyURL, 20, clibucket, "", t, errCh)
	// The error could be no object in the bucket. In that case, consider it as not an error;
	// this test will be skipped
	if len(errCh) != 0 {
		t.Logf("LRU: need a cloud bucket with at least 20 objects")
		t.Skip("skipping - cannot test LRU.")
	}

	//
	// remember targets' watermarks
	//
	smap := getClusterMap(t, proxyURL)
	lwms := make(map[string]interface{})
	hwms := make(map[string]interface{})
	bytesEvictedOrig := make(map[string]int64)
	filesEvictedOrig := make(map[string]int64)
	for k, di := range smap.Tmap {
		cfg := getDaemonConfig(t, proxyURL, di.ID())
		lwms[k] = cfg.LRU.LowWM
		hwms[k] = cfg.LRU.HighWM
	}
	// add a few more
	getRandomFiles(proxyURL, 3, clibucket, "", t, errCh)
	selectErr(errCh, "get", t, true)
	//
	// find out min usage %% across all targets
	//
	stats := getClusterStats(t, proxyURL)
	for k, v := range stats.Target {
		bytesEvictedOrig[k], filesEvictedOrig[k] = getNamedTargetStats(v, "lru.evict.size"), getNamedTargetStats(v, "lru.evict.n")
		for _, c := range v.Capacity {
			usedPct = cmn.MinI32(usedPct, c.Usedpct)
		}
	}
	tutils.Logf("LRU: current min space usage in the cluster: %d%%\n", usedPct)
	var (
		lowwm  = usedPct - 5
		highwm = usedPct - 1
	)
	if int(lowwm) < 3 {
		t.Logf("The current space usage is too low (%d) for the LRU to be tested", lowwm)
		t.Skip("skipping - cannot test LRU.")
		return
	}
	oconfig := getClusterConfig(t, proxyURL)
	if t.Failed() {
		return
	}
	//
	// all targets: set new watermarks; restore upon exit
	//
	olruconfig := oconfig.LRU
	defer func() {
		oldhwm, _ := cmn.ConvertToString(olruconfig.HighWM)
		oldlwm, _ := cmn.ConvertToString(olruconfig.LowWM)
		setClusterConfig(t, proxyURL, cmn.SimpleKVs{
			"lru.dont_evict_time":   olruconfig.DontEvictTimeStr,
			"lru.capacity_upd_time": olruconfig.CapacityUpdTimeStr,
			"lru.highwm":            oldhwm,
			"lru.lowwm":             oldlwm,
		})
		for k, di := range smap.Tmap {
			hwmStr, err := cmn.ConvertToString(hwms[k])
			tassert.CheckFatal(t, err)

			lwmStr, err := cmn.ConvertToString(lwms[k])
			tassert.CheckFatal(t, err)
			setDaemonConfig(t, proxyURL, di.ID(), cmn.SimpleKVs{
				"highwm": hwmStr,
				"lowwm":  lwmStr,
			})
		}
	}()
	//
	// cluster-wide reduce dont-evict-time
	//
	lowwmStr, _ := cmn.ConvertToString(lowwm)
	hwmStr, _ := cmn.ConvertToString(highwm)
	setClusterConfig(t, proxyURL, cmn.SimpleKVs{
		"lru.dont_evict_time":   "30s",
		"lru.capacity_upd_time": "5s",
		"lru.lowwm":             lowwmStr,
		"lru.highwm":            hwmStr,
	})
	if t.Failed() {
		return
	}
	waitForBucketXactionToStart(t, cmn.ActLRU, "", baseParams)
	getRandomFiles(proxyURL, 1, clibucket, "", t, errCh)
	waitForBucketXactionToComplete(t, cmn.ActLRU, "", baseParams, rebalanceTimeout)

	//
	// results
	//
	stats = getClusterStats(t, proxyURL)
	for k, v := range stats.Target {
		bytes := getNamedTargetStats(v, "lru.evict.size") - bytesEvictedOrig[k]
		tutils.Logf("Target %s: evicted %d files - %.2f MB (%dB) total\n",
			k, getNamedTargetStats(v, "lru.evict.n")-filesEvictedOrig[k], float64(bytes)/1000/1000, bytes)
		//
		// TestingEnv() - cannot reliably verify space utilization by tmpfs
		//
		if oconfig.TestFSP.Count > 0 {
			continue
		}
		for mpath, c := range v.Capacity {
			if c.Usedpct < lowwm-1 || c.Usedpct > lowwm+1 {
				t.Errorf("Target %s failed to reach lwm %d%%: mpath %s, used space %d%%", k, lowwm, mpath, c.Usedpct)
			}
		}
	}
}

func TestPrefetchList(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}
	var (
		toprefetch    = make(chan string, numfiles)
		netprefetches = int64(0)
		proxyURL      = getPrimaryURL(t, proxyURLReadOnly)
		baseParams    = tutils.BaseAPIParams(proxyURL)
	)

	if !isCloudBucket(t, proxyURL, clibucket) {
		t.Skipf("Cannot prefetch from ais bucket %s", clibucket)
	}

	// 1. Get initial number of prefetches
	smap := getClusterMap(t, proxyURL)
	for _, v := range smap.Tmap {
		stats := getDaemonStats(t, v.PublicNet.DirectURL)
		npf, err := getPrefetchCnt(stats)
		if err != nil {
			t.Fatalf("Could not decode target stats: pre.n")
		}
		netprefetches -= npf
	}

	// 2. Get keys to prefetch
	n := int64(getMatchingKeys(proxyURL, match, clibucket, []chan string{toprefetch}, nil, t))
	close(toprefetch) // to exit for-range
	files := make([]string, 0)
	for i := range toprefetch {
		files = append(files, i)
	}

	// 3. Evict those objects from the cache and prefetch them
	tutils.Logf("Evicting and Prefetching %d objects\n", len(files))
	err := api.EvictList(baseParams, clibucket, cmn.Cloud, files, true, 0)
	if err != nil {
		t.Error(err)
	}
	err = api.PrefetchList(baseParams, clibucket, cmn.Cloud, files, true, 0)
	if err != nil {
		t.Error(err)
	}

	// 5. Ensure that all the prefetches occurred.
	for _, v := range smap.Tmap {
		stats := getDaemonStats(t, v.PublicNet.DirectURL)
		npf, err := getPrefetchCnt(stats)
		if err != nil {
			t.Fatalf("Could not decode target stats: pre.n")
		}
		netprefetches += npf
	}
	if netprefetches != n {
		t.Errorf("Did not prefetch all files: Missing %d of %d\n", (n - netprefetches), n)
	}
}

// FIXME: stop type-casting and use stats constants, here and elsewhere
func getPrefetchCnt(stats map[string]interface{}) (npf int64, err error) {
	corestats := stats["core"].(map[string]interface{})
	if _, ok := corestats["pre.n"]; !ok {
		return
	}
	npf, err = corestats["pre.n"].(json.Number).Int64()
	return
}

func TestDeleteList(t *testing.T) {
	var (
		err      error
		prefix   = ListRangeStr + "/tstf-"
		wg       = &sync.WaitGroup{}
		errCh    = make(chan error, numfiles)
		files    = make([]string, 0, numfiles)
		proxyURL = getPrimaryURL(t, proxyURLReadOnly)
	)
	if created := createBucketIfNotExists(t, proxyURL, clibucket); created {
		defer tutils.DestroyBucket(t, proxyURL, clibucket)
	}

	// 1. Put files to delete
	for i := 0; i < numfiles; i++ {
		r, err := tutils.NewRandReader(fileSize, true /* withHash */)
		tassert.CheckFatal(t, err)

		keyname := fmt.Sprintf("%s%d", prefix, i)

		wg.Add(1)
		go tutils.PutAsync(wg, proxyURL, clibucket, keyname, r, errCh)
		files = append(files, keyname)
	}
	wg.Wait()
	selectErr(errCh, "put", t, true)
	tutils.Logf("PUT done.\n")

	// 2. Delete the objects
	err = api.DeleteList(tutils.BaseAPIParams(proxyURL), clibucket, "", files, true, 0)
	if err != nil {
		t.Error(err)
	}

	// 3. Check to see that all the files have been deleted
	msg := &cmn.SelectMsg{Prefix: prefix, PageSize: int(pagesize)}
	bktlst, err := api.ListBucket(tutils.DefaultBaseAPIParams(t), clibucket, msg, 0)
	tassert.CheckFatal(t, err)
	if len(bktlst.Entries) != 0 {
		t.Errorf("Incorrect number of remaining files: %d, should be 0", len(bktlst.Entries))
	}
}

func TestPrefetchRange(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}
	var (
		netprefetches  = int64(0)
		err            error
		rmin, rmax     int64
		re             *regexp.Regexp
		proxyURL       = getPrimaryURL(t, proxyURLReadOnly)
		baseParams     = tutils.BaseAPIParams(proxyURL)
		prefetchPrefix = "regressionList/obj"
		prefetchRegex  = "\\d*"
	)

	if !isCloudBucket(t, proxyURL, clibucket) {
		t.Skipf("Cannot prefetch from ais bucket %s", clibucket)
	}

	// 1. Get initial number of prefetches
	smap := getClusterMap(t, proxyURL)
	for _, v := range smap.Tmap {
		stats := getDaemonStats(t, v.PublicNet.DirectURL)
		npf, err := getPrefetchCnt(stats)
		if err != nil {
			t.Fatalf("Could not decode target stats: pre.n")
		}
		netprefetches -= npf
	}

	// 2. Parse arguments
	if prefetchRange != "" {
		ranges := strings.Split(prefetchRange, ":")
		if rmin, err = strconv.ParseInt(ranges[0], 10, 64); err != nil {
			t.Errorf("Error parsing range min: %v", err)
		}
		if rmax, err = strconv.ParseInt(ranges[1], 10, 64); err != nil {
			t.Errorf("Error parsing range max: %v", err)
		}
	}

	// 3. Discover the number of items we expect to be prefetched
	if re, err = regexp.Compile(prefetchRegex); err != nil {
		t.Errorf("Error compiling regex: %v", err)
	}
	msg := &cmn.SelectMsg{Prefix: prefetchPrefix, PageSize: int(pagesize)}
	objsToFilter := testListBucket(t, proxyURL, clibucket, msg, 0)
	files := make([]string, 0)
	if objsToFilter != nil {
		for _, be := range objsToFilter.Entries {
			if oname := strings.TrimPrefix(be.Name, prefetchPrefix); oname != be.Name {
				s := re.FindStringSubmatch(oname)
				if s == nil {
					continue
				}
				if i, err := strconv.ParseInt(s[0], 10, 64); err != nil && s[0] != "" {
					continue
				} else if s[0] == "" || (rmin == 0 && rmax == 0) || (i >= rmin && i <= rmax) {
					files = append(files, be.Name)
				}
			}
		}
	}

	// 4. Evict those objects from the cache, and then prefetch them
	tutils.Logf("Evicting and Prefetching %d objects\n", len(files))
	err = api.EvictRange(baseParams, clibucket, cmn.Cloud, prefetchPrefix, prefetchRegex, prefetchRange, true, 0)
	if err != nil {
		t.Error(err)
	}
	err = api.PrefetchRange(baseParams, clibucket, cmn.Cloud, prefetchPrefix, prefetchRegex, prefetchRange, true, 0)
	if err != nil {
		t.Error(err)
	}

	// 5. Ensure that all the prefetches occurred
	for _, v := range smap.Tmap {
		stats := getDaemonStats(t, v.PublicNet.DirectURL)
		npf, err := getPrefetchCnt(stats)
		if err != nil {
			t.Fatalf("Could not decode target stats: pre.n")
		}
		netprefetches += npf
	}
	if netprefetches != int64(len(files)) {
		t.Errorf("Did not prefetch all files: Missing %d of %d\n",
			(int64(len(files)) - netprefetches), len(files))
	}
}

func TestDeleteRange(t *testing.T) {
	var (
		err            error
		prefix         = ListRangeStr + "/tstf-"
		quarter        = numfiles / 4
		third          = numfiles / 3
		smallrangesize = third - quarter + 1
		smallrange     = fmt.Sprintf("%d:%d", quarter, third)
		bigrange       = fmt.Sprintf("0:%d", numfiles)
		regex          = "\\d?\\d"
		wg             = &sync.WaitGroup{}
		errCh          = make(chan error, numfiles)
		proxyURL       = getPrimaryURL(t, proxyURLReadOnly)
		baseParams     = tutils.DefaultBaseAPIParams(t)
	)

	if created := createBucketIfNotExists(t, proxyURL, clibucket); created {
		defer tutils.DestroyBucket(t, proxyURL, clibucket)
	}

	// 1. Put files to delete
	for i := 0; i < numfiles; i++ {
		r, err := tutils.NewRandReader(fileSize, true /* withHash */)
		tassert.CheckFatal(t, err)

		wg.Add(1)
		go tutils.PutAsync(wg, proxyURL, clibucket, fmt.Sprintf("%s%d", prefix, i), r, errCh)
	}
	wg.Wait()
	selectErr(errCh, "put", t, true)
	tutils.Logf("PUT done.\n")

	// 2. Delete the small range of objects
	err = api.DeleteRange(tutils.BaseAPIParams(proxyURL), clibucket, "", prefix, regex, smallrange, true, 0)
	if err != nil {
		t.Error(err)
	}

	// 3. Check to see that the correct files have been deleted
	msg := &cmn.SelectMsg{Prefix: prefix, PageSize: int(pagesize)}
	bktlst, err := api.ListBucket(baseParams, clibucket, msg, 0)
	tassert.CheckFatal(t, err)
	if len(bktlst.Entries) != numfiles-smallrangesize {
		t.Errorf("Incorrect number of remaining files: %d, should be %d", len(bktlst.Entries), numfiles-smallrangesize)
	}
	filemap := make(map[string]*cmn.BucketEntry)
	for _, entry := range bktlst.Entries {
		filemap[entry.Name] = entry
	}
	for i := 0; i < numfiles; i++ {
		keyname := fmt.Sprintf("%s%d", prefix, i)
		_, ok := filemap[keyname]
		if ok && i >= quarter && i <= third {
			t.Errorf("File exists that should have been deleted: %s", keyname)
		} else if !ok && (i < quarter || i > third) {
			t.Errorf("File does not exist that should not have been deleted: %s", keyname)
		}
	}

	// 4. Delete the big range of objects
	err = api.DeleteRange(tutils.BaseAPIParams(proxyURL), clibucket, "", prefix, regex, bigrange, true, 0)
	if err != nil {
		t.Error(err)
	}

	// 5. Check to see that all the files have been deleted
	bktlst, err = api.ListBucket(baseParams, clibucket, msg, 0)
	tassert.CheckFatal(t, err)
	if len(bktlst.Entries) != 0 {
		t.Errorf("Incorrect number of remaining files: %d, should be 0", len(bktlst.Entries))
	}
}

// Testing only ais bucket objects since generally not concerned with cloud bucket object deletion
func TestStressDeleteRange(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}
	const (
		numFiles   = 20000 // FIXME: must divide by 10 and by the numReaders
		numReaders = 200
	)
	var (
		err          error
		prefix       = ListRangeStr + "/tstf-"
		wg           = &sync.WaitGroup{}
		errCh        = make(chan error, numFiles)
		proxyURL     = getPrimaryURL(t, proxyURLReadOnly)
		regex        = "\\d*"
		tenth        = numFiles / 10
		partialRange = fmt.Sprintf("%d:%d", 0, numFiles-tenth-1) // TODO: partial range with non-zero left boundary
		rnge         = fmt.Sprintf("0:%d", numFiles)
		readersList  [numReaders]tutils.Reader
		baseParams   = tutils.DefaultBaseAPIParams(t)
	)

	tutils.CreateFreshBucket(t, proxyURL, TestBucketName)

	// 1. PUT
	for i := 0; i < numReaders; i++ {
		random := rand.New(rand.NewSource(int64(i)))
		size := random.Int63n(cmn.KiB*128) + cmn.KiB/3
		tassert.CheckFatal(t, err)
		reader, err := tutils.NewRandReader(size, true /* withHash */)
		tassert.CheckFatal(t, err)
		readersList[i] = reader

		wg.Add(1)
		go func(i int, reader tutils.Reader) {
			defer wg.Done()

			for j := 0; j < numFiles/numReaders; j++ {
				objname := fmt.Sprintf("%s%d", prefix, i*numFiles/numReaders+j)
				putArgs := api.PutObjectArgs{
					BaseParams: baseParams,
					Bucket:     TestBucketName,
					Object:     objname,
					Hash:       reader.XXHash(),
					Reader:     reader,
				}
				err = api.PutObject(putArgs)
				if err != nil {
					errCh <- err
				}
				reader.Seek(0, io.SeekStart)
			}
			tutils.Progress(i, 99)
		}(i, reader)
	}
	wg.Wait()
	selectErr(errCh, "put", t, true)

	// 2. Delete a range of objects
	tutils.Logf("Deleting objects in range: %s\n", partialRange)
	err = api.DeleteRange(tutils.BaseAPIParams(proxyURL), TestBucketName, cmn.AIS, prefix, regex, partialRange, true, 0)
	if err != nil {
		t.Error(err)
	}

	// 3. Check to see that correct objects have been deleted
	expectedRemaining := tenth
	msg := &cmn.SelectMsg{Prefix: prefix, PageSize: int(pagesize)}
	bktlst, err := api.ListBucket(baseParams, TestBucketName, msg, 0)
	tassert.CheckFatal(t, err)
	if len(bktlst.Entries) != expectedRemaining {
		t.Errorf("Incorrect number of remaining objects: %d, expected: %d", len(bktlst.Entries), expectedRemaining)
	}

	filemap := make(map[string]*cmn.BucketEntry)
	for _, entry := range bktlst.Entries {
		filemap[entry.Name] = entry
	}
	for i := 0; i < numFiles; i++ {
		objname := fmt.Sprintf("%s%d", prefix, i)
		_, ok := filemap[objname]
		if ok && i < numFiles-tenth {
			t.Errorf("%s exists (expected to be deleted)", objname)
		} else if !ok && i >= numFiles-tenth {
			t.Errorf("%s does not exist", objname)
		}
	}

	// 4. Delete the entire range of objects
	tutils.Logf("Deleting objects in range: %s\n", rnge)
	err = api.DeleteRange(tutils.BaseAPIParams(proxyURL), TestBucketName, cmn.AIS, prefix, regex, rnge, true, 0)
	if err != nil {
		t.Error(err)
	}

	// 5. Check to see that all files have been deleted
	msg = &cmn.SelectMsg{Prefix: prefix, PageSize: int(pagesize)}
	bktlst, err = api.ListBucket(baseParams, TestBucketName, msg, 0)
	tassert.CheckFatal(t, err)
	if len(bktlst.Entries) != 0 {
		t.Errorf("Incorrect number of remaining files: %d, should be 0", len(bktlst.Entries))
	}

	tutils.DestroyBucket(t, proxyURL, TestBucketName)
}

//========
//
// Helpers
//
//========

func allCompleted(targetsStats map[string][]*stats.BaseXactStatsExt) bool {
	for target, targetStats := range targetsStats {
		for _, xaction := range targetStats {
			if xaction.Running() {
				tutils.Logf("%s(%d) in progress for %s\n", xaction.Kind(), xaction.ShortID(), target)
				return false
			}
		}
	}
	return true
}

func checkXactAPIErr(t *testing.T, err error) {
	if err != nil {
		if httpErr, ok := err.(*cmn.HTTPError); !ok {
			t.Fatalf("Unrecognized error from xactions request: [%v]", err)
		} else if httpErr.Status != http.StatusNotFound {
			t.Fatalf("Unable to get global rebalance stats. Error: [%v]", err)
		}
	}
}

// nolint:unparam // for now timeout is always the same but it is better to keep it generalized
func waitForBucketXactionToComplete(t *testing.T, kind, bucket string, baseParams api.BaseParams, timeout time.Duration) {
	var (
		wg    = &sync.WaitGroup{}
		ch    = make(chan error, 1)
		sleep = 3 * time.Second
		i     time.Duration
	)
	wg.Add(1)
	go func() {
		for {
			time.Sleep(sleep)
			i++
			stats, err := tutils.GetXactionStats(baseParams, kind, bucket)
			checkXactAPIErr(t, err)
			if allCompleted(stats) {
				break
			}
			if i == 1 {
				tutils.Logf("waiting for %s\n", kind)
			}
			if i*sleep > timeout {
				ch <- errors.New(kind + ": timeout")
				break
			}
		}
		wg.Done()
	}()
	wg.Wait()
	close(ch)
	for err := range ch {
		t.Fatal(err)
	}
}

func waitForBucketXactionToStart(t *testing.T, kind, bucket string, baseParams api.BaseParams, timeouts ...time.Duration) {
	var (
		start   = time.Now()
		timeout = time.Duration(0)
		logged  = false
	)

	if len(timeouts) > 0 {
		timeout = timeouts[0]
	}

	for {
		stats, err := tutils.GetXactionStats(baseParams, kind, bucket)
		checkXactAPIErr(t, err)
		for _, targetStats := range stats {
			for _, xaction := range targetStats {
				if xaction.Running() {
					return // xaction started
				}
			}
		}
		if len(stats) > 0 {
			return // all xaction finished
		}

		if !logged {
			tutils.Logf("waiting for %s to start\n", kind)
			logged = true
		}

		if timeout != 0 && time.Since(start) > timeout {
			t.Fatalf("%s has not started before %s", kind, timeout)
			return
		}

		time.Sleep(1 * time.Second)
	}
}

// Waits for both local and global rebalances to complete
// If they were not started, this function treats them as completed
// and returns. If timeout set, if any of rebalances doesn't complete before timeout
// the function ends with fatal
func waitForRebalanceToComplete(t *testing.T, baseParams api.BaseParams, timeouts ...time.Duration) {
	start := time.Now()
	time.Sleep(time.Second * 10)
	wg := &sync.WaitGroup{}
	wg.Add(2)
	ch := make(chan error, 2)

	timeout := time.Duration(0)
	if len(timeouts) > 0 {
		timeout = timeouts[0]
	}
	sleep := time.Second * 10
	go func() {
		var logged bool
		defer wg.Done()
		for {
			time.Sleep(sleep)
			globalRebalanceStats, err := tutils.GetXactionStats(baseParams, cmn.ActGlobalReb)
			checkXactAPIErr(t, err)

			if allCompleted(globalRebalanceStats) {
				return
			}
			if !logged {
				tutils.Logf("waiting for global rebalance\n")
				logged = true
			}

			if timeout.Nanoseconds() != 0 && time.Since(start) > timeout {
				ch <- errors.New("global rebalance has not completed before " + timeout.String())
				return
			}
		}
	}()

	go func() {
		var logged bool
		defer wg.Done()
		for {
			time.Sleep(sleep)
			localRebalanceStats, err := tutils.GetXactionStats(baseParams, cmn.ActLocalReb)
			checkXactAPIErr(t, err)

			if allCompleted(localRebalanceStats) {
				return
			}
			if !logged {
				tutils.Logf("waiting for local rebalance\n")
				logged = true
			}

			if timeout.Nanoseconds() != 0 && time.Since(start) > timeout {
				ch <- errors.New("global rebalance has not completed before " + timeout.String())
				return
			}
		}
	}()

	wg.Wait()
	close(ch)

	for err := range ch {
		t.Fatal(err)
	}
}

func getClusterStats(t *testing.T, proxyURL string) (stats stats.ClusterStats) {
	baseParams := tutils.BaseAPIParams(proxyURL)
	clusterStats, err := api.GetClusterStats(baseParams)
	tassert.CheckFatal(t, err)
	return clusterStats
}

func getNamedTargetStats(trunner *stats.Trunner, name string) int64 {
	v, ok := trunner.Core.Tracker[name]
	if !ok {
		return 0
	}
	return v.Value
}

func getDaemonStats(t *testing.T, url string) (stats map[string]interface{}) {
	q := tutils.GetWhatRawQuery(cmn.GetWhatStats, "")
	url = fmt.Sprintf("%s?%s", url+cmn.URLPath(cmn.Version, cmn.Daemon), q)
	resp, err := tutils.DefaultHTTPClient.Get(url)
	if err != nil {
		t.Fatalf("Failed to perform get, err = %v", err)
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body, err = %v", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		t.Fatalf("HTTP error = %d, message = %s", err, string(b))
	}

	dec := jsoniter.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	// If this isn't used, json.Unmarshal converts uint32s to floats, losing precision
	err = dec.Decode(&stats)
	if err != nil {
		t.Fatalf("Failed to unmarshal config: %v", err)
	}

	return
}

func getClusterMap(t *testing.T, url string) *cluster.Smap {
	baseParams := tutils.BaseAPIParams(url)
	time.Sleep(time.Second * 2)
	smap, err := api.GetClusterMap(baseParams)
	tassert.CheckFatal(t, err)
	return smap
}

func getClusterConfig(t *testing.T, proxyURL string) (config *cmn.Config) {
	primary, err := tutils.GetPrimaryProxy(proxyURL)
	tassert.CheckFatal(t, err)
	return getDaemonConfig(t, proxyURL, primary.ID())
}

func getDaemonConfig(t *testing.T, proxyURL string, nodeID string) (config *cmn.Config) {
	var err error
	baseParams := tutils.BaseAPIParams(proxyURL)
	config, err = api.GetDaemonConfig(baseParams, nodeID)
	tassert.CheckFatal(t, err)
	return
}

func setDaemonConfig(t *testing.T, proxyURL string, nodeID string, nvs cmn.SimpleKVs) {
	baseParams := tutils.BaseAPIParams(proxyURL)
	err := api.SetDaemonConfig(baseParams, nodeID, nvs)
	tassert.CheckFatal(t, err)
}

func setClusterConfig(t *testing.T, proxyURL string, nvs cmn.SimpleKVs) {
	baseParams := tutils.BaseAPIParams(proxyURL)
	err := api.SetClusterConfig(baseParams, nvs)
	tassert.CheckFatal(t, err)
}

func selectErr(errCh chan error, verb string, t *testing.T, errisfatal bool) {
	select {
	case err := <-errCh:
		if errisfatal {
			t.Fatalf("Failed to %s objects: %v", verb, err)
		} else {
			t.Errorf("Failed to %s objects: %v", verb, err)
		}
	default:
	}
}
