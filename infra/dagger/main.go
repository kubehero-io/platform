// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Dagger pipeline entry point. Real implementation imports dagger.io/dagger
// and orchestrates build / test / image / sign for every component. This
// scaffold ships a CLI so CI can run the same pipeline locally.

package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	stage := flag.String("stage", "build", "build | test | image | sign | all")
	flag.Parse()

	fmt.Printf("dagger: running stage=%s (scaffold stub)\n", *stage)
	// TODO: wire dagger.io/dagger pipelines.
	os.Exit(0)
}
