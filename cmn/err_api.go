// Package cmn provides common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"fmt"
	"os"
)

///////////////////////////////////////////////////////
// API errors - the errors that API calls may return //
///////////////////////////////////////////////////////

type (
	ErrorBucketAlreadyExists     struct{ bucket string }
	ErrorCloudBucketDoesNotExist struct{ bucket string }
	ErrorCloudBucketOffline      struct{ bucket, provider string }
	ErrorBucketDoesNotExist      struct{ bucket string }

	ErrorCapacityExceeded struct {
		prefix string
		high   int64
		used   int32
		oos    bool
	}

	BucketAccessDenied struct{ errAccessDenied }
	ObjectAccessDenied struct{ errAccessDenied }
	errAccessDenied    struct {
		entity      string
		operation   string
		accessAttrs uint64
	}

	InvalidCksumError struct {
		expectedHash string
		actualHash   string
	}
	NoMountpathError struct {
		mpath string
	}
	InvalidMountpathError struct {
		mpath string
		cause string
	}
	TimeoutError struct {
		cause string
	}
	XactionNotFoundError struct {
		cause string
	}
	ObjDefunctErr struct {
		name   string // object's name
		d1, d2 uint64 // lom.md.(bucket-ID) and lom.bck.(bucket-ID), respectively
	}
)

func NewErrorBucketAlreadyExists(bucket string) *ErrorBucketAlreadyExists {
	return &ErrorBucketAlreadyExists{bucket: bucket}
}
func (e *ErrorBucketAlreadyExists) Error() string {
	return fmt.Sprintf("bucket %q already exists", e.bucket)
}

func NewErrorCloudBucketDoesNotExist(bucket string) *ErrorCloudBucketDoesNotExist {
	return &ErrorCloudBucketDoesNotExist{bucket: bucket}
}
func (e *ErrorCloudBucketDoesNotExist) Error() string {
	return fmt.Sprintf("cloud bucket %q does not exist", e.bucket)
}

func NewErrorCloudBucketOffline(bucket, provider string) *ErrorCloudBucketOffline {
	return &ErrorCloudBucketOffline{bucket: bucket, provider: provider}
}
func (e *ErrorCloudBucketOffline) Error() string {
	return fmt.Sprintf("%s bucket %q is currently unreachable", e.provider, e.bucket)
}

func NewErrorBucketDoesNotExist(bucket string) *ErrorBucketDoesNotExist {
	return &ErrorBucketDoesNotExist{bucket: bucket}
}
func (e *ErrorBucketDoesNotExist) Error() string {
	return fmt.Sprintf("%q does not appear to be an ais bucket or does not exist", e.bucket)
}

func (e *errAccessDenied) String() string {
	return fmt.Sprintf("%s: %s access denied (%#x)", e.entity, e.operation, e.accessAttrs)
}
func (e *BucketAccessDenied) Error() string { return "bucket " + e.String() }
func (e *ObjectAccessDenied) Error() string { return "object " + e.String() }

func NewBucketAccessDenied(bucket, oper string, aattrs uint64) *BucketAccessDenied {
	return &BucketAccessDenied{errAccessDenied{bucket, oper, aattrs}}
}

func NewErrorCapacityExceeded(prefix string, high int64, used int32, oos bool) *ErrorCapacityExceeded {
	return &ErrorCapacityExceeded{prefix: prefix, high: high, used: used, oos: oos}
}

func (e *ErrorCapacityExceeded) Error() string {
	if e.oos {
		return fmt.Sprintf("%s: OUT OF SPACE (used %d%% of total available capacity)", e.prefix, e.used)
	}
	return fmt.Sprintf("%s: used capacity %d%% exceeded high watermark %d%%", e.prefix, e.used, e.high)
}

func (e InvalidCksumError) Error() string {
	return fmt.Sprintf("checksum: expected [%s], actual [%s]", e.expectedHash, e.actualHash)
}
func NewInvalidCksumError(eHash string, aHash string) InvalidCksumError {
	return InvalidCksumError{actualHash: aHash, expectedHash: eHash}
}
func (e InvalidCksumError) Expected() string { return e.expectedHash }

func (e NoMountpathError) Error() string                { return "mountpath [" + e.mpath + "] doesn't exist" }
func NewNoMountpathError(mpath string) NoMountpathError { return NoMountpathError{mpath} }

func (e InvalidMountpathError) Error() string {
	return "invalid mountpath [" + e.mpath + "]; " + e.cause
}
func NewInvalidaMountpathError(mpath, cause string) InvalidMountpathError {
	return InvalidMountpathError{mpath: mpath, cause: cause}
}

func (e TimeoutError) Error() string            { return e.cause + " timed out" }
func NewTimeoutError(cause string) TimeoutError { return TimeoutError{cause} }

func (e XactionNotFoundError) Error() string { return "xaction '" + e.cause + "' not found" }
func NewXactionNotFoundError(cause string) XactionNotFoundError {
	return XactionNotFoundError{cause: cause}
}

func (e ObjDefunctErr) Error() string {
	return fmt.Sprintf("%s is defunct (%d != %d)", e.name, e.d1, e.d2)
}
func NewObjDefunctError(name string, d1, d2 uint64) ObjDefunctErr { return ObjDefunctErr{name, d1, d2} }

//////////////////////////////////
// error grouping, error levels //
//////////////////////////////////

// nought: not a thing

func IsErrBucketNought(err error) bool {
	if _, ok := err.(*ErrorBucketDoesNotExist); ok {
		return true
	}
	if _, ok := err.(*ErrorCloudBucketDoesNotExist); ok {
		return true
	}
	_, ok := err.(*ErrorCloudBucketOffline)
	return ok
}

func IsErrObjNought(err error) bool {
	if IsNotObjExist(err) {
		return true
	}
	if _, ok := err.(ObjDefunctErr); ok {
		return true
	}
	return false
}

func IsNotObjExist(err error) bool    { return os.IsNotExist(err) }
func IsErrBucketLevel(err error) bool { return IsErrBucketNought(err) }
func IsErrObjLevel(err error) bool    { return IsErrObjNought(err) }
