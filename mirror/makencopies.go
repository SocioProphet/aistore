// Package mirror provides local mirroring and replica management
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package mirror

import (
	"fmt"
	"os"
	"runtime"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
)

const pkgName = "mirror"
const (
	throttleNumObjects = 16                      // unit of self-throttling
	minThrottleSize    = 256 * cmn.KiB           // throttle every 4MB (= 16K*256) or greater
	logNumProcessed    = throttleNumObjects * 16 // unit of house-keeping
	MaxNCopies         = 16                      // validation
)

// XactBckMakeNCopies runs in a background, traverses all local mountpaths, and makes sure
// the bucket is N-way replicated (where N >= 1)

type (
	XactBckMakeNCopies struct {
		xactBckBase
		slab   *memsys.Slab2
		copies int
	}
	mncJogger struct { // one per mountpath
		joggerBckBase
		parent *XactBckMakeNCopies
		buf    []byte
	}
)

func init() {
	if logLvl, ok := cmn.CheckDebug(pkgName); ok {
		glog.SetV(glog.SmoduleMirror, logLvl)
	}
}

//
// public methods
//

func NewXactMNC(id int64, bck *cluster.Bck, t cluster.Target, slab *memsys.Slab2, copies int) *XactBckMakeNCopies {
	return &XactBckMakeNCopies{
		xactBckBase: *newXactBckBase(id, cmn.ActMakeNCopies, bck, t),
		slab:        slab,
		copies:      copies,
	}
}

func (r *XactBckMakeNCopies) Run() (err error) {
	var mpathersCount int
	if mpathersCount, err = r.init(); err != nil {
		return
	}
	glog.Infoln(r.String(), "copies=", r.copies)
	return r.xactBckBase.run(mpathersCount)
}

func ValidateNCopies(prefix string, copies int) error {
	if _, err := cmn.CheckI64Range(int64(copies), 1, MaxNCopies); err != nil {
		return fmt.Errorf("number of copies (%d) %s", copies, err.Error())
	}
	availablePaths, _ := fs.Mountpaths.Get()
	mpathCount := len(availablePaths)
	if mpathCount == 0 {
		return fmt.Errorf("%s: no mountpaths", prefix)
	}
	if copies > mpathCount {
		return fmt.Errorf("%s: number of copies (%d) exceeds the number of mountpaths (%d)", prefix, copies, mpathCount)
	}
	return nil
}

//
// private methods
//

func (r *XactBckMakeNCopies) init() (mpathCount int, err error) {
	tname := r.Target().Snode().Name()
	if err = ValidateNCopies(tname, r.copies); err != nil {
		return
	}

	var (
		availablePaths, _ = fs.Mountpaths.Get()
		config            = cmn.GCO.Get()
	)
	mpathCount = len(availablePaths)

	r.xactBckBase.init(mpathCount)
	for _, mpathInfo := range availablePaths {
		mncJogger := newMNCJogger(r, mpathInfo, config)
		mpathLC := mpathInfo.MakePath(fs.ObjectType, r.Provider())
		r.mpathers[mpathLC] = mncJogger
	}
	for _, mpather := range r.mpathers {
		mncJogger := mpather.(*mncJogger)
		go mncJogger.jog()
	}
	return
}

func (r *XactBckMakeNCopies) Description() string {
	return "delete or add local object copies (replicas)"
}

//
// mpath mncJogger - main
//

func newMNCJogger(parent *XactBckMakeNCopies, mpathInfo *fs.MountpathInfo, config *cmn.Config) *mncJogger {
	j := &mncJogger{
		joggerBckBase: joggerBckBase{parent: &parent.xactBckBase, mpathInfo: mpathInfo, config: config},
		parent:        parent,
	}
	j.joggerBckBase.callback = j.delAddCopies
	return j
}

func (j *mncJogger) jog() {
	glog.Infof("jogger[%s/%s] started", j.mpathInfo, j.parent.Bucket())
	j.buf = j.parent.slab.Alloc()
	j.joggerBckBase.jog()
	j.parent.slab.Free(j.buf)
}

func (j *mncJogger) delAddCopies(lom *cluster.LOM) (err error) {
	if j.parent.Aborted() {
		return fmt.Errorf("makenaction xaction was aborted")
	}

	var size int64
	if n := lom.NumCopies(); n == j.parent.copies {
		return nil
	} else if n > j.parent.copies {
		size, err = delCopies(lom, j.parent.copies)
	} else {
		size, err = addCopies(lom, j.parent.copies, j.parent.Mpathers(), j.buf)
	}

	if os.IsNotExist(err) {
		return nil
	}

	j.num++
	j.size += size

	j.parent.ObjectsInc()
	j.parent.BytesAdd(size)

	if (j.num % throttleNumObjects) == 0 {
		if j.size > minThrottleSize*throttleNumObjects {
			j.size = 0
			if errstop := j.yieldTerm(); errstop != nil {
				return errstop
			}
		}
		if (j.num % logNumProcessed) == 0 {
			glog.Infof("jogger[%s/%s] processed %d objects...", j.mpathInfo, j.parent.Bucket(), j.num)
			j.config = cmn.GCO.Get()
		}
	} else {
		runtime.Gosched()
	}
	return
}
