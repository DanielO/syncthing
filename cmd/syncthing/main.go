// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof" // Need to import this to support STPROFILER.
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/dialer"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/logger"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/syncthing/syncthing/lib/tlsutil"
	"github.com/syncthing/syncthing/lib/upgrade"

	"github.com/pkg/errors"
)

const (
	exitSuccess            = 0
	exitError              = 1
	exitNoUpgradeAvailable = 2
	exitRestarting         = 3
	exitUpgrading          = 4
)

const (
	bepProtocolName      = "bep/1.0"
	tlsDefaultCommonName = "syncthing"
	maxSystemErrors      = 5
	initialSystemLog     = 10
	maxSystemLog         = 250
)

var myID protocol.DeviceID

const (
	usage      = "syncthing [options]"
	extraUsage = `
The -logflags value is a sum of the following:

   1  Date
   2  Time
   4  Microsecond time
   8  Long filename
  16  Short filename

I.e. to prefix each log line with date and time, set -logflags=3 (1 + 2 from
above). The value 0 is used to disable all of the above. The default is to
show time only (2).


Development Settings
--------------------

The following environment variables modify Syncthing's behavior in ways that
are mostly useful for developers. Use with care.

 STNODEFAULTFOLDER Don't create a default folder when starting for the first
                   time. This variable will be ignored anytime after the first
                   run.

 STGUIASSETS       Directory to load GUI assets from. Overrides compiled in
                   assets.

 STTRACE           A comma separated string of facilities to trace. The valid
                   facility strings listed below.

 STPROFILER        Set to a listen address such as "127.0.0.1:9090" to start
                   the profiler with HTTP access.

 STCPUPROFILE      Write a CPU profile to cpu-$pid.pprof on exit.

 STHEAPPROFILE     Write heap profiles to heap-$pid-$timestamp.pprof each time
                   heap usage increases.

 STBLOCKPROFILE    Write block profiles to block-$pid-$timestamp.pprof every 20
                   seconds.

 STPERFSTATS       Write running performance statistics to perf-$pid.csv. Not
                   supported on Windows.

 STDEADLOCKTIMEOUT Used for debugging internal deadlocks; sets debug
                   sensitivity. Use only under direction of a developer.

 STLOCKTHRESHOLD   Used for debugging internal deadlocks; sets debug
                   sensitivity.  Use only under direction of a developer.

 STNORESTART       Equivalent to the -no-restart argument. Disable the
                   Syncthing monitor process which handles restarts for some
                   configuration changes, upgrades, crashes and also log file
                   writing (stdout is still written).

 STNOUPGRADE       Disable automatic upgrades.

 STHASHING         Select the SHA256 hashing package to use. Possible values
                   are "standard" for the Go standard library implementation,
                   "minio" for the github.com/minio/sha256-simd implementation,
                   and blank (the default) for auto detection.

 STRECHECKDBEVERY  Set to a time interval to override the default database
                   check interval of 30 days (720h). The interval understands
                   "h", "m" and "s" abbreviations for hours minutes and seconds.
                   Valid values are like "720h", "30s", etc.

 GOMAXPROCS        Set the maximum number of CPU cores to use. Defaults to all
                   available CPU cores.

 GOGC              Percentage of heap growth at which to trigger GC. Default is
                   100. Lower numbers keep peak memory usage down, at the price
                   of CPU usage (i.e. performance).


Debugging Facilities
--------------------

The following are valid values for the STTRACE variable:

%s`
)

// Environment options
var (
	innerProcess    = os.Getenv("STNORESTART") != "" || os.Getenv("STMONITORED") != ""
	noDefaultFolder = os.Getenv("STNODEFAULTFOLDER") != ""
)

type RuntimeOptions struct {
	syncthing.Options
	confDir          string
	resetDatabase    bool
	showVersion      bool
	showPaths        bool
	showDeviceId     bool
	doUpgrade        bool
	doUpgradeCheck   bool
	upgradeTo        string
	noBrowser        bool
	browserOnly      bool
	hideConsole      bool
	logFile          string
	auditEnabled     bool
	auditFile        string
	paused           bool
	unpaused         bool
	guiAddress       string
	guiAPIKey        string
	generateDir      string
	noRestart        bool
	cpuProfile       bool
	stRestarting     bool
	logFlags         int
	showHelp         bool
	allowNewerConfig bool
	umask            string
}

func defaultRuntimeOptions() RuntimeOptions {
	options := RuntimeOptions{
		Options: syncthing.Options{
			AssetDir:    os.Getenv("STGUIASSETS"),
			NoUpgrade:   os.Getenv("STNOUPGRADE") != "",
			ProfilerURL: os.Getenv("STPROFILER"),
		},
		noRestart:    os.Getenv("STNORESTART") != "",
		cpuProfile:   os.Getenv("STCPUPROFILE") != "",
		stRestarting: os.Getenv("STRESTART") != "",
		logFlags:     log.Ltime,
		umask:        "",
	}

	if os.Getenv("STTRACE") != "" {
		options.logFlags = logger.DebugFlags
	}

	if runtime.GOOS != "windows" {
		// On non-Windows, we explicitly default to "-" which means stdout. On
		// Windows, the blank options.logFile will later be replaced with the
		// default path, unless the user has manually specified "-" or
		// something else.
		options.logFile = "-"
	}

	return options
}

func parseCommandLineOptions() RuntimeOptions {
	options := defaultRuntimeOptions()

	flag.StringVar(&options.generateDir, "generate", "", "Generate key and config in specified dir, then exit")
	flag.StringVar(&options.guiAddress, "gui-address", options.guiAddress, "Override GUI address (e.g. \"http://192.0.2.42:8443\")")
	flag.StringVar(&options.guiAPIKey, "gui-apikey", options.guiAPIKey, "Override GUI API key")
	flag.StringVar(&options.confDir, "home", "", "Set configuration directory")
	flag.IntVar(&options.logFlags, "logflags", options.logFlags, "Select information in log line prefix (see below)")
	flag.BoolVar(&options.noBrowser, "no-browser", false, "Do not start browser")
	flag.BoolVar(&options.browserOnly, "browser-only", false, "Open GUI in browser")
	flag.BoolVar(&options.noRestart, "no-restart", options.noRestart, "Disable monitor process, managed restarts and log file writing")
	flag.BoolVar(&options.resetDatabase, "reset-database", false, "Reset the database, forcing a full rescan and resync")
	flag.BoolVar(&options.ResetDeltaIdxs, "reset-deltas", false, "Reset delta index IDs, forcing a full index exchange")
	flag.BoolVar(&options.doUpgrade, "upgrade", false, "Perform upgrade")
	flag.BoolVar(&options.doUpgradeCheck, "upgrade-check", false, "Check for available upgrade")
	flag.BoolVar(&options.showVersion, "version", false, "Show version")
	flag.BoolVar(&options.showHelp, "help", false, "Show this help")
	flag.BoolVar(&options.showPaths, "paths", false, "Show configuration paths")
	flag.BoolVar(&options.showDeviceId, "device-id", false, "Show the device ID")
	flag.StringVar(&options.upgradeTo, "upgrade-to", options.upgradeTo, "Force upgrade directly from specified URL")
	flag.BoolVar(&options.auditEnabled, "audit", false, "Write events to audit file")
	flag.BoolVar(&options.Verbose, "verbose", false, "Print verbose log output")
	flag.BoolVar(&options.paused, "paused", false, "Start with all devices and folders paused")
	flag.BoolVar(&options.unpaused, "unpaused", false, "Start with all devices and folders unpaused")
	flag.StringVar(&options.logFile, "logfile", options.logFile, "Log file name (still always logs to stdout). Cannot be used together with -no-restart/STNORESTART environment variable.")
	flag.StringVar(&options.auditFile, "auditfile", options.auditFile, "Specify audit file (use \"-\" for stdout, \"--\" for stderr)")
	flag.BoolVar(&options.allowNewerConfig, "allow-newer-config", false, "Allow loading newer than current config version")
	if runtime.GOOS == "windows" {
		// Allow user to hide the console window
		flag.BoolVar(&options.hideConsole, "no-console", false, "Hide console window")
	} else {
		flag.StringVar(&options.umask, "umask", options.umask, "Set umask")
	}

	longUsage := fmt.Sprintf(extraUsage, debugFacilities())
	flag.Usage = usageFor(flag.CommandLine, usage, longUsage)
	flag.Parse()

	if len(flag.Args()) > 0 {
		flag.Usage()
		os.Exit(2)
	}

	return options
}

func main() {
	options := parseCommandLineOptions()
	l.SetFlags(options.logFlags)
	if runtime.GOOS != "windows" {
		if options.umask != "" {
			var mask int64
			var err error
			if mask, err = strconv.ParseInt(options.umask, 0, 9); err != nil {
				l.Warnln("Failed to parse umask:", err)
				os.Exit(exitError)
			}

			if mask < 0 || mask > 0777 {
				l.Warnln("Umask invalid, must between 0000 and 0777")
				os.Exit(exitError)
			}
			umask(int(mask))
		}
	}

	if options.guiAddress != "" {
		// The config picks this up from the environment.
		os.Setenv("STGUIADDRESS", options.guiAddress)
	}
	if options.guiAPIKey != "" {
		// The config picks this up from the environment.
		os.Setenv("STGUIAPIKEY", options.guiAPIKey)
	}

	// Check for options which are not compatible with each other. We have
	// to check logfile before it's set to the default below - we only want
	// to complain if they set -logfile explicitly, not if it's set to its
	// default location
	if options.noRestart && (options.logFile != "" && options.logFile != "-") {
		l.Warnln("-logfile may not be used with -no-restart or STNORESTART")
		os.Exit(exitError)
	}

	if options.hideConsole {
		osutil.HideConsole()
	}

	if options.confDir != "" {
		// Not set as default above because the string can be really long.
		if !filepath.IsAbs(options.confDir) {
			var err error
			options.confDir, err = filepath.Abs(options.confDir)
			if err != nil {
				l.Warnln("Failed to make options path absolute:", err)
				os.Exit(exitError)
			}
		}
		if err := locations.SetBaseDir(locations.ConfigBaseDir, options.confDir); err != nil {
			l.Warnln(err)
			os.Exit(exitError)
		}
	}

	if options.logFile == "" {
		// Blank means use the default logfile location. We must set this
		// *after* expandLocations above.
		options.logFile = locations.Get(locations.LogFile)
	}

	if options.AssetDir == "" {
		// The asset dir is blank if STGUIASSETS wasn't set, in which case we
		// should look for extra assets in the default place.
		options.AssetDir = locations.Get(locations.GUIAssets)
	}

	if options.showVersion {
		fmt.Println(build.LongVersion)
		return
	}

	if options.showHelp {
		flag.Usage()
		return
	}

	if options.showPaths {
		showPaths(options)
		return
	}

	if options.showDeviceId {
		cert, err := tls.LoadX509KeyPair(
			locations.Get(locations.CertFile),
			locations.Get(locations.KeyFile),
		)
		if err != nil {
			l.Warnln("Error reading device ID:", err)
			os.Exit(exitError)
		}

		myID = protocol.NewDeviceID(cert.Certificate[0])
		fmt.Println(myID)
		return
	}

	if options.browserOnly {
		if err := openGUI(); err != nil {
			l.Warnln("Failed to open web UI:", err)
			os.Exit(exitError)
		}
		return
	}

	if options.generateDir != "" {
		if err := generate(options.generateDir); err != nil {
			l.Warnln("Failed to generate config and keys:", err)
			os.Exit(exitError)
		}
		return
	}

	// Ensure that our home directory exists.
	if err := ensureDir(locations.GetBaseDir(locations.ConfigBaseDir), 0700); err != nil {
		l.Warnln("Failure on home directory:", err)
		os.Exit(exitError)
	}

	if options.upgradeTo != "" {
		err := upgrade.ToURL(options.upgradeTo)
		if err != nil {
			l.Warnln("Error while Upgrading:", err)
			os.Exit(exitError)
		}
		l.Infoln("Upgraded from", options.upgradeTo)
		return
	}

	if options.doUpgradeCheck {
		checkUpgrade()
		return
	}

	if options.doUpgrade {
		release := checkUpgrade()
		performUpgrade(release)
		return
	}

	if options.resetDatabase {
		if err := resetDB(); err != nil {
			l.Warnln("Resetting database:", err)
			os.Exit(exitError)
		}
		return
	}

	if innerProcess || options.noRestart {
		syncthingMain(options)
	} else {
		monitorMain(options)
	}
}

func openGUI() error {
	cfg, err := loadOrDefaultConfig()
	if err != nil {
		return err
	}
	if cfg.GUI().Enabled {
		if err := openURL(cfg.GUI().URL()); err != nil {
			return err
		}
	} else {
		l.Warnln("Browser: GUI is currently disabled")
	}
	return nil
}

func generate(generateDir string) error {
	dir, err := fs.ExpandTilde(generateDir)
	if err != nil {
		return err
	}

	if err := ensureDir(dir, 0700); err != nil {
		return err
	}

	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err == nil {
		l.Warnln("Key exists; will not overwrite.")
		l.Infoln("Device ID:", protocol.NewDeviceID(cert.Certificate[0]))
	} else {
		cert, err = tlsutil.NewCertificate(certFile, keyFile, tlsDefaultCommonName)
		if err != nil {
			return errors.Wrap(err, "create certificate")
		}
		myID = protocol.NewDeviceID(cert.Certificate[0])
		if err != nil {
			return errors.Wrap(err, "load certificate")
		}
		if err == nil {
			l.Infoln("Device ID:", protocol.NewDeviceID(cert.Certificate[0]))
		}
	}

	cfgFile := filepath.Join(dir, "config.xml")
	if _, err := os.Stat(cfgFile); err == nil {
		l.Warnln("Config exists; will not overwrite.")
		return nil
	}
	cfg, err := defaultConfig(cfgFile)
	if err != nil {
		return err
	}
	err = cfg.Save()
	if err != nil {
		return errors.Wrap(err, "save config")
	}
	return nil
}

func debugFacilities() string {
	facilities := l.Facilities()

	// Get a sorted list of names
	var names []string
	maxLen := 0
	for name := range facilities {
		names = append(names, name)
		if len(name) > maxLen {
			maxLen = len(name)
		}
	}
	sort.Strings(names)

	// Format the choices
	b := new(bytes.Buffer)
	for _, name := range names {
		fmt.Fprintf(b, " %-*s - %s\n", maxLen, name, facilities[name])
	}
	return b.String()
}

func checkUpgrade() upgrade.Release {
	cfg, _ := loadOrDefaultConfig()
	opts := cfg.Options()
	release, err := upgrade.LatestRelease(opts.ReleasesURL, build.Version, opts.UpgradeToPreReleases)
	if err != nil {
		l.Warnln("Upgrade:", err)
		os.Exit(exitError)
	}

	if upgrade.CompareVersions(release.Tag, build.Version) <= 0 {
		noUpgradeMessage := "No upgrade available (current %q >= latest %q)."
		l.Infof(noUpgradeMessage, build.Version, release.Tag)
		os.Exit(exitNoUpgradeAvailable)
	}

	l.Infof("Upgrade available (current %q < latest %q)", build.Version, release.Tag)
	return release
}

func performUpgrade(release upgrade.Release) {
	// Use leveldb database locks to protect against concurrent upgrades
	_, err := syncthing.OpenGoleveldb(locations.Get(locations.Database))
	if err == nil {
		err = upgrade.To(release)
		if err != nil {
			l.Warnln("Upgrade:", err)
			os.Exit(exitError)
		}
		l.Infof("Upgraded to %q", release.Tag)
	} else {
		l.Infoln("Attempting upgrade through running Syncthing...")
		err = upgradeViaRest()
		if err != nil {
			l.Warnln("Upgrade:", err)
			os.Exit(exitError)
		}
		l.Infoln("Syncthing upgrading")
		os.Exit(exitUpgrading)
	}
}

func upgradeViaRest() error {
	cfg, _ := loadOrDefaultConfig()
	u, err := url.Parse(cfg.GUI().URL())
	if err != nil {
		return err
	}
	u.Path = path.Join(u.Path, "rest/system/upgrade")
	target := u.String()
	r, _ := http.NewRequest("POST", target, nil)
	r.Header.Set("X-API-Key", cfg.GUI().APIKey)

	tr := &http.Transport{
		Dial:            dialer.Dial,
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   60 * time.Second,
	}
	resp, err := client.Do(r)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		bs, err := ioutil.ReadAll(resp.Body)
		defer resp.Body.Close()
		if err != nil {
			return err
		}
		return errors.New(string(bs))
	}

	return err
}

func syncthingMain(runtimeOptions RuntimeOptions) {
	cfg, err := loadConfigAtStartup(runtimeOptions.allowNewerConfig)
	if err != nil {
		l.Warnln("Failed to initialize config:", err)
		os.Exit(exitError)
	}

	if runtimeOptions.unpaused {
		setPauseState(cfg, false)
	} else if runtimeOptions.paused {
		setPauseState(cfg, true)
	}

	// Ensure that we have a certificate and key.
	cert, err := tls.LoadX509KeyPair(
		locations.Get(locations.CertFile),
		locations.Get(locations.KeyFile),
	)
	if err != nil {
		l.Infof("Generating ECDSA key and certificate for %s...", tlsDefaultCommonName)
		cert, err = tlsutil.NewCertificate(
			locations.Get(locations.CertFile),
			locations.Get(locations.KeyFile),
			tlsDefaultCommonName,
		)
		if err != nil {
			l.Warnln("Failed to generate certificate:", err)
			os.Exit(1)
		}
	}

	dbFile := locations.Get(locations.Database)
	ldb, err := syncthing.OpenGoleveldb(dbFile)
	if err != nil {
		l.Warnln("Error opening database:", err)
		os.Exit(1)
	}

	appOpts := runtimeOptions.Options
	if runtimeOptions.auditEnabled {
		appOpts.AuditWriter = auditWriter(runtimeOptions.auditFile)
	}
	if t := os.Getenv("STDEADLOCKTIMEOUT"); t != "" {
		secs, _ := strconv.Atoi(t)
		appOpts.DeadlockTimeoutS = secs
	}

	app := syncthing.New(cfg, ldb, cert, appOpts)

	setupSignalHandling(app)

	if len(os.Getenv("GOMAXPROCS")) == 0 {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	if runtimeOptions.cpuProfile {
		f, err := os.Create(fmt.Sprintf("cpu-%d.pprof", os.Getpid()))
		if err != nil {
			l.Warnln("Creating profile:", err)
			os.Exit(exitError)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			l.Warnln("Starting profile:", err)
			os.Exit(exitError)
		}
	}

	if opts := cfg.Options(); opts.RestartOnWakeup {
		go standbyMonitor(app)
	}

	// Candidate builds should auto upgrade. Make sure the option is set,
	// unless we are in a build where it's disabled or the STNOUPGRADE
	// environment variable is set.

	if build.IsCandidate && !upgrade.DisabledByCompilation && !runtimeOptions.NoUpgrade {
		l.Infoln("Automatic upgrade is always enabled for candidate releases.")
		if opts := cfg.Options(); opts.AutoUpgradeIntervalH == 0 || opts.AutoUpgradeIntervalH > 24 {
			opts.AutoUpgradeIntervalH = 12
			// Set the option into the config as well, as the auto upgrade
			// loop expects to read a valid interval from there.
			cfg.SetOptions(opts)
			cfg.Save()
		}
		// We don't tweak the user's choice of upgrading to pre-releases or
		// not, as otherwise they cannot step off the candidate channel.
	}

	if opts := cfg.Options(); opts.AutoUpgradeIntervalH > 0 {
		if runtimeOptions.NoUpgrade {
			l.Infof("No automatic upgrades; STNOUPGRADE environment variable defined.")
		} else {
			go autoUpgrade(cfg, app)
		}
	}

	app.Start()

	cleanConfigDirectory()

	if cfg.Options().StartBrowser && !runtimeOptions.noBrowser && !runtimeOptions.stRestarting {
		// Can potentially block if the utility we are invoking doesn't
		// fork, and just execs, hence keep it in its own routine.
		go func() { _ = openURL(cfg.GUI().URL()) }()
	}

	status := app.Wait()

	if runtimeOptions.cpuProfile {
		pprof.StopCPUProfile()
	}

	os.Exit(int(status))
}

func setupSignalHandling(app *syncthing.App) {
	// Exit cleanly with "restarting" code on SIGHUP.

	restartSign := make(chan os.Signal, 1)
	sigHup := syscall.Signal(1)
	signal.Notify(restartSign, sigHup)
	go func() {
		<-restartSign
		app.Stop(syncthing.ExitRestart)
	}()

	// Exit with "success" code (no restart) on INT/TERM

	stopSign := make(chan os.Signal, 1)
	sigTerm := syscall.Signal(15)
	signal.Notify(stopSign, os.Interrupt, sigTerm)
	go func() {
		<-stopSign
		app.Stop(syncthing.ExitSuccess)
	}()
}

func loadOrDefaultConfig() (config.Wrapper, error) {
	cfgFile := locations.Get(locations.ConfigFile)
	cfg, err := config.Load(cfgFile, myID)

	if err != nil {
		cfg, err = defaultConfig(cfgFile)
	}

	return cfg, err
}

func loadConfigAtStartup(allowNewerConfig bool) (config.Wrapper, error) {
	cfgFile := locations.Get(locations.ConfigFile)
	cfg, err := config.Load(cfgFile, myID)
	if os.IsNotExist(err) {
		cfg, err = defaultConfig(cfgFile)
		if err != nil {
			return nil, errors.Wrap(err, "failed to generate default config")
		}
		err = cfg.Save()
		if err != nil {
			return nil, errors.Wrap(err, "failed to save default config")
		}
		l.Infof("Default config saved. Edit %s to taste (with Syncthing stopped) or use the GUI", cfg.ConfigPath())
	} else if err == io.EOF {
		return nil, errors.New("Failed to load config: unexpected end of file. Truncated or empty configuration?")
	} else if err != nil {
		return nil, errors.Wrap(err, "failed to load config")
	}

	if cfg.RawCopy().OriginalVersion != config.CurrentVersion {
		if cfg.RawCopy().OriginalVersion == config.CurrentVersion+1101 {
			l.Infof("Now, THAT's what we call a config from the future! Don't worry. As long as you hit that wire with the connecting hook at precisely eighty-eight miles per hour the instant the lightning strikes the tower... everything will be fine.")
		}
		if cfg.RawCopy().OriginalVersion > config.CurrentVersion && !allowNewerConfig {
			return nil, fmt.Errorf("Config file version (%d) is newer than supported version (%d). If this is expected, use -allow-newer-config to override.", cfg.RawCopy().OriginalVersion, config.CurrentVersion)
		}
		err = archiveAndSaveConfig(cfg)
		if err != nil {
			return nil, errors.Wrap(err, "config archive")
		}
	}

	return cfg, nil
}

func archiveAndSaveConfig(cfg config.Wrapper) error {
	// Copy the existing config to an archive copy
	archivePath := cfg.ConfigPath() + fmt.Sprintf(".v%d", cfg.RawCopy().OriginalVersion)
	l.Infoln("Archiving a copy of old config file format at:", archivePath)
	if err := copyFile(cfg.ConfigPath(), archivePath); err != nil {
		return err
	}

	// Do a regular atomic config sve
	return cfg.Save()
}

func copyFile(src, dst string) error {
	bs, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(dst, bs, 0600); err != nil {
		// Attempt to clean up
		os.Remove(dst)
		return err
	}

	return nil
}

func auditWriter(auditFile string) io.Writer {
	var fd io.Writer
	var err error
	var auditDest string
	var auditFlags int

	if auditFile == "-" {
		fd = os.Stdout
		auditDest = "stdout"
	} else if auditFile == "--" {
		fd = os.Stderr
		auditDest = "stderr"
	} else {
		if auditFile == "" {
			auditFile = locations.GetTimestamped(locations.AuditLog)
			auditFlags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
		} else {
			auditFlags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		}
		fd, err = os.OpenFile(auditFile, auditFlags, 0600)
		if err != nil {
			l.Warnln("Audit:", err)
			os.Exit(exitError)
		}
		auditDest = auditFile
	}

	l.Infoln("Audit log in", auditDest)

	return fd
}

func defaultConfig(cfgFile string) (config.Wrapper, error) {
	newCfg, err := config.NewWithFreePorts(myID)
	if err != nil {
		return nil, err
	}

	if noDefaultFolder {
		l.Infoln("We will skip creation of a default folder on first start since the proper envvar is set")
		return config.Wrap(cfgFile, newCfg), nil
	}

	newCfg.Folders = append(newCfg.Folders, config.NewFolderConfiguration(myID, "default", "Default Folder", fs.FilesystemTypeBasic, locations.Get(locations.DefFolder)))
	l.Infoln("Default folder created and/or linked to new config")
	return config.Wrap(cfgFile, newCfg), nil
}

func resetDB() error {
	return os.RemoveAll(locations.Get(locations.Database))
}

func ensureDir(dir string, mode fs.FileMode) error {
	fs := fs.NewFilesystem(fs.FilesystemTypeBasic, dir)
	err := fs.MkdirAll(".", mode)
	if err != nil {
		return err
	}

	if fi, err := fs.Stat("."); err == nil {
		// Apprently the stat may fail even though the mkdirall passed. If it
		// does, we'll just assume things are in order and let other things
		// fail (like loading or creating the config...).
		currentMode := fi.Mode() & 0777
		if currentMode != mode {
			err := fs.Chmod(".", mode)
			// This can fail on crappy filesystems, nothing we can do about it.
			if err != nil {
				l.Warnln(err)
			}
		}
	}
	return nil
}

func standbyMonitor(app *syncthing.App) {
	restartDelay := 60 * time.Second
	now := time.Now()
	for {
		time.Sleep(10 * time.Second)
		if time.Since(now) > 2*time.Minute {
			l.Infof("Paused state detected, possibly woke up from standby. Restarting in %v.", restartDelay)

			// We most likely just woke from standby. If we restart
			// immediately chances are we won't have networking ready. Give
			// things a moment to stabilize.
			time.Sleep(restartDelay)

			app.Stop(syncthing.ExitRestart)
			return
		}
		now = time.Now()
	}
}

func autoUpgrade(cfg config.Wrapper, app *syncthing.App) {
	timer := time.NewTimer(0)
	sub := events.Default.Subscribe(events.DeviceConnected)
	for {
		select {
		case event := <-sub.C():
			data, ok := event.Data.(map[string]string)
			if !ok || data["clientName"] != "syncthing" || upgrade.CompareVersions(data["clientVersion"], build.Version) != upgrade.Newer {
				continue
			}
			l.Infof("Connected to device %s with a newer version (current %q < remote %q). Checking for upgrades.", data["id"], build.Version, data["clientVersion"])
		case <-timer.C:
		}

		opts := cfg.Options()
		checkInterval := time.Duration(opts.AutoUpgradeIntervalH) * time.Hour
		if checkInterval < time.Hour {
			// We shouldn't be here if AutoUpgradeIntervalH < 1, but for
			// safety's sake.
			checkInterval = time.Hour
		}

		rel, err := upgrade.LatestRelease(opts.ReleasesURL, build.Version, opts.UpgradeToPreReleases)
		if err == upgrade.ErrUpgradeUnsupported {
			events.Default.Unsubscribe(sub)
			return
		}
		if err != nil {
			// Don't complain too loudly here; we might simply not have
			// internet connectivity, or the upgrade server might be down.
			l.Infoln("Automatic upgrade:", err)
			timer.Reset(checkInterval)
			continue
		}

		if upgrade.CompareVersions(rel.Tag, build.Version) != upgrade.Newer {
			// Skip equal, older or majorly newer (incompatible) versions
			timer.Reset(checkInterval)
			continue
		}

		l.Infof("Automatic upgrade (current %q < latest %q)", build.Version, rel.Tag)
		err = upgrade.To(rel)
		if err != nil {
			l.Warnln("Automatic upgrade:", err)
			timer.Reset(checkInterval)
			continue
		}
		events.Default.Unsubscribe(sub)
		l.Warnf("Automatically upgraded to version %q. Restarting in 1 minute.", rel.Tag)
		time.Sleep(time.Minute)
		app.Stop(syncthing.ExitUpgrade)
		return
	}
}

// cleanConfigDirectory removes old, unused configuration and index formats, a
// suitable time after they have gone out of fashion.
func cleanConfigDirectory() {
	patterns := map[string]time.Duration{
		"panic-*.log":        7 * 24 * time.Hour,  // keep panic logs for a week
		"audit-*.log":        7 * 24 * time.Hour,  // keep audit logs for a week
		"index":              14 * 24 * time.Hour, // keep old index format for two weeks
		"index-v0.11.0.db":   14 * 24 * time.Hour, // keep old index format for two weeks
		"index-v0.13.0.db":   14 * 24 * time.Hour, // keep old index format for two weeks
		"index*.converted":   14 * 24 * time.Hour, // keep old converted indexes for two weeks
		"config.xml.v*":      30 * 24 * time.Hour, // old config versions for a month
		"*.idx.gz":           30 * 24 * time.Hour, // these should for sure no longer exist
		"backup-of-v0.8":     30 * 24 * time.Hour, // these neither
		"tmp-index-sorter.*": time.Minute,         // these should never exist on startup
		"support-bundle-*":   30 * 24 * time.Hour, // keep old support bundle zip or folder for a month
	}

	for pat, dur := range patterns {
		fs := fs.NewFilesystem(fs.FilesystemTypeBasic, locations.GetBaseDir(locations.ConfigBaseDir))
		files, err := fs.Glob(pat)
		if err != nil {
			l.Infoln("Cleaning:", err)
			continue
		}

		for _, file := range files {
			info, err := fs.Lstat(file)
			if err != nil {
				l.Infoln("Cleaning:", err)
				continue
			}

			if time.Since(info.ModTime()) > dur {
				if err = fs.RemoveAll(file); err != nil {
					l.Infoln("Cleaning:", err)
				} else {
					l.Infoln("Cleaned away old file", filepath.Base(file))
				}
			}
		}
	}
}

func showPaths(options RuntimeOptions) {
	fmt.Printf("Configuration file:\n\t%s\n\n", locations.Get(locations.ConfigFile))
	fmt.Printf("Database directory:\n\t%s\n\n", locations.Get(locations.Database))
	fmt.Printf("Device private key & certificate files:\n\t%s\n\t%s\n\n", locations.Get(locations.KeyFile), locations.Get(locations.CertFile))
	fmt.Printf("HTTPS private key & certificate files:\n\t%s\n\t%s\n\n", locations.Get(locations.HTTPSKeyFile), locations.Get(locations.HTTPSCertFile))
	fmt.Printf("Log file:\n\t%s\n\n", options.logFile)
	fmt.Printf("GUI override directory:\n\t%s\n\n", options.AssetDir)
	fmt.Printf("Default sync folder directory:\n\t%s\n\n", locations.Get(locations.DefFolder))
}

func setPauseState(cfg config.Wrapper, paused bool) {
	raw := cfg.RawCopy()
	for i := range raw.Devices {
		raw.Devices[i].Paused = paused
	}
	for i := range raw.Folders {
		raw.Folders[i].Paused = paused
	}
	if _, err := cfg.Replace(raw); err != nil {
		l.Warnln("Cannot adjust paused state:", err)
		os.Exit(exitError)
	}
}
