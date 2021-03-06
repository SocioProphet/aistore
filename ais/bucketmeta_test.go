// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("BMD marshal and unmarshal", func() {
	const (
		mpath = "/tmp"
	)

	var (
		bmd *bucketMD
		cfg *cmn.Config
	)

	BeforeEach(func() {
		// Set path for proxy (it uses Confdir)
		tmpCfg := cmn.GCO.BeginUpdate()
		tmpCfg.Confdir = mpath
		cmn.GCO.CommitUpdate(tmpCfg)
		cfg = cmn.GCO.Get()

		bmd = newBucketMD()
		for _, provider := range []string{cmn.ProviderAIS, cmn.ProviderAmazon} {
			for i := 0; i < 10; i++ {
				bmd.add(&cluster.Bck{
					Name:     fmt.Sprintf("local%d", i),
					Provider: provider,
				}, cmn.DefaultBucketProps())
			}
		}
	})

	for _, node := range []string{cmn.Target, cmn.Proxy} {
		makeBMDOwner := func() bmdOwner {
			var bmdo bmdOwner
			switch node {
			case cmn.Target:
				bmdo = newBMDOwnerTgt()
			case cmn.Proxy:
				bmdo = newBMDOwnerPrx(cfg)
			}
			return bmdo
		}

		Describe(node, func() {
			var (
				bmdo bmdOwner
			)

			BeforeEach(func() {
				bmdo = makeBMDOwner()
				bmdo.put(bmd)
			})

			It(fmt.Sprintf("should correctly save and load bmd for %s", node), func() {
				bmdo.init()
				Expect(bmdo.Get()).To(Equal(&bmd.BMD))
			})

			It(fmt.Sprintf("should correctly save and check for incorrect data for %s", node), func() {
				bmdFullPath := filepath.Join(mpath, bmdFname)
				f, err := os.OpenFile(bmdFullPath, os.O_RDWR, 0)
				Expect(err).NotTo(HaveOccurred())
				_, err = f.WriteAt([]byte("xxxxxxxxxxxx"), 10)
				Expect(err).NotTo(HaveOccurred())
				Expect(f.Close()).NotTo(HaveOccurred())

				bmdo = makeBMDOwner()
				bmdo.init()

				Expect(bmdo.Get()).NotTo(Equal(&bmd.BMD))
			})
		})
	}
})
