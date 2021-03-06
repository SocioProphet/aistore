// Package tutils provides common low-level utilities for all aistore unit and integration tests
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package tutils

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/OneOfOne/xxhash"
)

const (
	SkipMsg = "skipping test in short mode."
)

func prependTime(msg string) string {
	return fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05.000000"), msg)
}

func Logln(msg string) {
	if testing.Verbose() {
		fmt.Fprintln(os.Stdout, prependTime(msg))
	}
}

func Logf(msg string, args ...interface{}) {
	if testing.Verbose() {
		fmt.Fprintf(os.Stdout, prependTime(msg), args...)
	}
}

func Progress(id int, period int) {
	if id > 0 && id%period == 0 {
		Logf("%3d: done.\n", id)
	}
}

// Generates strong random string or fallbacks to weak if error occurred
// during generation.
func GenRandomString(fnLen int) string {
	bytes := make([]byte, fnLen)
	rand.Read(bytes)
	for i, b := range bytes {
		bytes[i] = cmn.LetterBytes[b%byte(len(cmn.LetterBytes))]
	}
	return string(bytes)
}

// Generates an object name that hashes to a different target than `baseName`.
func GenerateNotConflictingObjectName(baseName, newNamePrefix string, bck *cluster.Bck, smap *cluster.Smap) string {
	// Init digests - HrwTarget() requires it
	smap.InitDigests()

	newName := newNamePrefix

	baseNameHrw, _ := cluster.HrwTarget(bck.MakeUname(baseName), smap)
	newNameHrw, _ := cluster.HrwTarget(bck.MakeUname(newName), smap)

	for i := 0; baseNameHrw == newNameHrw; i++ {
		newName = newNamePrefix + strconv.Itoa(i)
		newNameHrw, _ = cluster.HrwTarget(bck.MakeUname(newName), smap)
	}
	return newName
}

func GenerateNonexistentBucketName(prefix string, baseParams api.BaseParams) (string, error) {
	for i := 0; i < 100; i++ {
		name := prefix + GenRandomString(8)
		_, err := api.HeadBucket(baseParams, name)
		if err == nil {
			continue
		}
		errHTTP, ok := err.(*cmn.HTTPError)
		if !ok {
			return "", fmt.Errorf("error generating bucket name: expected error of type *cmn.HTTPError, but got: %T", err)
		}
		if errHTTP.Status == http.StatusNotFound {
			return name, nil
		}

		return "", fmt.Errorf("error generating bucket name: unexpected HEAD request error: %v", err)
	}

	return "", errors.New("error generating bucket name: too many tries gave no result")
}

// copyRandWithHash reads data from random source and writes it to a writer while
// optionally computing xxhash
// See related: memsys_test.copyRand
func copyRandWithHash(w io.Writer, size int64, withHash bool, rnd *rand.Rand) (string, error) {
	var (
		rem   = size
		shash string
		h     *xxhash.XXHash64
	)
	buf, s := Mem2.AllocForSize(cmn.DefaultBufSize)
	blkSize := int64(len(buf))
	defer s.Free(buf)

	if withHash {
		h = xxhash.New64()
	}
	for i := int64(0); i <= size/blkSize; i++ {
		n := int(cmn.MinI64(blkSize, rem))
		rnd.Read(buf[:n])
		m, err := w.Write(buf[:n])
		if err != nil {
			return "", err
		}

		if withHash {
			h.Write(buf[:m])
		}
		cmn.Assert(m == n)
		rem -= int64(m)
	}
	if withHash {
		shash = cmn.HashToStr(h)
	}
	return shash, nil
}
