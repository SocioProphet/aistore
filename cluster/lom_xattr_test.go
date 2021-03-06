/*
* Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package cluster_test

import (
	"os"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("LOM Xattributes", func() {
	const (
		tmpDir     = "/tmp/lom_xattr_test"
		xattrMpath = tmpDir + "/xattr"
		copyMpath  = tmpDir + "/copy"

		bucketLocal = "LOM_TEST_Local"
	)

	_ = fs.CSM.RegisterFileType(fs.ObjectType, &fs.ObjectContentResolver{})
	_ = fs.CSM.RegisterFileType(fs.WorkfileType, &fs.WorkfileContentResolver{})

	var (
		tMock         = cluster.NewTargetMock(cluster.NewBaseBownerMock(bucketLocal))
		copyMpathInfo *fs.MountpathInfo
		mix           = fs.MountpathInfo{Path: xattrMpath}
	)

	BeforeEach(func() {
		_ = cmn.CreateDir(xattrMpath)
		_ = cmn.CreateDir(copyMpath)

		fs.Mountpaths.DisableFsIDCheck()
		_ = fs.Mountpaths.Add(xattrMpath)
		_ = fs.Mountpaths.Add(copyMpath)

		available, _ := fs.Mountpaths.Get()
		copyMpathInfo = available[copyMpath]
	})

	AfterEach(func() {
		_ = fs.Mountpaths.Remove(xattrMpath)
		_ = fs.Mountpaths.Remove(copyMpath)
		_ = os.RemoveAll(tmpDir)
	})

	Describe("xattrs", func() {
		var (
			testFileSize   = 456
			testObjectName = "xattr-foldr/test-obj.ext"

			// Bucket needs to have checksum enabled
			localFQN = mix.MakePathBucketObject(fs.ObjectType, bucketLocal, cmn.AIS, testObjectName)

			fqns = []string{
				copyMpath + "/copy/fqn",
				copyMpath + "/other/copy/fqn",
			}
		)

		Describe("Persist", func() {
			It("should save correct meta to disk", func() {
				lom := filePut(localFQN, testFileSize, tMock)
				lom.SetCksum(cmn.NewCksum(cmn.ChecksumXXHash, "test_checksum"))
				lom.SetVersion("dummy_version")
				Expect(lom.AddCopy(fqns[0], copyMpathInfo)).NotTo(HaveOccurred())
				Expect(lom.AddCopy(fqns[1], copyMpathInfo)).NotTo(HaveOccurred())

				b, err := fs.GetXattr(localFQN, cmn.XattrLOM)
				Expect(b).ToNot(BeEmpty())
				Expect(err).NotTo(HaveOccurred())

				lom.Uncache()
				newLom := NewBasicLom(localFQN, tMock)
				err = newLom.Load(false)
				Expect(err).NotTo(HaveOccurred())
				Expect(lom.Cksum()).To(BeEquivalentTo(newLom.Cksum()))
				Expect(lom.Version()).To(BeEquivalentTo(newLom.Version()))
				Expect(lom.GetCopies()).To(HaveLen(3))
				Expect(lom.GetCopies()).To(BeEquivalentTo(newLom.GetCopies()))
			})

			It("should override old values", func() {
				lom := filePut(localFQN, testFileSize, tMock)
				lom.SetCksum(cmn.NewCksum(cmn.ChecksumXXHash, "test_checksum"))
				lom.SetVersion("dummy_version1")
				Expect(lom.AddCopy(fqns[0], copyMpathInfo)).NotTo(HaveOccurred())
				Expect(lom.AddCopy(fqns[1], copyMpathInfo)).NotTo(HaveOccurred())

				lom.SetCksum(cmn.NewCksum(cmn.ChecksumXXHash, "test_checksum"))
				lom.SetVersion("dummy_version2")
				Expect(lom.AddCopy(fqns[0], copyMpathInfo)).NotTo(HaveOccurred())
				Expect(lom.AddCopy(fqns[1], copyMpathInfo)).NotTo(HaveOccurred())

				b, err := fs.GetXattr(localFQN, cmn.XattrLOM)
				Expect(b).ToNot(BeEmpty())
				Expect(err).NotTo(HaveOccurred())

				lom.Uncache()
				newLom := NewBasicLom(localFQN, tMock)
				err = newLom.Load(false)
				Expect(err).NotTo(HaveOccurred())
				Expect(lom.Cksum()).To(BeEquivalentTo(newLom.Cksum()))
				Expect(lom.Version()).To(BeEquivalentTo(newLom.Version()))
				Expect(lom.GetCopies()).To(HaveLen(3))
				Expect(lom.GetCopies()).To(BeEquivalentTo(newLom.GetCopies()))
			})
		})

		Describe("LoadMetaFromFS", func() {
			It("should read fresh meta from fs", func() {
				createTestFile(localFQN, testFileSize)
				lom1 := NewBasicLom(localFQN, tMock)
				lom2 := NewBasicLom(localFQN, tMock)
				lom1.SetCksum(cmn.NewCksum(cmn.ChecksumXXHash, "test_checksum"))
				lom1.SetVersion("dummy_version")
				Expect(lom1.AddCopy(fqns[0], copyMpathInfo)).NotTo(HaveOccurred())
				Expect(lom1.AddCopy(fqns[1], copyMpathInfo)).NotTo(HaveOccurred())

				err := lom2.LoadMetaFromFS()
				Expect(err).NotTo(HaveOccurred())

				Expect(lom1.Cksum()).To(BeEquivalentTo(lom2.Cksum()))
				Expect(lom1.Version()).To(BeEquivalentTo(lom2.Version()))
				Expect(lom1.GetCopies()).To(HaveLen(3))
				Expect(lom1.GetCopies()).To(BeEquivalentTo(lom2.GetCopies()))
			})

			It("should fail when checksum does not match", func() {
				createTestFile(localFQN, testFileSize)
				lom := NewBasicLom(localFQN, tMock)
				lom.SetCksum(cmn.NewCksum(cmn.ChecksumXXHash, "test_checksum"))
				lom.SetVersion("dummy_version")
				Expect(lom.AddCopy(fqns[0], copyMpathInfo)).NotTo(HaveOccurred())
				Expect(lom.AddCopy(fqns[1], copyMpathInfo)).NotTo(HaveOccurred())

				b, err := fs.GetXattr(localFQN, cmn.XattrLOM)
				Expect(err).NotTo(HaveOccurred())
				b[0] = b[0] + 1 // changing first byte of meta checksum
				Expect(fs.SetXattr(localFQN, cmn.XattrLOM, b)).NotTo(HaveOccurred())

				err = lom.LoadMetaFromFS()
				Expect(err).To(HaveOccurred())
			})

			It("should fail when meta is corrupted", func() {
				// This test is supposed to end with LoadMetaFromFS error
				// not with nil pointer exception / panic
				createTestFile(localFQN, testFileSize)
				lom := NewBasicLom(localFQN, tMock)
				lom.SetCksum(cmn.NewCksum(cmn.ChecksumXXHash, "test_checksum"))
				lom.SetVersion("dummy_version")
				Expect(lom.AddCopy(fqns[0], copyMpathInfo)).NotTo(HaveOccurred())
				Expect(lom.AddCopy(fqns[1], copyMpathInfo)).NotTo(HaveOccurred())

				Expect(fs.SetXattr(localFQN, cmn.XattrLOM, []byte("1321\nwr;as\n;, ;\n\n;;,,dadsa;aa\n"))).NotTo(HaveOccurred())
				err := lom.LoadMetaFromFS()
				Expect(err).To(HaveOccurred())
			})
		})
	})
})
