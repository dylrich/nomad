// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/cli"
	checkpoint "github.com/hashicorp/go-checkpoint"
	discover "github.com/hashicorp/go-discover"
	hclog "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/hashicorp/go-metrics/compat/circonus"
	"github.com/hashicorp/go-metrics/compat/datadog"
	"github.com/hashicorp/go-metrics/compat/prometheus"
	gsyslog "github.com/hashicorp/go-syslog"
	"github.com/hashicorp/nomad/helper"
	flaghelper "github.com/hashicorp/nomad/helper/flags"
	gatedwriter "github.com/hashicorp/nomad/helper/gated-writer"
	"github.com/hashicorp/nomad/helper/logging"
	"github.com/hashicorp/nomad/helper/winsvc"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/version"
	"github.com/posener/complete"
)

// gracefulTimeout controls how long we wait before forcefully terminating
const gracefulTimeout = 5 * time.Second

// Command is a Command implementation that runs a Nomad agent.
// The command will not end unless a shutdown message is sent on the
// ShutdownCh. If two messages are sent on the ShutdownCh it will forcibly
// exit.
type Command struct {
	Version    *version.VersionInfo
	Ui         cli.Ui
	ShutdownCh <-chan struct{}

	args           []string
	agent          *Agent
	httpServers    []*HTTPServer
	retryJoinErrCh chan struct{}
}

func (c *Command) readConfig() *Config {
	var configPath []string
	var servers string
	var meta []string

	// Make a new, empty config.
	cmdConfig := &Config{
		Client:  &ClientConfig{},
		Consuls: []*config.ConsulConfig{{Name: structs.ConsulDefaultCluster}},
		Ports:   &Ports{},
		Server: &ServerConfig{
			ServerJoin: &ServerJoin{},
		},
		Vaults:    []*config.VaultConfig{{Name: structs.VaultDefaultCluster}},
		ACL:       &ACLConfig{},
		Audit:     &config.AuditConfig{},
		Reporting: &config.ReportingConfig{},
	}

	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	flags.Usage = func() { c.Ui.Error(c.Help()) }

	// Role options
	var devMode bool
	var devConnectMode bool
	var devConsulMode bool
	var devVaultMode bool
	flags.BoolVar(&devMode, "dev", false, "")
	flags.BoolVar(&devConnectMode, "dev-connect", false, "")
	flags.BoolVar(&devConsulMode, "dev-consul", false, "")
	flags.BoolVar(&devVaultMode, "dev-vault", false, "")
	flags.BoolVar(&cmdConfig.Server.Enabled, "server", false, "")
	flags.BoolVar(&cmdConfig.Client.Enabled, "client", false, "")

	// Server-only options
	flags.IntVar(&cmdConfig.Server.BootstrapExpect, "bootstrap-expect", 0, "")
	flags.StringVar(&cmdConfig.Server.EncryptKey, "encrypt", "", "gossip encryption key")
	flags.IntVar(&cmdConfig.Server.RaftProtocol, "raft-protocol", 0, "")
	flags.BoolVar(&cmdConfig.Server.RejoinAfterLeave, "rejoin", false, "")
	flags.Var((*flaghelper.StringFlag)(&cmdConfig.Server.ServerJoin.StartJoin), "join", "")
	flags.Var((*flaghelper.StringFlag)(&cmdConfig.Server.ServerJoin.RetryJoin), "retry-join", "")
	flags.IntVar(&cmdConfig.Server.ServerJoin.RetryMaxAttempts, "retry-max", 0, "")
	flags.Var((flaghelper.FuncDurationVar)(func(d time.Duration) error {
		cmdConfig.Server.ServerJoin.RetryInterval = d
		return nil
	}), "retry-interval", "")

	// Client-only options
	flags.StringVar(&cmdConfig.Client.StateDir, "state-dir", "", "")
	flags.StringVar(&cmdConfig.Client.AllocDir, "alloc-dir", "", "")
	flags.StringVar(&cmdConfig.Client.AllocMountsDir, "alloc-mounts-dir", "", "")
	flags.StringVar(&cmdConfig.Client.HostVolumesDir, "host-volumes-dir", "", "")
	flags.StringVar(&cmdConfig.Client.HostVolumePluginDir, "host-volume-plugin-dir", "", "")
	flags.StringVar(&cmdConfig.Client.NodeClass, "node-class", "", "")
	flags.StringVar(&cmdConfig.Client.NodePool, "node-pool", "", "")
	flags.StringVar(&servers, "servers", "", "")
	flags.Var((*flaghelper.StringFlag)(&meta), "meta", "")
	flags.StringVar(&cmdConfig.Client.NetworkInterface, "network-interface", "", "")
	flags.StringVar((*string)(&cmdConfig.Client.PreferredAddressFamily), "preferred-address-family", "", "ipv4 or ipv6")
	flags.IntVar(&cmdConfig.Client.NetworkSpeed, "network-speed", 0, "")

	// General options
	flags.Var((*flaghelper.StringFlag)(&configPath), "config", "config")
	flags.StringVar(&cmdConfig.BindAddr, "bind", "", "")
	flags.StringVar(&cmdConfig.Region, "region", "", "")
	flags.StringVar(&cmdConfig.DataDir, "data-dir", "", "")
	flags.StringVar(&cmdConfig.PluginDir, "plugin-dir", "", "")
	flags.StringVar(&cmdConfig.Datacenter, "dc", "", "")
	flags.StringVar(&cmdConfig.LogLevel, "log-level", "", "")
	flags.BoolVar(&cmdConfig.LogJson, "log-json", false, "")
	flags.BoolVar(&cmdConfig.LogIncludeLocation, "log-include-location", false, "")
	flags.StringVar(&cmdConfig.NodeName, "node", "", "")

	// Consul options
	defaultConsul := cmdConfig.defaultConsul()
	flags.StringVar(&defaultConsul.Auth, "consul-auth", "", "")
	flags.Var((flaghelper.FuncBoolVar)(func(b bool) error {
		defaultConsul.AutoAdvertise = &b
		return nil
	}), "consul-auto-advertise", "")
	flags.StringVar(&defaultConsul.CAFile, "consul-ca-file", "", "")
	flags.StringVar(&defaultConsul.CertFile, "consul-cert-file", "", "")
	flags.StringVar(&defaultConsul.KeyFile, "consul-key-file", "", "")
	flags.Var((flaghelper.FuncBoolVar)(func(b bool) error {
		defaultConsul.ChecksUseAdvertise = &b
		return nil
	}), "consul-checks-use-advertise", "")
	flags.Var((flaghelper.FuncBoolVar)(func(b bool) error {
		defaultConsul.ClientAutoJoin = &b
		return nil
	}), "consul-client-auto-join", "")
	flags.StringVar(&defaultConsul.ClientServiceName, "consul-client-service-name", "", "")
	flags.StringVar(&defaultConsul.ClientHTTPCheckName, "consul-client-http-check-name", "", "")
	flags.IntVar(&defaultConsul.ClientFailuresBeforeCritical, "consul-client-failures-before-critical", 0, "")
	flags.IntVar(&defaultConsul.ClientFailuresBeforeWarning, "consul-client-failures-before-warning", 0, "")
	flags.StringVar(&defaultConsul.ServerServiceName, "consul-server-service-name", "", "")
	flags.StringVar(&defaultConsul.ServerHTTPCheckName, "consul-server-http-check-name", "", "")
	flags.StringVar(&defaultConsul.ServerSerfCheckName, "consul-server-serf-check-name", "", "")
	flags.StringVar(&defaultConsul.ServerRPCCheckName, "consul-server-rpc-check-name", "", "")
	flags.IntVar(&defaultConsul.ServerFailuresBeforeCritical, "consul-server-failures-before-critical", 0, "")
	flags.IntVar(&defaultConsul.ServerFailuresBeforeWarning, "consul-server-failures-before-warning", 0, "")
	flags.Var((flaghelper.FuncBoolVar)(func(b bool) error {
		defaultConsul.ServerAutoJoin = &b
		return nil
	}), "consul-server-auto-join", "")
	flags.Var((flaghelper.FuncBoolVar)(func(b bool) error {
		defaultConsul.EnableSSL = &b
		return nil
	}), "consul-ssl", "")
	flags.StringVar(&defaultConsul.Token, "consul-token", "", "")
	flags.Var((flaghelper.FuncBoolVar)(func(b bool) error {
		defaultConsul.VerifySSL = &b
		return nil
	}), "consul-verify-ssl", "")
	flags.StringVar(&defaultConsul.Addr, "consul-address", "", "")

	// Vault options
	defaultVault := cmdConfig.defaultVault()
	flags.Var((flaghelper.FuncBoolVar)(func(b bool) error {
		defaultVault.Enabled = &b
		return nil
	}), "vault-enabled", "")
	flags.StringVar(&defaultVault.Addr, "vault-address", "", "")
	flags.StringVar(&defaultVault.Namespace, "vault-namespace", "", "")
	flags.StringVar(&defaultVault.Role, "vault-create-from-role", "", "")
	flags.StringVar(&defaultVault.TLSCaFile, "vault-ca-file", "", "")
	flags.StringVar(&defaultVault.TLSCaPath, "vault-ca-path", "", "")
	flags.StringVar(&defaultVault.TLSCertFile, "vault-cert-file", "", "")
	flags.StringVar(&defaultVault.TLSKeyFile, "vault-key-file", "", "")
	flags.Var((flaghelper.FuncBoolVar)(func(b bool) error {
		defaultVault.TLSSkipVerify = &b
		return nil
	}), "vault-tls-skip-verify", "")
	flags.StringVar(&defaultVault.TLSServerName, "vault-tls-server-name", "", "")

	// ACL options
	flags.BoolVar(&cmdConfig.ACL.Enabled, "acl-enabled", false, "")
	flags.StringVar(&cmdConfig.ACL.ReplicationToken, "acl-replication-token", "", "")

	if err := flags.Parse(c.args); err != nil {
		return nil
	}

	// Split the servers.
	if servers != "" {
		cmdConfig.Client.Servers = strings.Split(servers, ",")
	}

	// Parse the meta flags.
	metaLength := len(meta)
	if metaLength != 0 {
		cmdConfig.Client.Meta = make(map[string]string, metaLength)
		for _, kv := range meta {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				c.Ui.Error(fmt.Sprintf("Error parsing Client.Meta value: %v", kv))
				return nil
			}
			cmdConfig.Client.Meta[parts[0]] = parts[1]
		}
	}

	// Load the configuration
	var config *Config

	devConfig := &devModeConfig{
		defaultMode: devMode,
		connectMode: devConnectMode,
		consulMode:  devConsulMode,
		vaultMode:   devVaultMode,
	}
	if devConfig.enabled() {
		err := devConfig.validate()
		if err != nil {
			c.Ui.Error(err.Error())
			return nil
		}
		err = devConfig.networkConfig()
		if err != nil {
			c.Ui.Error(err.Error())
			return nil
		}
		config = DevConfig(devConfig)
	} else {
		config = DefaultConfig()
	}

	// Merge in the enterprise overlay
	config = config.Merge(DefaultEntConfig())

	for _, path := range configPath {
		current, err := LoadConfig(path)
		if err != nil {
			c.Ui.Error(fmt.Sprintf(
				"Error loading configuration from %s: %s", path, err))
			return nil
		}

		// The user asked us to load some config here but we didn't find any,
		// so we'll complain but continue.
		if current == nil || reflect.DeepEqual(current, &Config{}) {
			c.Ui.Warn(fmt.Sprintf("No configuration loaded from %s", path))
		}

		if config == nil {
			config = current
		} else {
			config = config.Merge(current)
		}
	}

	// Ensure the sub-structs at least exist
	if config.Client == nil {
		config.Client = &ClientConfig{}
	}

	if config.Server == nil {
		config.Server = &ServerConfig{}
	}

	// Merge any CLI options over config file options
	config = config.Merge(cmdConfig)

	// Set the version info
	config.Version = c.Version

	// Normalize binds, ports, addresses, and advertise
	if err := config.normalizeAddrs(); err != nil {
		c.Ui.Error(err.Error())
		return nil
	}

	// Read Vault configuration for the default cluster again after all
	// configuration sources have been merged.
	defaultVault = config.defaultVault()

	// Check to see if we should read the Vault namespace from the environment
	if defaultVault.Namespace == "" {
		defaultVault.Namespace = os.Getenv("VAULT_NAMESPACE")
	}

	// Default the plugin directory to be under that of the data directory if it
	// isn't explicitly specified.
	if config.PluginDir == "" && config.DataDir != "" {
		config.PluginDir = filepath.Join(config.DataDir, "plugins")
	}

	// License configuration options
	config.Server.LicenseEnv = os.Getenv("NOMAD_LICENSE")
	if config.Server.LicensePath == "" {
		config.Server.LicensePath = os.Getenv("NOMAD_LICENSE_PATH")
	}

	config.Server.DefaultSchedulerConfig.Canonicalize()

	if !c.IsValidConfig(config, cmdConfig) {
		return nil
	}

	return config
}

func (c *Command) IsValidConfig(config, cmdConfig *Config) bool {
	// Check that the server is running in at least one mode.
	if !(config.Server.Enabled || config.Client.Enabled) {
		c.Ui.Error("Must specify either server, client or dev mode for the agent.")
		return false
	}

	// Check that the region does not contain invalid characters
	if strings.ContainsAny(config.Region, "\000") {
		c.Ui.Error("Region contains invalid characters")
		return false
	}

	// Check that the datacenter name does not contain invalid characters
	if strings.ContainsAny(config.Datacenter, "\000*") {
		c.Ui.Error("Datacenter contains invalid characters (null or '*')")
		return false
	}

	if err := config.Telemetry.Validate(); err != nil {
		c.Ui.Error(fmt.Sprintf("telemetry block invalid: %v", err))
		return false
	}

	// Set up the TLS configuration properly if we have one.
	// XXX chelseakomlo: set up a TLSConfig New method which would wrap
	// constructor-type actions like this.
	if config.TLSConfig != nil && !config.TLSConfig.IsEmpty() {
		if err := config.TLSConfig.SetChecksum(); err != nil {
			c.Ui.Error(fmt.Sprintf("WARNING: Error when parsing TLS configuration: %v", err))
		}
	}
	if !config.DevMode && (config.TLSConfig == nil ||
		!config.TLSConfig.EnableHTTP || !config.TLSConfig.EnableRPC) {
		c.Ui.Error("WARNING: mTLS is not configured - Nomad is not secure without mTLS!")
	}

	if config.Server.EncryptKey != "" {
		if _, err := config.Server.EncryptBytes(); err != nil {
			c.Ui.Error(fmt.Sprintf("Invalid encryption key: %s", err))
			return false
		}
		keyfile := filepath.Join(config.DataDir, serfKeyring)
		if _, err := os.Stat(keyfile); err == nil {
			c.Ui.Warn("WARNING: keyring exists but -encrypt given, using keyring")
		}
	}

	// Verify the paths are absolute.
	dirs := map[string]string{
		"data-dir":               config.DataDir,
		"plugin-dir":             config.PluginDir,
		"alloc-dir":              config.Client.AllocDir,
		"alloc-mounts-dir":       config.Client.AllocMountsDir,
		"host-volumes-dir":       config.Client.HostVolumesDir,
		"host-volume-plugin-dir": config.Client.HostVolumePluginDir,
		"state-dir":              config.Client.StateDir,
	}
	for k, dir := range dirs {
		if dir == "" {
			continue
		}

		if !filepath.IsAbs(dir) {
			c.Ui.Error(fmt.Sprintf("%s must be given as an absolute path: got %v", k, dir))
			return false
		}
	}

	if config.Client.Enabled {
		for k := range config.Client.Meta {
			if !helper.IsValidInterpVariable(k) {
				c.Ui.Error(fmt.Sprintf("Invalid Client.Meta key: %v", k))
				return false
			}
		}
	}

	if err := config.Server.DefaultSchedulerConfig.Validate(); err != nil {
		c.Ui.Error(err.Error())
		return false
	}

	// Validate node pool name early to prevent agent from starting but the
	// client failing to register.
	if pool := config.Client.NodePool; pool != "" {
		if err := structs.ValidateNodePoolName(pool); err != nil {
			c.Ui.Error(fmt.Sprintf("Invalid node pool: %v", err))
			return false
		}
		if pool == structs.NodePoolAll {
			c.Ui.Error(fmt.Sprintf("Invalid node pool: node is not allowed to register in node pool %q", structs.NodePoolAll))
			return false
		}
	}

	for _, consul := range config.Consuls {
		if err := structs.ValidateConsulClusterName(consul.Name); err != nil {
			c.Ui.Error(fmt.Sprintf("Invalid Consul configuration: %v", err))
		}
	}
	for _, vault := range config.Vaults {
		if err := structs.ValidateVaultClusterName(vault.Name); err != nil {
			c.Ui.Error(fmt.Sprintf("Invalid Vault configuration: %v", err))
		}
	}

	for _, volumeConfig := range config.Client.HostVolumes {
		if volumeConfig.Path == "" {
			c.Ui.Error("Missing path in host_volume config")
			return false
		}
	}

	if config.Client.MinDynamicPort < 0 || config.Client.MinDynamicPort > structs.MaxValidPort {
		c.Ui.Error(fmt.Sprintf("Invalid dynamic port range: min_dynamic_port=%d", config.Client.MinDynamicPort))
		return false
	}
	if config.Client.MaxDynamicPort < 0 || config.Client.MaxDynamicPort > structs.MaxValidPort {
		c.Ui.Error(fmt.Sprintf("Invalid dynamic port range: max_dynamic_port=%d", config.Client.MaxDynamicPort))
		return false
	}
	if config.Client.MinDynamicPort > config.Client.MaxDynamicPort {
		c.Ui.Error(fmt.Sprintf("Invalid dynamic port range: min_dynamic_port=%d and max_dynamic_port=%d", config.Client.MinDynamicPort, config.Client.MaxDynamicPort))
		return false
	}

	if config.Client.Reserved == nil {
		// Coding error; should always be set by DefaultConfig()
		c.Ui.Error("client.reserved must be initialized. Please report a bug.")
		return false
	}

	if ports := config.Client.Reserved.ReservedPorts; ports != "" {
		if _, err := structs.ParsePortRanges(ports); err != nil {
			c.Ui.Error(fmt.Sprintf("reserved.reserved_ports %q invalid: %v", ports, err))
			return false
		}
	}

	for _, hn := range config.Client.HostNetworks {
		// Ensure port range is valid
		if _, err := structs.ParsePortRanges(hn.ReservedPorts); err != nil {
			c.Ui.Error(fmt.Sprintf("host_network[%q].reserved_ports %q invalid: %v",
				hn.Name, hn.ReservedPorts, err))
			return false
		}
	}

	if err := config.Client.Artifact.Validate(); err != nil {
		c.Ui.Error(fmt.Sprintf("client.artifact block invalid: %v", err))
		return false
	}

	if err := config.Client.PreferredAddressFamily.Validate(); err != nil {
		c.Ui.Error(fmt.Sprintf("Invalid preferred-address-family value: %s (valid values: %s, %s)",
			config.Client.PreferredAddressFamily,
			structs.NodeNetworkAF_IPv4, structs.NodeNetworkAF_IPv6),
		)
		return false
	}
	if err := config.RPC.Validate(); err != nil {
		c.Ui.Error(fmt.Sprintf("rpc block invalid: %v)", err))
		return false
	}

	if !config.DevMode {
		// Ensure that we have the directories we need to run.
		if config.Server.Enabled && config.DataDir == "" {
			c.Ui.Error(`Must specify "data_dir" config option or "data-dir" CLI flag`)
			return false
		}

		// The config is valid if the top-level data-dir is set or if both
		// alloc-dir and state-dir are set.
		if config.Client.Enabled && config.DataDir == "" {
			missing := config.Client.AllocDir == "" ||
				config.Client.AllocMountsDir == "" ||
				config.Client.StateDir == "" ||
				config.PluginDir == ""
			if missing {
				c.Ui.Error("Must specify the state, alloc-dir, alloc-mounts-dir and plugin-dir if data-dir is omitted.")
				return false
			}
		}

		// Check the bootstrap flags
		if !config.Server.Enabled && cmdConfig.Server.BootstrapExpect > 0 {
			// report an error if BootstrapExpect is set in CLI but server is disabled
			c.Ui.Error("Bootstrap requires server mode to be enabled")
			return false
		}
		if config.Server.Enabled && config.Server.BootstrapExpect == 1 {
			c.Ui.Error("WARNING: Bootstrap mode enabled! Potentially unsafe operation.")
		}
		if config.Server.Enabled && config.Server.BootstrapExpect%2 == 0 {
			c.Ui.Error("WARNING: Number of bootstrap servers should ideally be set to an odd number.")
		}

		// Check OIDC Issuer if set
		if config.Server.Enabled && config.Server.OIDCIssuer != "" {
			issuerURL, err := url.Parse(config.Server.OIDCIssuer)
			if err != nil {
				c.Ui.Error(fmt.Sprintf(`Error using server.oidc_issuer = "%s" as a base URL: %s`, config.Server.OIDCIssuer, err))
				return false
			}

			if issuerURL.Scheme != "https" {
				c.Ui.Warn(fmt.Sprintf(`server.oidc_issuer = "%s" is not using https. Many OIDC implementations require https.`, config.Server.OIDCIssuer))
			}
		}
	}

	// ProtocolVersion has never been used. Warn if it is set as someone
	// has probably made a mistake.
	if config.Server.ProtocolVersion != 0 {
		c.Ui.Warn("Please remove deprecated protocol_version field from config.")
	}

	return true
}

// SetupLoggers is used to set up the logGate and our logOutput.
//
// The function needs to be public due to the way it is used within the Nomad
// Enterprise codebase.
func SetupLoggers(ui cli.Ui, config *Config) (*gatedwriter.Writer, io.Writer) {

	// Pull the log level from the configuration, ensure it is titled and then
	// perform validation. Do this before the gated writer, as this can
	// generate an error, whereas the writer does not.
	logLevel := strings.ToUpper(config.LogLevel)

	if !isLogLevelValid(logLevel) {
		ui.Error(fmt.Sprintf(
			"Invalid log level: %s. Valid log levels are: %v",
			logLevel, validLogLevels.Slice()))
		return nil, nil
	}

	// Create a gated log writer, which will store logs until we're ready to
	// output them.
	logGate := &gatedwriter.Writer{
		Writer: &cli.UiWriter{Ui: ui},
	}

	// Initialize our array of log writers with the gated writer. Additional
	// log writers will be appended if/when configured.
	writers := []io.Writer{logGate}

	if logLevel == "OFF" {
		config.EnableSyslog = false
	}
	// Check if syslog is enabled
	if config.EnableSyslog {
		ui.Output(fmt.Sprintf("Config enable_syslog is `true` with log_level=%v", config.LogLevel))
		l, err := gsyslog.NewLogger(getSysLogPriority(logLevel), config.SyslogFacility, "nomad")
		if err != nil {
			ui.Error(fmt.Sprintf("Syslog setup failed: %v", err))
			return nil, nil
		}
		writers = append(writers, newSyslogWriter(l, config.LogJson))
	}

	// Check if file logging is enabled
	if config.LogFile != "" {
		dir, fileName := filepath.Split(config.LogFile)

		// if a path is provided, but has no filename, then a default is used.
		if fileName == "" {
			fileName = "nomad.log"
		}

		// Try to enter the user specified log rotation duration first
		var logRotateDuration time.Duration
		if config.LogRotateDuration != "" {
			duration, err := time.ParseDuration(config.LogRotateDuration)
			if err != nil {
				ui.Error(fmt.Sprintf("Failed to parse log rotation duration: %v", err))
				return nil, nil
			}
			logRotateDuration = duration
		} else {
			// Default to 24 hrs if no rotation period is specified
			logRotateDuration = 24 * time.Hour
		}

		logFile := &logFile{
			fileName: fileName,
			logPath:  dir,
			duration: logRotateDuration,
			MaxBytes: config.LogRotateBytes,
			MaxFiles: config.LogRotateMaxFiles,
		}

		writers = append(writers, logFile)
	}

	logOutput := io.MultiWriter(writers...)
	return logGate, logOutput
}

// setupAgent is used to start the agent and various interfaces
func (c *Command) setupAgent(config *Config, logger hclog.InterceptLogger, logOutput io.Writer, inmem *metrics.InmemSink) error {
	c.Ui.Output("Starting Nomad agent...")

	agent, err := NewAgent(config, logger, logOutput, inmem)
	if err != nil {
		// log the error as well, so it appears at the end
		logger.Error("error starting agent", "error", err)
		c.Ui.Error(fmt.Sprintf("Error starting agent: %s", err))
		return err
	}
	c.agent = agent

	// Setup the HTTP server
	httpServers, err := NewHTTPServers(agent, config)
	if err != nil {
		agent.Shutdown()
		c.Ui.Error(fmt.Sprintf("Error starting http server: %s", err))
		return err
	}
	c.httpServers = httpServers

	// If DisableUpdateCheck is not enabled, set up update checking
	// (DisableUpdateCheck is false by default)
	if config.DisableUpdateCheck != nil && !*config.DisableUpdateCheck {
		version := config.Version.Version
		if config.Version.VersionPrerelease != "" {
			version += fmt.Sprintf("-%s", config.Version.VersionPrerelease)
		}
		updateParams := &checkpoint.CheckParams{
			Product: "nomad",
			Version: version,
		}
		if !config.DisableAnonymousSignature {
			updateParams.SignatureFile = filepath.Join(config.DataDir, "checkpoint-signature")
		}

		// Schedule a periodic check with expected interval of 24 hours
		checkpoint.CheckInterval(updateParams, 24*time.Hour, c.checkpointResults)

		// Do an immediate check within the next 30 seconds
		go func() {
			time.Sleep(helper.RandomStagger(30 * time.Second))
			c.checkpointResults(checkpoint.Check(updateParams))
		}()
	}

	return nil
}

// checkpointResults is used to handler periodic results from our update checker
func (c *Command) checkpointResults(results *checkpoint.CheckResponse, err error) {
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to check for updates: %v", err))
		return
	}
	if results.Outdated {
		c.Ui.Error(fmt.Sprintf("Newer Nomad version available: %s (currently running: %s)", results.CurrentVersion, c.Version.VersionNumber()))
	}
	for _, alert := range results.Alerts {
		switch alert.Level {
		case "info":
			c.Ui.Info(fmt.Sprintf("Bulletin [%s]: %s (%s)", alert.Level, alert.Message, alert.URL))
		default:
			c.Ui.Error(fmt.Sprintf("Bulletin [%s]: %s (%s)", alert.Level, alert.Message, alert.URL))
		}
	}
}

func (c *Command) AutocompleteFlags() complete.Flags {
	configFilePredictor := complete.PredictOr(
		complete.PredictFiles("*.json"),
		complete.PredictFiles("*.hcl"))

	return map[string]complete.Predictor{
		"-dev":                         complete.PredictNothing,
		"-dev-connect":                 complete.PredictNothing,
		"-server":                      complete.PredictNothing,
		"-client":                      complete.PredictNothing,
		"-bootstrap-expect":            complete.PredictAnything,
		"-encrypt":                     complete.PredictAnything,
		"-raft-protocol":               complete.PredictAnything,
		"-rejoin":                      complete.PredictNothing,
		"-join":                        complete.PredictAnything,
		"-retry-join":                  complete.PredictAnything,
		"-retry-max":                   complete.PredictAnything,
		"-state-dir":                   complete.PredictDirs("*"),
		"-alloc-dir":                   complete.PredictDirs("*"),
		"-node-class":                  complete.PredictAnything,
		"-node-pool":                   complete.PredictAnything,
		"-servers":                     complete.PredictAnything,
		"-meta":                        complete.PredictAnything,
		"-config":                      configFilePredictor,
		"-bind":                        complete.PredictAnything,
		"-region":                      complete.PredictAnything,
		"-data-dir":                    complete.PredictDirs("*"),
		"-plugin-dir":                  complete.PredictDirs("*"),
		"-host-volume-plugin-dir":      complete.PredictDirs("*"),
		"-dc":                          complete.PredictAnything,
		"-log-level":                   complete.PredictAnything,
		"-json-logs":                   complete.PredictNothing,
		"-node":                        complete.PredictAnything,
		"-consul-auth":                 complete.PredictAnything,
		"-consul-auto-advertise":       complete.PredictNothing,
		"-consul-ca-file":              complete.PredictAnything,
		"-consul-cert-file":            complete.PredictAnything,
		"-consul-key-file":             complete.PredictAnything,
		"-consul-checks-use-advertise": complete.PredictNothing,
		"-consul-client-auto-join":     complete.PredictNothing,
		"-consul-client-service-name":  complete.PredictAnything,
		"-consul-client-failures-before-critical": complete.PredictAnything,
		"-consul-client-failures-before-warning":  complete.PredictAnything,
		"-consul-client-http-check-name":          complete.PredictAnything,
		"-consul-server-service-name":             complete.PredictAnything,
		"-consul-server-http-check-name":          complete.PredictAnything,
		"-consul-server-serf-check-name":          complete.PredictAnything,
		"-consul-server-rpc-check-name":           complete.PredictAnything,
		"-consul-server-auto-join":                complete.PredictNothing,
		"-consul-server-failures-before-critical": complete.PredictAnything,
		"-consul-server-failures-before-warning":  complete.PredictAnything,
		"-consul-ssl":                             complete.PredictNothing,
		"-consul-verify-ssl":                      complete.PredictNothing,
		"-consul-address":                         complete.PredictAnything,
		"-consul-token":                           complete.PredictAnything,
		"-vault-enabled":                          complete.PredictNothing,
		"-vault-allow-unauthenticated":            complete.PredictNothing,
		"-vault-token":                            complete.PredictAnything,
		"-vault-address":                          complete.PredictAnything,
		"-vault-create-from-role":                 complete.PredictAnything,
		"-vault-ca-file":                          complete.PredictAnything,
		"-vault-ca-path":                          complete.PredictAnything,
		"-vault-cert-file":                        complete.PredictAnything,
		"-vault-key-file":                         complete.PredictAnything,
		"-vault-tls-skip-verify":                  complete.PredictNothing,
		"-vault-tls-server-name":                  complete.PredictAnything,
		"-acl-enabled":                            complete.PredictNothing,
		"-acl-replication-token":                  complete.PredictAnything,
	}
}

func (c *Command) AutocompleteArgs() complete.Predictor {
	return nil
}

func (c *Command) Run(args []string) int {
	c.Ui = &cli.PrefixedUi{
		OutputPrefix: "==> ",
		InfoPrefix:   "    ",
		ErrorPrefix:  "==> ",
		Ui:           c.Ui,
	}

	// Parse our configs
	c.args = args
	config := c.readConfig()
	if config == nil {
		return 1
	}

	// reset UI to prevent prefixed json output
	if config.LogJson {
		c.Ui = &cli.BasicUi{
			Reader:      os.Stdin,
			Writer:      os.Stdout,
			ErrorWriter: os.Stderr,
		}
	}

	// Set up the log outputs.
	logGate, logOutput := SetupLoggers(c.Ui, config)
	if logGate == nil {
		return 1
	}

	// Create logger
	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{
		Name:            "agent",
		Level:           hclog.LevelFromString(config.LogLevel),
		Output:          logOutput,
		JSONFormat:      config.LogJson,
		IncludeLocation: config.LogIncludeLocation,
	})

	// Wrap log messages emitted with the 'log' package.
	// These usually come from external dependencies.
	log.SetOutput(logger.StandardWriter(&hclog.StandardLoggerOptions{
		InferLevels:              true,
		InferLevelsWithTimestamp: true,
	}))
	log.SetPrefix("")
	log.SetFlags(0)

	// Swap out UI implementation if json logging is enabled
	if config.LogJson {
		c.Ui = &logging.HcLogUI{Log: logger}
		// Don't buffer json logs because they aren't reordered anyway.
		logGate.Flush()
	}

	// Log config files
	if len(config.Files) > 0 {
		c.Ui.Output(fmt.Sprintf("Loaded configuration from %s", strings.Join(config.Files, ", ")))
	} else {
		c.Ui.Output("No configuration files loaded")
	}

	// Initialize the telemetry
	inmem, err := c.setupTelemetry(config)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing telemetry: %s", err))
		return 1
	}

	// Create the agent
	if err := c.setupAgent(config, logger, logOutput, inmem); err != nil {
		logGate.Flush()
		return 1
	}

	defer func() {
		c.agent.Shutdown()

		// Shutdown the http server at the end, to ease debugging if
		// the agent takes long to shutdown
		if len(c.httpServers) > 0 {
			for _, srv := range c.httpServers {
				srv.Shutdown()
			}
		}
	}()

	// Join startup nodes if specified
	if err := c.startupJoin(config); err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	// Compile agent information for output later
	info := make(map[string]string)
	info["version"] = config.Version.VersionNumber()
	info["client"] = strconv.FormatBool(config.Client.Enabled)
	info["log level"] = config.LogLevel
	info["server"] = strconv.FormatBool(config.Server.Enabled)
	info["region"] = fmt.Sprintf("%s (DC: %s)", config.Region, config.Datacenter)
	info["bind addrs"] = c.getBindAddrSynopsis()
	info["advertise addrs"] = c.getAdvertiseAddrSynopsis()
	if config.Server.Enabled {
		serverConfig, err := c.agent.serverConfig()
		if err == nil {
			info["node id"] = serverConfig.NodeID
		}
	}

	// Sort the keys for output
	infoKeys := make([]string, 0, len(info))
	for key := range info {
		infoKeys = append(infoKeys, key)
	}
	sort.Strings(infoKeys)

	// Agent configuration output
	padding := 18
	c.Ui.Output("Nomad agent configuration:\n")
	for _, k := range infoKeys {
		c.Ui.Info(fmt.Sprintf(
			"%s%s: %s",
			strings.Repeat(" ", padding-len(k)),
			strings.Title(k),
			info[k]))
	}
	c.Ui.Output("")

	// Output the header that the server has started
	c.Ui.Output("Nomad agent started! Log data will stream in below:\n")

	// Enable log streaming
	logGate.Flush()

	// Start retry join process
	if err := c.handleRetryJoin(config); err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	// Wait for exit
	return c.handleSignals()
}

// handleRetryJoin is used to start retry joining if it is configured.
func (c *Command) handleRetryJoin(config *Config) error {
	c.retryJoinErrCh = make(chan struct{})

	if config.Server.Enabled && len(config.Server.RetryJoin) != 0 {

		joiner := retryJoiner{}

		if err := joiner.Validate(config); err != nil {
			return err
		}

		// Remove the duplicate fields
		if len(config.Server.RetryJoin) != 0 {
			config.Server.ServerJoin.RetryJoin = config.Server.RetryJoin
			config.Server.RetryJoin = nil
		}
		if config.Server.RetryMaxAttempts != 0 {
			config.Server.ServerJoin.RetryMaxAttempts = config.Server.RetryMaxAttempts
			config.Server.RetryMaxAttempts = 0
		}
		if config.Server.RetryInterval != 0 {
			config.Server.ServerJoin.RetryInterval = config.Server.RetryInterval
			config.Server.RetryInterval = 0
		}

		c.agent.logger.Warn("using deprecated retry_join fields. Upgrade configuration to use server_join")
	}

	if config.Server.Enabled &&
		config.Server.ServerJoin != nil &&
		len(config.Server.ServerJoin.RetryJoin) != 0 {

		joiner := retryJoiner{
			autoDiscover: autoDiscover{goDiscover: &discover.Discover{}, netAddrs: &netAddrs{}},
			errCh:        c.retryJoinErrCh,
			joinCfg:      config.Server.ServerJoin,
			joinFunc:     c.agent.server.Join,
			logger:       c.agent.logger.Named("joiner").With("agent_mode", "server"),
		}

		if err := joiner.Validate(config); err != nil {
			return err
		}

		go joiner.RetryJoin()
	}

	if config.Client.Enabled &&
		config.Client.ServerJoin != nil &&
		len(config.Client.ServerJoin.RetryJoin) != 0 {
		joiner := retryJoiner{
			autoDiscover: autoDiscover{goDiscover: &discover.Discover{}, netAddrs: &netAddrs{}},
			errCh:        c.retryJoinErrCh,
			joinCfg:      config.Client.ServerJoin,
			joinFunc:     c.agent.client.SetServers,
			logger:       c.agent.logger.Named("joiner").With("agent_mode", "client"),
		}

		if err := joiner.Validate(config); err != nil {
			return err
		}

		go joiner.RetryJoin()
	}

	return nil
}

// terminateGracefully attempts a graceful leave
func (c *Command) terminateGracefully(signalCh chan os.Signal, sdSock io.Writer) int {
	sdNotify(sdSock, sdStopping)

	gracefulCh := make(chan struct{})
	gracefulClose := sync.OnceFunc(func() { close(gracefulCh) })
	defer gracefulClose()

	timeout := gracefulTimeout

	if c.agent.client != nil {
		config := c.agent.client.GetConfig()

		if config == nil {
			c.Ui.Output("Unable to read the agent configuration, using the default graceful timeout")
		} else if config.Drain != nil && config.Drain.Deadline != 0 {
			timeout += config.Drain.Deadline
		}
	}

	c.Ui.Output("Gracefully shutting down agent...")
	go func() {
		if err := c.agent.Leave(); err != nil {
			c.Ui.Error(fmt.Sprintf("Error: %s", err))
			return
		}
		gracefulClose()
	}()

	delay := time.NewTimer(timeout)

	// Wait for leave or another signal to be received
	for {
		select {
		case sig := <-signalCh:
			// If a SIGPIPE is received, ignore it and
			// continue waiting
			if sig == syscall.SIGPIPE {
				c.agent.logger.Trace("caught SIGPIPE during graceful shutdown, ignoring")
				continue
			}
			c.agent.logger.Trace("caught signal during graceful shutdown", "signal", sig)

			return 1
		case <-delay.C:
			return 1
		case <-gracefulCh:
		}

		break
	}

	return 0
}

// handleSignals blocks until we get an exit-causing signal
func (c *Command) handleSignals() int {
	signalCh := make(chan os.Signal, 4)
	defer signal.Stop(signalCh)

	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)

	// Signal readiness only once signal handlers are setup
	sdSock, err := openNotify()
	if err != nil {
		c.agent.logger.Debug("notify socket could not be accessed", "error", err)
	}

	if sdSock != nil {
		defer sdSock.Close()
	}
	sdNotify(sdSock, sdReady)

	for {
		select {
		case sig := <-signalCh:
			c.Ui.Output(fmt.Sprintf("Caught signal: %v", sig))

			switch sig {
			case syscall.SIGPIPE:
				// Skip any SIGPIPE signal (see issues #1798, #3554)
				continue
			case syscall.SIGHUP:
				sdNotifyReloading(sdSock)
				err := c.handleReload()
				if err != nil {
					c.Ui.Error(fmt.Sprintf("Fatal error while reloading: %v", err))
					return 1
				}

				sdNotify(sdSock, sdReady)
			case syscall.SIGTERM:
				if !c.agent.GetConfig().LeaveOnTerm {
					return 1
				}

				return c.terminateGracefully(signalCh, sdSock)
			case os.Interrupt:
				if !c.agent.GetConfig().LeaveOnInt {
					return 1
				}

				return c.terminateGracefully(signalCh, sdSock)
			}

		case <-winsvc.ShutdownChannel():
			if !c.agent.GetConfig().LeaveOnInt {
				return 1
			}

			return c.terminateGracefully(signalCh, sdSock)

		case <-c.ShutdownCh:
			if !c.agent.GetConfig().LeaveOnInt {
				return 1
			}

			return c.terminateGracefully(signalCh, sdSock)

		case <-c.retryJoinErrCh:
			return 1
		}
	}
}

// reloadHTTPServer shuts down the existing HTTP server and restarts it. This
// is helpful when reloading the agent configuration.
func (c *Command) reloadHTTPServer() error {
	c.agent.logger.Info("reloading HTTP server with new TLS configuration")

	for _, srv := range c.httpServers {
		srv.Shutdown()
	}

	httpServers, err := NewHTTPServers(c.agent, c.agent.config)
	if err != nil {
		return err
	}
	c.httpServers = httpServers

	return nil
}

// handleReload is invoked when we should reload our configs, e.g. SIGHUP
// It will only return an error if the reload encountered a fatal error that must
// cause an agent termination.
func (c *Command) handleReload() error {
	c.Ui.Output("Reloading configuration...")
	newConf := c.readConfig()
	if newConf == nil {
		c.Ui.Error("Failed to reload configs")
		return nil
	}

	// Change the log level
	minLevel := strings.ToUpper(newConf.LogLevel)

	if !isLogLevelValid(minLevel) {
		c.Ui.Error(fmt.Sprintf(
			"Invalid log level: %s. Valid log levels are: %v",
			minLevel, validLogLevels.Slice()))

		// Keep the current log level
		newConf.LogLevel = c.agent.GetConfig().LogLevel
	}

	shouldReloadAgent, shouldReloadHTTP := c.agent.ShouldReload(newConf)
	if shouldReloadAgent {
		c.agent.logger.Debug("starting reload of agent config")
		err := c.agent.Reload(newConf)
		if err != nil {
			c.agent.logger.Error("failed to reload the config", "error", err)
			return nil
		}
	}

	if s := c.agent.Server(); s != nil {
		c.agent.logger.Debug("starting reload of server config")
		sconf, err := convertServerConfig(newConf)
		if err != nil {
			c.agent.logger.Error("failed to convert server config", "error", err)
			return nil
		}

		// Finalize the config to get the agent objects injected in
		c.agent.finalizeServerConfig(sconf)

		// Reload the config
		if err := s.Reload(sconf); err != nil {
			c.agent.logger.Error("reloading server config failed", "error", err)
			return fmt.Errorf("reloading server config failed: %w", err)
		}
	}

	if client := c.agent.Client(); client != nil {
		c.agent.logger.Debug("starting reload of client config")
		clientConfig, err := convertClientConfig(newConf)
		if err != nil {
			c.agent.logger.Error("failed to convert client config", "error", err)
			return nil
		}

		// Finalize the config to get the agent objects injected in
		if err := c.agent.finalizeClientConfig(clientConfig); err != nil {
			c.agent.logger.Error("failed to finalize client config", "error", err)
			return nil
		}

		if err := client.Reload(clientConfig); err != nil {
			c.agent.logger.Error("reloading client config failed", "error", err)
			return fmt.Errorf("reloading client config failed: %w", err)
		}
	}

	// reload HTTP server after we have reloaded both client and server, in case
	// we error in either of the above cases. For example, reloading the http
	// server to a TLS connection could succeed, while reloading the server's rpc
	// connections could fail.
	if shouldReloadHTTP {
		err := c.reloadHTTPServer()
		if err != nil {
			c.agent.httpLogger.Error("reloading config failed", "error", err)
		}
	}
	return nil
}

// setupTelemetry is used to set up the telemetry sub-systems.
func (c *Command) setupTelemetry(config *Config) (*metrics.InmemSink, error) {

	var telConfig *Telemetry
	if config.Telemetry == nil {
		telConfig = &Telemetry{}
	} else {
		telConfig = config.Telemetry
	}

	inm := metrics.NewInmemSink(telConfig.inMemoryCollectionInterval, telConfig.inMemoryRetentionPeriod)
	metrics.DefaultInmemSignal(inm)

	metricsConf := metrics.DefaultConfig("nomad")
	metricsConf.EnableHostname = !telConfig.DisableHostname

	// Prefer the hostname as a label.
	metricsConf.EnableHostnameLabel = !telConfig.DisableHostname

	if telConfig.UseNodeName {
		metricsConf.HostName = config.NodeName
		metricsConf.EnableHostname = true
	}

	allowedPrefixes, blockedPrefixes, err := telConfig.PrefixFilters()
	if err != nil {
		return inm, err
	}

	metricsConf.AllowedPrefixes = allowedPrefixes
	metricsConf.BlockedPrefixes = blockedPrefixes

	if telConfig.FilterDefault != nil {
		metricsConf.FilterDefault = *telConfig.FilterDefault
	}

	// Configure the statsite sink
	var fanout metrics.FanoutSink
	if telConfig.StatsiteAddr != "" {
		sink, err := metrics.NewStatsiteSink(telConfig.StatsiteAddr)
		if err != nil {
			return inm, err
		}
		fanout = append(fanout, sink)
	}

	// Configure the statsd sink
	if telConfig.StatsdAddr != "" {
		sink, err := metrics.NewStatsdSink(telConfig.StatsdAddr)
		if err != nil {
			return inm, err
		}
		fanout = append(fanout, sink)
	}

	// Configure the prometheus sink
	if telConfig.PrometheusMetrics {
		promSink, err := prometheus.NewPrometheusSink()
		if err != nil {
			return inm, err
		}
		fanout = append(fanout, promSink)
	}

	// Configure the datadog sink
	if telConfig.DataDogAddr != "" {
		sink, err := datadog.NewDogStatsdSink(telConfig.DataDogAddr, config.NodeName)
		if err != nil {
			return inm, err
		}
		sink.SetTags(telConfig.DataDogTags)
		fanout = append(fanout, sink)
	}

	// Configure the Circonus sink
	if telConfig.CirconusAPIToken != "" || telConfig.CirconusCheckSubmissionURL != "" {
		cfg := &circonus.Config{}
		cfg.Interval = telConfig.CirconusSubmissionInterval
		cfg.CheckManager.API.TokenKey = telConfig.CirconusAPIToken
		cfg.CheckManager.API.TokenApp = telConfig.CirconusAPIApp
		cfg.CheckManager.API.URL = telConfig.CirconusAPIURL
		cfg.CheckManager.Check.SubmissionURL = telConfig.CirconusCheckSubmissionURL
		cfg.CheckManager.Check.ID = telConfig.CirconusCheckID
		cfg.CheckManager.Check.ForceMetricActivation = telConfig.CirconusCheckForceMetricActivation
		cfg.CheckManager.Check.InstanceID = telConfig.CirconusCheckInstanceID
		cfg.CheckManager.Check.SearchTag = telConfig.CirconusCheckSearchTag
		cfg.CheckManager.Check.Tags = telConfig.CirconusCheckTags
		cfg.CheckManager.Check.DisplayName = telConfig.CirconusCheckDisplayName
		cfg.CheckManager.Broker.ID = telConfig.CirconusBrokerID
		cfg.CheckManager.Broker.SelectTag = telConfig.CirconusBrokerSelectTag

		if cfg.CheckManager.Check.DisplayName == "" {
			cfg.CheckManager.Check.DisplayName = "Nomad"
		}

		if cfg.CheckManager.API.TokenApp == "" {
			cfg.CheckManager.API.TokenApp = "nomad"
		}

		if cfg.CheckManager.Check.SearchTag == "" {
			cfg.CheckManager.Check.SearchTag = "service:nomad"
		}

		sink, err := circonus.NewCirconusSink(cfg)
		if err != nil {
			return inm, err
		}
		sink.Start()
		fanout = append(fanout, sink)
	}

	// Initialize the global sink
	if len(fanout) > 0 {
		fanout = append(fanout, inm)
		metrics.NewGlobal(metricsConf, fanout)
	} else {
		metricsConf.EnableHostname = false
		metrics.NewGlobal(metricsConf, inm)
	}

	return inm, nil
}

func (c *Command) startupJoin(config *Config) error {
	// Nothing to do
	if !config.Server.Enabled {
		return nil
	}

	// Validate both old and new aren't being set
	old := len(config.Server.StartJoin)
	var new int
	if config.Server.ServerJoin != nil {
		new = len(config.Server.ServerJoin.StartJoin)
	}
	if old != 0 && new != 0 {
		return fmt.Errorf("server_join and start_join cannot both be defined; prefer setting the server_join block")
	}

	// Nothing to do
	if old+new == 0 {
		return nil
	}

	// Combine the lists and join
	joining := config.Server.StartJoin
	if new != 0 {
		joining = append(joining, config.Server.ServerJoin.StartJoin...)
	}

	c.Ui.Output("Joining cluster...")
	n, err := c.agent.server.Join(joining)
	if err != nil {
		return err
	}

	c.Ui.Output(fmt.Sprintf("Join completed. Synced with %d initial agents", n))
	return nil
}

// getBindAddrSynopsis returns a string that describes the addresses the agent
// is bound to.
func (c *Command) getBindAddrSynopsis() string {
	if c == nil || c.agent == nil || c.agent.config == nil || c.agent.config.normalizedAddrs == nil {
		return ""
	}

	b := new(strings.Builder)
	fmt.Fprintf(b, "HTTP: %s", c.agent.config.normalizedAddrs.HTTP)

	if c.agent.server != nil {
		if c.agent.config.normalizedAddrs.RPC != "" {
			fmt.Fprintf(b, "; RPC: %s", c.agent.config.normalizedAddrs.RPC)
		}
		if c.agent.config.normalizedAddrs.Serf != "" {
			fmt.Fprintf(b, "; Serf: %s", c.agent.config.normalizedAddrs.Serf)
		}
	}

	return b.String()
}

// getAdvertiseAddrSynopsis returns a string that describes the addresses the agent
// is advertising.
func (c *Command) getAdvertiseAddrSynopsis() string {
	if c == nil || c.agent == nil || c.agent.config == nil || c.agent.config.AdvertiseAddrs == nil {
		return ""
	}

	b := new(strings.Builder)
	fmt.Fprintf(b, "HTTP: %s", c.agent.config.AdvertiseAddrs.HTTP)

	if c.agent.server != nil {
		if c.agent.config.AdvertiseAddrs.RPC != "" {
			fmt.Fprintf(b, "; RPC: %s", c.agent.config.AdvertiseAddrs.RPC)
		}
		if c.agent.config.AdvertiseAddrs.Serf != "" {
			fmt.Fprintf(b, "; Serf: %s", c.agent.config.AdvertiseAddrs.Serf)
		}
	}

	return b.String()
}

func (c *Command) Synopsis() string {
	return "Runs a Nomad agent"
}

func (c *Command) Help() string {
	helpText := `
Usage: nomad agent [options]

  Starts the Nomad agent and runs until an interrupt is received.
  The agent may be a client and/or server.

  The Nomad agent's configuration primarily comes from the config
  files used, but a subset of the options may also be passed directly
  as CLI arguments, listed below.

General Options (clients and servers):

  -bind=<addr>
    The address the agent will bind to for all of its various network
    services. The individual services that run bind to individual
    ports on this address. Defaults to the loopback 127.0.0.1.

  -config=<path>
    The path to either a single config file or a directory of config
    files to use for configuring the Nomad agent. This option may be
    specified multiple times. If multiple config files are used, the
    values from each will be merged together. During merging, values
    from files found later in the list are merged over values from
    previously parsed files.

  -data-dir=<path>
    The data directory used to store state and other persistent data.
    On client machines this is used to house allocation data such as
    downloaded artifacts used by drivers. On server nodes, the data
    dir is also used to store the replicated log.

  -plugin-dir=<path>
    The plugin directory is used to discover Nomad plugins. If not specified,
    the plugin directory defaults to be that of <data-dir>/plugins/.

  -dc=<datacenter>
    The name of the datacenter this Nomad agent is a member of. By
    default this is set to "dc1".

  -log-level=<level>
    Specify the verbosity level of Nomad's logs. Valid values include
    DEBUG, INFO, and WARN, in decreasing order of verbosity. The
    default is INFO.

  -log-json
    Output logs in a JSON format. The default is false.

  -log-include-location
    Include file and line information in each log line. The default is false.

  -node=<name>
    The name of the local agent. This name is used to identify the node
    in the cluster. The name must be unique per region. The default is
    the current hostname of the machine.

  -region=<region>
    Name of the region the Nomad agent will be a member of. By default
    this value is set to "global".

  -dev
    Start the agent in development mode. This enables a pre-configured
    dual-role agent (client + server) which is useful for developing
    or testing Nomad. No other configuration is required to start the
    agent in this mode, but you may pass an optional comma-separated
    list of mode configurations:

  -dev-connect
    Start the agent in development mode, but bind to a public network
    interface rather than localhost for using Consul Connect. It may be used
    with -dev-consul to configure default workload identities for Consul. This
    mode is supported only on Linux as root.

  -dev-consul
    Starts the agent in development mode with a default Consul configuration
    for Nomad workload identity. It may be used with -dev-connect to configure
    the agent for Consul Service Mesh.

  -dev-vault
    Starts the agent in development mode with a default Vault configuration
    for Nomad workload identity.

Server Options:

  -server
    Enable server mode for the agent. Agents in server mode are
    clustered together and handle the additional responsibility of
    leader election, data replication, and scheduling work onto
    eligible client nodes.

  -bootstrap-expect=<num>
    Configures the expected number of servers nodes to wait for before
    bootstrapping the cluster. Once <num> servers have joined each other,
    Nomad initiates the bootstrap process.

  -encrypt=<key>
    Provides the gossip encryption key

  -join=<address>
    Address of an agent to join at start time. Can be specified
    multiple times.

  -raft-protocol=<num>
    The Raft protocol version to use. Used for enabling certain Autopilot
    features. Defaults to 2.

  -retry-join=<address>
    Address of an agent to join at start time with retries enabled.
    Can be specified multiple times.

  -retry-max=<num>
    Maximum number of join attempts. Defaults to 0, which will retry
    indefinitely.

  -retry-interval=<dur>
    Time to wait between join attempts.

  -rejoin
    Ignore a previous leave and attempts to rejoin the cluster.

Client Options:

  -client
    Enable client mode for the agent. Client mode enables a given node to be
    evaluated for allocations. If client mode is not enabled, no work will be
    scheduled to the agent.

  -state-dir
    The directory used to store state and other persistent data. If not
    specified a subdirectory under the "-data-dir" will be used.

  -alloc-dir
    The directory used to store allocation data such as downloaded artifacts as
    well as data produced by tasks. If not specified, a subdirectory under the
    "-data-dir" will be used.

  -servers
    A list of known server addresses to connect to given as "host:port" and
    delimited by commas.

  -node-class
    Mark this node as a member of a node-class. This can be used to label
    similar node types.

  -node-pool
    Register this node in this node pool. If the node pool does not exist it
    will be created automatically if the node registers in the authoritative
    region. In non-authoritative regions, the node is kept in the
    'initializing' status until the node pool is created and replicated.

  -meta
    User specified metadata to associated with the node. Each instance of -meta
    parses a single KEY=VALUE pair. Repeat the meta flag for each key/value pair
    to be added.

  -network-interface
    Forces the network fingerprinter to use the specified network interface.
  
  -preferred-address-family
    Specify which IP family to prefer when selecting an IP address of the
    network interface. Valid values are "ipv4" and "ipv6". When not specified,
    the agent will not sort the addresses and use the first one.

  -network-speed
    The default speed for network interfaces in MBits if the link speed can not
    be determined dynamically.

  -host-volumes-dir
    Directory wherein host volume plugins should place volumes. The default is
    <data-dir>/host_volumes.

  -host-volume-plugin-dir
    Directory containing dynamic host volume plugins. The default is
    <data-dir>/host_volume_plugins.

ACL Options:

  -acl-enabled
    Specifies whether the agent should enable ACLs.

  -acl-replication-token
    The replication token for servers to use when replicating from the
    authoritative region. The token must be a valid management token from the
    authoritative region.

Consul Options:

  -consul-address=<addr>
    Specifies the address to the local Consul agent, given in the format host:port.
    Supports Unix sockets with the format: unix:///tmp/consul/consul.sock

  -consul-auth=<auth>
    Specifies the HTTP Basic Authentication information to use for access to the
    Consul Agent, given in the format username:password.

  -consul-auto-advertise
    Specifies if Nomad should advertise its services in Consul. The services
    are named according to server_service_name and client_service_name. Nomad
    servers and clients advertise their respective services, each tagged
    appropriately with either http or rpc tag. Nomad servers also advertise a
    serf tagged service.

  -consul-ca-file=<path>
    Specifies an optional path to the CA certificate used for Consul communication.
    This defaults to the system bundle if unspecified.

  -consul-cert-file=<path>
    Specifies the path to the certificate used for Consul communication. If this
    is set then you need to also set key_file.

  -consul-checks-use-advertise
    Specifies if Consul heath checks should bind to the advertise address. By
    default, this is the bind address.

  -consul-client-auto-join
    Specifies if the Nomad clients should automatically discover servers in the
    same region by searching for the Consul service name defined in the
    server_service_name option.

  -consul-client-service-name=<name>
    Specifies the name of the service in Consul for the Nomad clients.

  -consul-client-http-check-name=<name>
    Specifies the HTTP health check name in Consul for the Nomad clients.

  -consul-client-failures-before-critical
    Specifies the number of consecutive failures before the Nomad client
    Consul health check is critical. Defaults to 0.

  -consul-client-failures-before-warning
    Specifies the number of consecutive failures before the Nomad client
    Consul health check shows a warning. Defaults to 0.

  -consul-key-file=<path>
    Specifies the path to the private key used for Consul communication. If this
    is set then you need to also set cert_file.

  -consul-server-service-name=<name>
    Specifies the name of the service in Consul for the Nomad servers.

  -consul-server-http-check-name=<name>
    Specifies the HTTP health check name in Consul for the Nomad servers.

  -consul-server-serf-check-name=<name>
    Specifies the Serf health check name in Consul for the Nomad servers.

  -consul-server-rpc-check-name=<name>
    Specifies the RPC health check name in Consul for the Nomad servers.

  -consul-server-auto-join
    Specifies if the Nomad servers should automatically discover and join other
    Nomad servers by searching for the Consul service name defined in the
    server_service_name option. This search only happens if the server does not
    have a leader.

  -consul-server-failures-before-critical
    Specifies the number of consecutive failures before the Nomad server
    Consul health check is critical. Defaults to 0.

  -consul-server-failures-before-warning
    Specifies the number of consecutive failures before the Nomad server
    Consul health check shows a warning. Defaults to 0.

  -consul-ssl
    Specifies if the transport scheme should use HTTPS to communicate with the
    Consul agent.

  -consul-token=<token>
    Specifies the token used to provide a per-request ACL token.

  -consul-verify-ssl
    Specifies if SSL peer verification should be used when communicating to the
    Consul API client over HTTPS.

Vault Options:

  -vault-enabled
    Whether to enable or disable Vault integration.

  -vault-address=<addr>
    The address to communicate with Vault. This should be provided with the http://
    or https:// prefix.

  -vault-token=<token>
    The Vault token used to derive tokens from Vault on behalf of clients.
    This only needs to be set on Servers. Overrides the Vault token read from
    the VAULT_TOKEN environment variable.

  -vault-create-from-role=<role>
    The role name to create tokens for tasks from.

  -vault-allow-unauthenticated
    Whether to allow jobs to be submitted that request Vault Tokens but do not
    authentication. The flag only applies to Servers.

  -vault-ca-file=<path>
    The path to a PEM-encoded CA cert file to use to verify the Vault server SSL
    certificate.

  -vault-ca-path=<path>
    The path to a directory of PEM-encoded CA cert files to verify the Vault server
    certificate.

  -vault-cert-file=<token>
    The path to the certificate for Vault communication.

  -vault-key-file=<addr>
    The path to the private key for Vault communication.

  -vault-tls-skip-verify=<token>
    Enables or disables SSL certificate verification.

  -vault-tls-server-name=<token>
    Used to set the SNI host when connecting over TLS.
 `
	return strings.TrimSpace(helpText)
}
