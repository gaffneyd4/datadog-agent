// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build !windows
// +build !windows

package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/DataDog/datadog-agent/cmd/process-agent/flags"
)

func rootCmdRun(cmd *cobra.Command, args []string) {
	exit := make(chan struct{})

	// Invoke the Agent
	runAgent(exit)
}

func main() {
	rootCmd.PersistentFlags().StringVar(&opts.configPath, "cfgpath", flags.DefaultConfPath, "Path to datadog.yaml config")

	if flags.DefaultSysProbeConfPath != "" {
		rootCmd.PersistentFlags().StringVar(&opts.sysProbeConfigPath, "sysprobe-config", flags.DefaultSysProbeConfPath, "Path to system-probe.yaml config")
	}

	rootCmd.PersistentFlags().StringVarP(&opts.pidfilePath, "pid", "p", "", "Path to set pidfile for process")
	rootCmd.PersistentFlags().BoolVarP(&opts.info, "info", "i", false, "Show info about running process agent and exit")
	rootCmd.PersistentFlags().BoolVarP(&opts.version, "version", "v", false, "Print the version and exit")
	rootCmd.PersistentFlags().StringVar(&opts.check, "check", "",
		"Run a specific check and print the results. Choose from: process, connections, realtime, process_discovery")

	fixDeprecatedFlags(os.Args, os.Stdout)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(-1)
	}
}
