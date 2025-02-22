// Package main for the AIS node executable.
/*
 * Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
 */
package main

import (
	"os"

	"github.com/NVIDIA/aistore/ais"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
)

var (
	build     string
	buildtime string
)

func main() {
	debug.AssertMsg(build != "", "missing build")
	debug.AssertMsg(buildtime != "", "missing build time")
	ecode := ais.Run(cmn.VersionAIStore+"."+build, buildtime)
	os.Exit(ecode)
}
