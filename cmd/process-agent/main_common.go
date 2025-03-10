// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/DataDog/agent-payload/v5/process"
	cmdconfig "github.com/DataDog/datadog-agent/cmd/agent/common/commands/config"
	"github.com/DataDog/datadog-agent/cmd/manager"
	"github.com/DataDog/datadog-agent/cmd/process-agent/api"
	"github.com/DataDog/datadog-agent/cmd/process-agent/app"
	sysconfig "github.com/DataDog/datadog-agent/cmd/system-probe/config"
	apiutil "github.com/DataDog/datadog-agent/pkg/api/util"
	ddconfig "github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/config/settings"
	settingshttp "github.com/DataDog/datadog-agent/pkg/config/settings/http"
	"github.com/DataDog/datadog-agent/pkg/metadata/host"
	"github.com/DataDog/datadog-agent/pkg/pidfile"
	"github.com/DataDog/datadog-agent/pkg/process/checks"
	"github.com/DataDog/datadog-agent/pkg/process/config"
	"github.com/DataDog/datadog-agent/pkg/process/statsd"
	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/tagger"
	"github.com/DataDog/datadog-agent/pkg/tagger/local"
	"github.com/DataDog/datadog-agent/pkg/tagger/remote"
	"github.com/DataDog/datadog-agent/pkg/telemetry"
	ddutil "github.com/DataDog/datadog-agent/pkg/util"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/DataDog/datadog-agent/pkg/util/profiling"
	"github.com/DataDog/datadog-agent/pkg/version"
	"github.com/DataDog/datadog-agent/pkg/workloadmeta"

	// register all workloadmeta collectors
	_ "github.com/DataDog/datadog-agent/pkg/workloadmeta/collectors"
)

const loggerName ddconfig.LoggerName = "PROCESS"

var opts struct {
	configPath         string
	sysProbeConfigPath string
	pidfilePath        string
	debug              bool
	version            bool
	check              string
	info               bool
}

var (
	rootCmd = &cobra.Command{
		Run:          rootCmdRun,
		SilenceUsage: true,
	}

	configCommand = cmdconfig.Config(getSettingsClient)
)

func getSettingsClient(_ *cobra.Command, _ []string) (settings.Client, error) {
	// Set up the config so we can get the port later
	// We set this up differently from the main process-agent because this way is quieter
	cfg := config.NewDefaultAgentConfig()
	if opts.configPath != "" {
		if err := config.LoadConfigIfExists(opts.configPath); err != nil {
			return nil, err
		}
	}
	err := cfg.LoadAgentConfig(opts.configPath)
	if err != nil {
		return nil, err
	}

	httpClient := apiutil.GetClient(false)
	ipcAddress, err := ddconfig.GetIPCAddress()

	port := ddconfig.Datadog.GetInt("process_config.cmd_port")
	if port <= 0 {
		return nil, fmt.Errorf("invalid process_config.cmd_port -- %d", port)
	}

	ipcAddressWithPort := fmt.Sprintf("http://%s:%d/config", ipcAddress, port)
	if err != nil {
		return nil, err
	}
	settingsClient := settingshttp.NewClient(httpClient, ipcAddressWithPort, "process-agent")
	return settingsClient, nil
}

func init() {
	rootCmd.AddCommand(configCommand, app.StatusCmd(), app.VersionCmd)
}

func deprecatedFlagWarning(deprecated, replaceWith string) string {
	return fmt.Sprintf("WARNING: `%s` argument is deprecated and will be removed in a future version. Please use `%s` instead.\n", deprecated, replaceWith)
}

// fixDeprecatedFlags modifies os.Args so that non-posix flags are converted to posix flags
// it also displays a warning when a non-posix flag is found
func fixDeprecatedFlags(args []string, w io.Writer) {
	deprecatedFlags := []string{
		// Global flags
		"-config", "--config", "-sysprobe-config", "-pid", "-info", "-version", "-check",
		// Windows flags
		"-install-service", "-uninstall-service", "-start-service", "-stop-service", "-foreground",
	}

	replaceFlags := map[string]string{
		"-config":  "--cfgpath",
		"--config": "--cfgpath",
	}

	for i, arg := range args {
		for _, f := range deprecatedFlags {
			if !strings.HasPrefix(arg, f) {
				continue
			}
			replaceWith := replaceFlags[f]
			if len(replaceWith) == 0 {
				replaceWith = "-" + f
				args[i] = "-" + args[i]
			} else {
				args[i] = strings.Replace(args[i], f, replaceWith, 1)
			}

			fmt.Fprint(w, deprecatedFlagWarning(f, replaceWith))
		}
	}
}

const (
	agent6DisabledMessage = `process-agent not enabled.
Set env var DD_PROCESS_CONFIG_PROCESS_COLLECTION_ENABLED=true or add
process_config:
  process_collection:
    enabled: true
to your datadog.yaml file.
Exiting.`
)

func runAgent(exit chan struct{}) {
	if opts.version {
		fmt.Println("WARNING: --version is deprecated and will be removed in a future version. Please use `process-agent version` instead.")
		_ = app.WriteVersion(color.Output)
		cleanupAndExit(0)
	}

	if err := ddutil.SetupCoreDump(); err != nil {
		log.Warnf("Can't setup core dumps: %v, core dumps might not be available after a crash", err)
	}

	if opts.check == "" && !opts.info && opts.pidfilePath != "" {
		err := pidfile.WritePID(opts.pidfilePath)
		if err != nil {
			log.Errorf("Error while writing PID file, exiting: %v", err)
			cleanupAndExit(1)
		}

		log.Infof("pid '%d' written to pid file '%s'", os.Getpid(), opts.pidfilePath)
		defer func() {
			// remove pidfile if set
			os.Remove(opts.pidfilePath)
		}()
	}

	// We need to load in the system probe environment variables before we load the config, otherwise an
	// "Unknown environment variable" warning will show up whenever valid system probe environment variables are defined.
	ddconfig.InitSystemProbeConfig(ddconfig.Datadog)

	// `GetContainers` will panic when running in docker if the config hasn't called `DetectFeatures`.
	// `LoadConfigIfExists` does the job of loading the config and calling `DetectFeatures` so that we can detect containers.
	if err := config.LoadConfigIfExists(opts.configPath); err != nil {
		_ = log.Criticalf("Error parsing config: %s", err)
		cleanupAndExit(1)
	}

	// For system probe, there is an additional config file that is shared with the system-probe
	syscfg, err := sysconfig.Merge(opts.sysProbeConfigPath)
	if err != nil {
		_ = log.Critical(err)
		cleanupAndExit(1)
	}

	config.InitRuntimeSettings()

	cfg, err := config.NewAgentConfig(loggerName, opts.configPath, syscfg)
	if err != nil {
		log.Criticalf("Error parsing config: %s", err)
		cleanupAndExit(1)
	}

	mainCtx, mainCancel := context.WithCancel(context.Background())
	defer mainCancel()
	err = manager.ConfigureAutoExit(mainCtx)
	if err != nil {
		log.Criticalf("Unable to configure auto-exit, err: %w", err)
		cleanupAndExit(1)
	}

	// Now that the logger is configured log host info
	hostInfo := host.GetStatusInformation()
	log.Infof("running on platform: %s", hostInfo.Platform)
	agentVersion, _ := version.Agent()
	log.Infof("running version: %s", agentVersion.GetNumberAndPre())

	// Start workload metadata store before tagger (used for containerCollection)
	store := workloadmeta.GetGlobalStore()
	store.Start(mainCtx)

	// Tagger must be initialized after agent config has been setup
	var t tagger.Tagger
	if ddconfig.Datadog.GetBool("process_config.remote_tagger") {
		t = remote.NewTagger()
	} else {
		t = local.NewTagger(store)
	}
	tagger.SetDefaultTagger(t)
	err = tagger.Init(mainCtx)
	if err != nil {
		log.Errorf("failed to start the tagger: %s", err)
	}
	defer tagger.Stop() //nolint:errcheck

	err = initInfo(cfg)
	if err != nil {
		log.Criticalf("Error initializing info: %s", err)
		cleanupAndExit(1)
	}
	if err := statsd.Configure(ddconfig.GetBindHost(), ddconfig.Datadog.GetInt("dogstatsd_port")); err != nil {
		log.Criticalf("Error configuring statsd: %s", err)
		cleanupAndExit(1)
	}

	enabledChecks := getChecks(syscfg, cfg.Orchestrator, ddconfig.IsAnyContainerFeaturePresent())

	// Exit if agent is not enabled and we're not debugging a check.
	if len(enabledChecks) == 0 && opts.check == "" {
		log.Infof(agent6DisabledMessage)

		// a sleep is necessary to ensure that supervisor registers this process as "STARTED"
		// If the exit is "too quick", we enter a BACKOFF->FATAL loop even though this is an expected exit
		// http://supervisord.org/subprocess.html#process-states
		time.Sleep(5 * time.Second)
		return
	}

	// update docker socket path in info
	dockerSock, err := util.GetDockerSocketPath()
	if err != nil {
		log.Debugf("Docker is not available on this host")
	}
	// we shouldn't quit because docker is not required. If no docker docket is available,
	// we just pass down empty string
	updateDockerSocket(dockerSock)

	// use `internal_profiling.enabled` field in `process_config` section to enable/disable profiling for process-agent,
	// but use the configuration from main agent to fill the settings
	if ddconfig.Datadog.GetBool("process_config.internal_profiling.enabled") {
		// allow full url override for development use
		site := ddconfig.Datadog.GetString("internal_profiling.profile_dd_url")
		if site == "" {
			s := ddconfig.Datadog.GetString("site")
			if s == "" {
				s = ddconfig.DefaultSite
			}
			site = fmt.Sprintf(profiling.ProfilingURLTemplate, s)
		}

		v, _ := version.Agent()
		profilingSettings := profiling.Settings{
			ProfilingURL:         site,
			Env:                  ddconfig.Datadog.GetString("env"),
			Service:              "process-agent",
			Period:               ddconfig.Datadog.GetDuration("internal_profiling.period"),
			CPUDuration:          ddconfig.Datadog.GetDuration("internal_profiling.cpu_duration"),
			MutexProfileFraction: ddconfig.Datadog.GetInt("internal_profiling.mutex_profile_fraction"),
			BlockProfileRate:     ddconfig.Datadog.GetInt("internal_profiling.block_profile_rate"),
			WithGoroutineProfile: ddconfig.Datadog.GetBool("internal_profiling.enable_goroutine_stacktraces"),
			Tags:                 []string{fmt.Sprintf("version:%v", v)},
		}

		if err := profiling.Start(profilingSettings); err != nil {
			log.Warnf("failed to enable profiling: %s", err)
		} else {
			log.Info("start profiling process-agent")
		}
		defer profiling.Stop()
	}

	log.Debug("Running process-agent with DEBUG logging enabled")
	if opts.check != "" {
		err := debugCheckResults(cfg, opts.check)
		if err != nil {
			fmt.Println(err)
			cleanupAndExit(1)
		} else {
			cleanupAndExit(0)
		}
		return
	}

	expVarPort := ddconfig.Datadog.GetInt("process_config.expvar_port")
	if expVarPort <= 0 {
		log.Warnf("Invalid process_config.expvar_port -- %d, using default port %d", expVarPort, ddconfig.DefaultProcessExpVarPort)
		expVarPort = ddconfig.DefaultProcessExpVarPort
	}

	if opts.info {
		// using the debug port to get info to work
		url := fmt.Sprintf("http://localhost:%d/debug/vars", expVarPort)
		if err := Info(os.Stdout, cfg, url); err != nil {
			cleanupAndExit(1)
		}
		return
	}

	// Run a profile & telemetry server.
	go func() {
		if ddconfig.Datadog.GetBool("telemetry.enabled") {
			http.Handle("/telemetry", telemetry.Handler())
		}
		err := http.ListenAndServe(fmt.Sprintf("localhost:%d", expVarPort), nil)
		if err != nil && err != http.ErrServerClosed {
			log.Errorf("Error creating expvar server on port %v: %v", expVarPort, err)
		}
	}()

	// Run API server
	err = api.StartServer()
	if err != nil {
		_ = log.Error(err)
	}

	cl, err := NewCollector(cfg, enabledChecks)
	if err != nil {
		log.Criticalf("Error creating collector: %s", err)
		cleanupAndExit(1)
		return
	}
	if err := cl.run(exit); err != nil {
		log.Criticalf("Error starting collector: %s", err)
		os.Exit(1)
		return
	}

	for range exit {
	}
}

func debugCheckResults(cfg *config.AgentConfig, check string) error {
	sysInfo, err := checks.CollectSystemInfo(cfg)
	if err != nil {
		return err
	}

	// Connections check requires process-check to have occurred first (for process creation ts),
	if check == checks.Connections.Name() {
		checks.Process.Init(cfg, sysInfo)
		checks.Process.Run(cfg, 0) //nolint:errcheck
	}

	names := make([]string, 0, len(checks.All))
	for _, ch := range checks.All {
		names = append(names, ch.Name())

		if ch.Name() == check {
			ch.Init(cfg, sysInfo)
			return runCheck(cfg, ch)
		}

		withRealTime, ok := ch.(checks.CheckWithRealTime)
		if ok && withRealTime.RealTimeName() == check {
			withRealTime.Init(cfg, sysInfo)
			return runCheckAsRealTime(cfg, withRealTime)
		}
	}
	return fmt.Errorf("invalid check '%s', choose from: %v", check, names)
}

func runCheck(cfg *config.AgentConfig, ch checks.Check) error {
	// Run the check once to prime the cache.
	if _, err := ch.Run(cfg, 0); err != nil {
		return fmt.Errorf("collection error: %s", err)
	}

	time.Sleep(1 * time.Second)

	printResultsBanner(ch.Name())

	msgs, err := ch.Run(cfg, 1)
	if err != nil {
		return fmt.Errorf("collection error: %s", err)
	}
	return printResults(msgs)
}

func runCheckAsRealTime(cfg *config.AgentConfig, ch checks.CheckWithRealTime) error {
	options := checks.RunOptions{
		RunStandard: true,
		RunRealTime: true,
	}
	var (
		groupID     int32
		nextGroupID = func() int32 {
			groupID++
			return groupID
		}
	)

	// We need to run the check twice in order to initialize the stats
	// Rate calculations rely on having two datapoints
	if _, err := ch.RunWithOptions(cfg, nextGroupID, options); err != nil {
		return fmt.Errorf("collection error: %s", err)
	}

	time.Sleep(1 * time.Second)

	printResultsBanner(ch.RealTimeName())

	run, err := ch.RunWithOptions(cfg, nextGroupID, options)
	if err != nil {
		return fmt.Errorf("collection error: %s", err)
	}

	return printResults(run.RealTime)
}

func printResultsBanner(name string) {
	fmt.Printf("-----------------------------\n\n")
	fmt.Printf("\nResults for check %s\n", name)
	fmt.Printf("-----------------------------\n\n")
}

func printResults(msgs []process.MessageBody) error {
	for _, m := range msgs {
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal error: %s", err)
		}
		fmt.Println(string(b))
	}
	return nil
}

// cleanupAndExit cleans all resources allocated by the agent before calling
// os.Exit
func cleanupAndExit(status int) {
	// remove pidfile if set
	if opts.pidfilePath != "" {
		if _, err := os.Stat(opts.pidfilePath); err == nil {
			os.Remove(opts.pidfilePath)
		}
	}

	os.Exit(status)
}
