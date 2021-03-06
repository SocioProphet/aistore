// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
)

type signalError struct {
	sig syscall.Signal
}

func (se *signalError) Error() string { return fmt.Sprintf("Signal %d", se.sig) }
func pexit()                          { time.Sleep(time.Second * 3); os.Exit(1) }

//===========================================================================
//
// sig runner
//
//===========================================================================
type sigrunner struct {
	cmn.Named
	chsig chan os.Signal
}

// signal handler
func (r *sigrunner) Run() error {
	r.chsig = make(chan os.Signal, 1)
	signal.Notify(r.chsig,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	s, ok := <-r.chsig
	signal.Stop(r.chsig) // stop immediately
	if ok {
		go pexit()
	}
	switch s {
	case syscall.SIGHUP: // kill -SIGHUP XXXX
		return &signalError{sig: syscall.SIGHUP}
	case syscall.SIGINT: // kill -SIGINT XXXX or Ctrl+c
		return &signalError{sig: syscall.SIGINT}
	case syscall.SIGTERM: // kill -SIGTERM XXXX
		return &signalError{sig: syscall.SIGTERM}
	case syscall.SIGQUIT: // kill -SIGQUIT XXXX
		return &signalError{sig: syscall.SIGQUIT}
	}
	return nil
}

func (r *sigrunner) Stop(err error) {
	glog.Infof("Stopping %s, err: %v", r.Getname(), err)
	signal.Stop(r.chsig)
	close(r.chsig)
}
