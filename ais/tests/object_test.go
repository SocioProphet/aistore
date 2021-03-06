// Package ais_test contains AIS integration tests.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package ais_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/NVIDIA/aistore/tutils/tassert"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/tutils"
)

func TestCloudBucketObject(t *testing.T) {
	if testing.Short() {
		t.Skip(tutils.SkipMsg)
	}

	const (
		getOP = "get"
		putOP = "put"
	)

	var (
		baseParams = tutils.DefaultBaseAPIParams(t)
	)

	if !isCloudBucket(t, baseParams.URL, clibucket) {
		t.Skipf("%s requires a cloud bucket", t.Name())
	}

	tests := []struct {
		ty     string
		exists bool
	}{
		{putOP, false},
		{putOP, true},
		{getOP, false},
		{getOP, true},
	}

	for _, test := range tests {
		t.Run(fmt.Sprintf("%s:%v", test.ty, test.exists), func(t *testing.T) {
			var (
				bucket = clibucket
				object = cmn.RandString(10)
			)
			if !test.exists {
				bucket = cmn.RandString(10)
			}

			reader, err := tutils.NewRandReader(cmn.KiB, false /* withHash */)
			tassert.CheckFatal(t, err)

			defer api.DeleteObject(baseParams, bucket, object, cmn.Cloud)

			switch test.ty {
			case putOP:
				err = api.PutObject(api.PutObjectArgs{
					BaseParams: baseParams,
					Bucket:     bucket,
					Provider:   cmn.Cloud,
					Object:     object,
					Reader:     reader,
				})
			case getOP:
				if test.exists {
					err = api.PutObject(api.PutObjectArgs{
						BaseParams: baseParams,
						Bucket:     bucket,
						Provider:   cmn.Cloud,
						Object:     object,
						Reader:     reader,
					})
					tassert.CheckFatal(t, err)
				}

				_, err = api.GetObject(baseParams, bucket, object)
			default:
				t.Fail()
			}

			if !test.exists {
				if err == nil {
					t.Errorf("expected error when doing %s on non existing %q bucket", test.ty, bucket)
				} else if errAsHTTPError, ok := err.(*cmn.HTTPError); !ok {
					t.Errorf("invalid error returned")
				} else if errAsHTTPError.Status != http.StatusNotFound {
					t.Errorf("returned status is incorrect")
				}
			} else {
				if err != nil {
					t.Errorf("expected no error when doing %s on existing %q bucket", test.ty, bucket)
				}
			}
		})
	}
}
