// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This specific file handles the CLI commands related to specific (not supported for other entities) bucket actions.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"github.com/urfave/cli"
)

var (
	bucketSpecificCmdsFlags = map[string][]cli.Flag{
		commandSetCopies: {
			providerFlag,
			copiesFlag,
		},
		commandECEncode: {
			providerFlag,
		},
	}

	bucketSpecificCmds = []cli.Command{
		{
			Name:         commandSetCopies,
			Usage:        "configures a bucket for n-way mirroring",
			ArgsUsage:    bucketArgument,
			Flags:        bucketSpecificCmdsFlags[commandSetCopies],
			Action:       setCopiesHandler,
			BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, false /* multiple */, false /* separator */),
		},
		{
			Name:         commandECEncode,
			Usage:        "makes all objects in a bucket erasure coded",
			ArgsUsage:    bucketArgument,
			Flags:        bucketSpecificCmdsFlags[commandECEncode],
			Action:       ecEncodeHandler,
			BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, false /* multiple */, false /* separator */),
		},
	}
)

func setCopiesHandler(c *cli.Context) (err error) {
	bucket := c.Args().First()
	if bucket, _, err = validateBucket(c, bucket, "", false /* optional */); err != nil {
		return
	}
	return configureNCopies(c, bucket)
}

func ecEncodeHandler(c *cli.Context) (err error) {
	bucket := c.Args().First()
	if bucket, _, err = validateBucket(c, bucket, "", false /* optional */); err != nil {
		return
	}
	return ecEncode(c, bucket)
}
