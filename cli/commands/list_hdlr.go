// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This specific file handles the CLI commands that list cluster metadata information.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"strings"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/urfave/cli"
)

var (
	listObjectFlags = []cli.Flag{
		regexFlag,
		templateFlag,
		prefixFlag,
		pageSizeFlag,
		objPropsFlag,
		objLimitFlag,
		showUnmatchedFlag,
		allItemsFlag,
		fastFlag,
		noHeaderFlag,
		pagedFlag,
		maxPagesFlag,
		markerFlag,
		cachedFlag,
	}

	listCmdsFlags = map[string][]cli.Flag{
		commandList:     listObjectFlags,
		subcmdListAIS:   listObjectFlags,
		subcmdListCloud: listObjectFlags,
		subcmdListBckProps: {
			providerFlag,
			jsonFlag,
		},
		subcmdListConfig: {
			jsonFlag,
		},
		subcmdListSmap: {
			jsonFlag,
		},
	}

	// add subcommand names for completion
	listSubcmds = []string{
		subcmdListAIS,
		subcmdListCloud,
		subcmdListBckProps,
		subcmdListConfig,
		subcmdListSmap,
	}

	listCmds = []cli.Command{
		{
			Name:         commandList,
			Usage:        "lists cluster metadata information",
			Action:       defaultListHandler,
			ArgsUsage:    listCommandArgument,
			Flags:        listCmdsFlags[commandList],
			BashComplete: listCompletions,
			Subcommands: []cli.Command{
				{
					Name:         subcmdListAIS,
					Usage:        "lists ais buckets",
					ArgsUsage:    optionalBucketWithSeparatorArgument,
					Flags:        listCmdsFlags[subcmdListAIS],
					Action:       listAISBucketsHandler,
					BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, false /* multiple */, true /* separator */, cmn.AIS),
				},
				{
					Name:         subcmdListCloud,
					Usage:        "lists cloud buckets",
					ArgsUsage:    optionalBucketWithSeparatorArgument,
					Flags:        listCmdsFlags[subcmdListCloud],
					Action:       listCloudBucketsHandler,
					BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, false /* multiple */, true /* separator */, cmn.Cloud),
				},
				{
					Name:         subcmdListBckProps,
					Usage:        "lists bucket properties",
					ArgsUsage:    bucketArgument,
					Flags:        listCmdsFlags[subcmdListBckProps],
					Action:       listBckPropsHandler,
					BashComplete: bucketCompletions([]cli.BashCompleteFunc{}, false /* multiple */, false /* separator */),
				},
				{
					Name:         subcmdListConfig,
					Usage:        "lists daemon configuration",
					ArgsUsage:    listConfigArgument,
					Flags:        listCmdsFlags[subcmdListConfig],
					Action:       listConfigHandler,
					BashComplete: daemonConfigSectionCompletions(false /* daemon optional */, true /* config optional */),
				},
				{
					Name:         subcmdListSmap,
					Usage:        "displays an smap copy of a node",
					ArgsUsage:    optionalDaemonIDArgument,
					Flags:        listCmdsFlags[subcmdListSmap],
					Action:       listSmapHandler,
					BashComplete: daemonCompletions(true /* optional */, false /* omit proxies */),
				},
			},
		},
	}
)

// Note: This handler ignores aisBucketEnvVar and aisProviderEnvVar
// because the intention is to list all buckets or auto-detect bucket provider
// for a given bucket.
func defaultListHandler(c *cli.Context) (err error) {
	bucket := c.Args().First()
	if bucket == "" {
		return listBucketNames(c, "" /* any provider */)
	}

	bucket = strings.TrimSuffix(bucket, "/")
	return listBucketObj(c, bucket, "" /* auto-detect provider */)
}

// Note: This handler ignores aisBucketEnvVar because the intention
// is to list ais bucket names if bucket name isn't given.
func listAISBucketsHandler(c *cli.Context) (err error) {
	bucket := c.Args().First()
	if bucket == "" {
		return listBucketNames(c, cmn.AIS)
	}

	bucket = strings.TrimSuffix(bucket, "/")
	return listBucketObj(c, bucket, cmn.AIS)
}

// Note: This handler ignores aisBucketEnvVar because the intention
// is to list cloud bucket names if bucket name isn't given.
func listCloudBucketsHandler(c *cli.Context) (err error) {
	bucket := c.Args().First()
	if bucket == "" {
		return listBucketNames(c, cmn.Cloud)
	}

	bucket = strings.TrimSuffix(bucket, "/")
	return listBucketObj(c, bucket, cmn.Cloud)
}

func listBckPropsHandler(c *cli.Context) (err error) {
	return listBucketProps(c)
}

func listConfigHandler(c *cli.Context) (err error) {
	if _, err = fillMap(); err != nil {
		return
	}
	return getDaemonConfig(c)
}

func listSmapHandler(c *cli.Context) (err error) {
	var (
		daemonID    = c.Args().First()
		primarySmap *cluster.Smap
	)

	if primarySmap, err = fillMap(); err != nil {
		return
	}

	return clusterSmap(c, primarySmap, daemonID, flagIsSet(c, jsonFlag))
}
