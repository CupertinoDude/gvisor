// Copyright 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Binary runsc is an implementation of the Open Container Initiative Runtime
// that runs applications inside a sandbox.
package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"context"
	"flag"

	"github.com/google/subcommands"
	"gvisor.googlesource.com/gvisor/pkg/log"
	"gvisor.googlesource.com/gvisor/runsc/boot"
	"gvisor.googlesource.com/gvisor/runsc/cmd"
	"gvisor.googlesource.com/gvisor/runsc/specutils"
)

var (
	// Although these flags are not part of the OCI spec, they are used by
	// Docker, and thus should not be changed.
	rootDir     = flag.String("root", "", "root directory for storage of container state")
	logFilename = flag.String("log", "", "file path where internal debug information is written, default is stdout")
	logFormat   = flag.String("log-format", "text", "log format: text (default) or json")
	debug       = flag.Bool("debug", false, "enable debug logging")

	// These flags are unique to runsc, and are used to configure parts of the
	// system that are not covered by the runtime spec.

	// Debugging flags.
	debugLogDir = flag.String("debug-log-dir", "", "additional location for logs. It creates individual log files per command")
	logPackets  = flag.Bool("log-packets", false, "enable network packet logging")
	logFD       = flag.Int("log-fd", -1, "file descriptor to log to.  If set, the 'log' flag is ignored.")
	debugLogFD  = flag.Int("debug-log-fd", -1, "file descriptor to write debug logs to.  If set, the 'debug-log-dir' flag is ignored.")

	// Debugging flags: strace related
	strace         = flag.Bool("strace", false, "enable strace")
	straceSyscalls = flag.String("strace-syscalls", "", "comma-separated list of syscalls to trace. If --strace is true and this list is empty, then all syscalls will be traced.")
	straceLogSize  = flag.Uint("strace-log-size", 1024, "default size (in bytes) to log data argument blobs")

	// Flags that control sandbox runtime behavior.
	platform       = flag.String("platform", "ptrace", "specifies which platform to use: ptrace (default), kvm")
	network        = flag.String("network", "sandbox", "specifies which network to use: sandbox (default), host, none. Using network inside the sandbox is more secure because it's isolated from the host network.")
	fileAccess     = flag.String("file-access", "exclusive", "specifies which filesystem to use for the root mount: exclusive (default), shared. Volume mounts are always shared.")
	overlay        = flag.Bool("overlay", false, "wrap filesystem mounts with writable overlay. All modifications are stored in memory inside the sandbox.")
	multiContainer = flag.Bool("multi-container", false, "enable *experimental* multi-container support.")
	watchdogAction = flag.String("watchdog-action", "log", "sets what action the watchdog takes when triggered: log (default), panic.")
	panicSignal    = flag.Int("panic-signal", -1, "register signal handling that panics. Usually set to SIGUSR2(12) to troubleshoot hangs. -1 disables it.")
)

// gitRevision is set during linking.
var gitRevision = ""

func main() {
	// Help and flags commands are generated automatically.
	subcommands.Register(subcommands.HelpCommand(), "")
	subcommands.Register(subcommands.FlagsCommand(), "")

	// Register user-facing runsc commands.
	subcommands.Register(new(cmd.Checkpoint), "")
	subcommands.Register(new(cmd.Create), "")
	subcommands.Register(new(cmd.Delete), "")
	subcommands.Register(new(cmd.Events), "")
	subcommands.Register(new(cmd.Exec), "")
	subcommands.Register(new(cmd.Gofer), "")
	subcommands.Register(new(cmd.Kill), "")
	subcommands.Register(new(cmd.List), "")
	subcommands.Register(new(cmd.Pause), "")
	subcommands.Register(new(cmd.PS), "")
	subcommands.Register(new(cmd.Restore), "")
	subcommands.Register(new(cmd.Resume), "")
	subcommands.Register(new(cmd.Run), "")
	subcommands.Register(new(cmd.Start), "")
	subcommands.Register(new(cmd.State), "")
	subcommands.Register(new(cmd.Wait), "")

	// Register internal commands with the internal group name. This causes
	// them to be sorted below the user-facing commands with empty group.
	// The string below will be printed above the commands.
	const internalGroup = "internal use only"
	subcommands.Register(new(cmd.Boot), internalGroup)
	subcommands.Register(new(cmd.Debug), internalGroup)
	subcommands.Register(new(cmd.Gofer), internalGroup)

	// All subcommands must be registered before flag parsing.
	flag.Parse()

	platformType, err := boot.MakePlatformType(*platform)
	if err != nil {
		cmd.Fatalf("%v", err)
	}

	fsAccess, err := boot.MakeFileAccessType(*fileAccess)
	if err != nil {
		cmd.Fatalf("%v", err)
	}

	if fsAccess == boot.FileAccessShared && *overlay {
		cmd.Fatalf("overlay flag is incompatible with shared file access")
	}

	netType, err := boot.MakeNetworkType(*network)
	if err != nil {
		cmd.Fatalf("%v", err)
	}

	wa, err := boot.MakeWatchdogAction(*watchdogAction)
	if err != nil {
		cmd.Fatalf("%v", err)
	}

	// Create a new Config from the flags.
	conf := &boot.Config{
		RootDir:        *rootDir,
		Debug:          *debug,
		LogFilename:    *logFilename,
		LogFormat:      *logFormat,
		DebugLogDir:    *debugLogDir,
		FileAccess:     fsAccess,
		Overlay:        *overlay,
		Network:        netType,
		LogPackets:     *logPackets,
		Platform:       platformType,
		Strace:         *strace,
		StraceLogSize:  *straceLogSize,
		MultiContainer: *multiContainer,
		WatchdogAction: wa,
		PanicSignal:    *panicSignal,
	}
	if len(*straceSyscalls) != 0 {
		conf.StraceSyscalls = strings.Split(*straceSyscalls, ",")
	}

	// Set up logging.
	if *debug {
		log.SetLevel(log.Debug)
	}

	var logFile io.Writer = os.Stderr
	if *logFD > -1 {
		logFile = os.NewFile(uintptr(*logFD), "log file")
	} else if *logFilename != "" {
		// We must set O_APPEND and not O_TRUNC because Docker passes
		// the same log file for all commands (and also parses these
		// log files), so we can't destroy them on each command.
		f, err := os.OpenFile(*logFilename, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			cmd.Fatalf("error opening log file %q: %v", *logFilename, err)
		}
		logFile = f
	}

	var e log.Emitter
	switch *logFormat {
	case "text":
		e = log.GoogleEmitter{&log.Writer{Next: logFile}}
	case "json":
		e = log.JSONEmitter{log.Writer{Next: logFile}}
	default:
		cmd.Fatalf("invalid log format %q, must be 'json' or 'text'", *logFormat)
	}

	if *debugLogFD > -1 {
		f := os.NewFile(uintptr(*debugLogFD), "debug log file")
		e = log.MultiEmitter{e, log.GoogleEmitter{&log.Writer{Next: f}}}
	} else if *debugLogDir != "" {
		if err := os.MkdirAll(*debugLogDir, 0775); err != nil {
			cmd.Fatalf("error creating dir %q: %v", *debugLogDir, err)
		}
		subcommand := flag.CommandLine.Arg(0)
		f, err := specutils.DebugLogFile(*debugLogDir, subcommand)
		if err != nil {
			cmd.Fatalf("error opening debug log file in %q: %v", *debugLogDir, err)
		}
		e = log.MultiEmitter{e, log.GoogleEmitter{&log.Writer{Next: f}}}
	}

	log.SetTarget(e)

	log.Infof("***************************")
	log.Infof("Args: %s", os.Args)
	log.Infof("Git Revision: %s", gitRevision)
	log.Infof("PID: %d", os.Getpid())
	log.Infof("UID: %d, GID: %d", os.Getuid(), os.Getgid())
	log.Infof("Configuration:")
	log.Infof("\t\tRootDir: %s", conf.RootDir)
	log.Infof("\t\tPlatform: %v", conf.Platform)
	log.Infof("\t\tFileAccess: %v, overlay: %t", conf.FileAccess, conf.Overlay)
	log.Infof("\t\tNetwork: %v, logging: %t", conf.Network, conf.LogPackets)
	log.Infof("\t\tStrace: %t, max size: %d, syscalls: %s", conf.Strace, conf.StraceLogSize, conf.StraceSyscalls)
	log.Infof("***************************")

	// Call the subcommand and pass in the configuration.
	var ws syscall.WaitStatus
	subcmdCode := subcommands.Execute(context.Background(), conf, &ws)
	if subcmdCode == subcommands.ExitSuccess {
		log.Infof("Exiting with status: %v", ws)
		if ws.Signaled() {
			// No good way to return it, emulate what the shell does. Maybe raise
			// signall to self?
			os.Exit(128 + int(ws.Signal()))
		}
		os.Exit(ws.ExitStatus())
	}
	// Return an error that is unlikely to be used by the application.
	log.Warningf("Failure to execute command, err: %v", subcmdCode)
	os.Exit(128)
}

func init() {
	// Set default root dir to something (hopefully) user-writeable.
	*rootDir = "/var/run/runsc"
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		*rootDir = filepath.Join(runtimeDir, "runsc")
	}
}
