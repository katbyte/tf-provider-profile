// Package main implements tfpp, a CLI that profiles terraform provider releases over time: binary size and
// composition, startup and schema cost, source metrics, and (sampled) build and lint times.
package main

import (
	"os"

	c "github.com/gookit/color"
	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/tf-provider-profile/cli"
)

func main() {
	// the log level comes from TFPP_LOG; read it once here, before anything logs
	clog.SetLevelFromEnv("TFPP_LOG")

	cmd, err := cli.Make()
	if err != nil {
		clog.Log.Error(c.Sprintf("<red>tfpp: building cmd</> %v", err))

		os.Exit(1)
	}

	if err := cmd.Execute(); err != nil {
		clog.Log.Error(c.Sprintf("<red>tfpp:</> %v", err))

		os.Exit(1)
	}

	os.Exit(0)
}
