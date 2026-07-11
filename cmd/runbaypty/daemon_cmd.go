// daemon_cmd.go — `runbaypty daemon install|uninstall|start|stop|status`:
// OS-managed lifecycle (MISSION: the daemon is NEVER a child of any app).
//
// macOS: a launchd LaunchAgent (RunAtLoad + KeepAlive) — starts at login,
// restarts on crash. Linux: a systemd user service with Restart=on-failure.
// install copies the CURRENT binary to a stable path (~/.runbaypty/bin/)
// first: the on-PATH binary gets rebuilt/replaced constantly, and a
// supervisor must never point at a moving target.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/thesatellite-ai/runbaypty/pkg/constants"
	"github.com/thesatellite-ai/runbaypty/pkg/daemon"
	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"

	"github.com/spf13/cobra"
)

// launchdLabel is the LaunchAgent identifier (also the plist basename).
const launchdLabel = "com.runbay.runbaypty"

// systemdUnitName is the user-service unit filename on Linux.
const systemdUnitName = "runbaypty.service"

// Supervisor CLI contract — upstream command names/arguments (launchctl /
// systemctl own these strings; we translate INTO them, never invent).
// Constants because each appears at multiple call sites.
const (
	supervisorLaunchctl = "launchctl"
	supervisorSystemctl = "systemctl"
	launchctlBootout    = "bootout"
	launchctlBootstrap  = "bootstrap"
	launchctlKickstart  = "kickstart"
	systemctlUserFlag   = "--user"
	systemctlNowFlag    = "--now"
)

// launchdGUIDomain / launchdGUIService format launchd's per-user domain
// target ("gui/<uid>" and "gui/<uid>/<label>").
const (
	launchdGUIDomain  = "gui/%d"
	launchdGUIService = "gui/%d/%s"
)

// runtime.GOOS values this CLI branches on (Go toolchain contract; typed
// constants so a typo'd platform check is a compile-time miss, not a
// silently-false branch).
const (
	goosDarwin = "darwin"
	goosLinux  = "linux"
)

// generateLaunchdPlist renders the LaunchAgent. binPath must be the STABLE
// copy, not the on-PATH binary. Logs go under the runbaypty home.
func generateLaunchdPlist(binPath, homeDir string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + binPath + `</string>
		<string>serve</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>` + constants.EnvHome + `</key>
		<string>` + homeDir + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + filepath.Join(homeDir, constants.DaemonStdoutLog) + `</string>
	<key>StandardErrorPath</key>
	<string>` + filepath.Join(homeDir, constants.DaemonStderrLog) + `</string>
</dict>
</plist>
`
}

// generateSystemdUnit renders the Linux user service.
func generateSystemdUnit(binPath, homeDir string) string {
	return `[Unit]
Description=runbaypty — persistent PTY daemon
Documentation=https://github.com/thesatellite-ai/runbaypty

[Service]
Type=simple
ExecStart=` + binPath + ` serve
Environment=` + constants.EnvHome + `=` + homeDir + `
Restart=on-failure
RestartSec=1

[Install]
WantedBy=default.target
`
}

// stableBinDir is where install copies the binary (inside the home dir so
// one env var relocates everything).
func stableBinDir(homeDir string) string { return filepath.Join(homeDir, constants.StableBinDirname) }

// installPaths resolves every path install/uninstall touches.
type installPaths struct {
	homeDir   string
	stableBin string
	unitPath  string // plist on darwin, unit file on linux
}

func resolveInstallPaths() (installPaths, error) {
	home, err := constants.Home()
	if err != nil {
		return installPaths{}, errcodes.New(errcodes.Internal, "resolve home").WithCause(err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return installPaths{}, errcodes.New(errcodes.Internal, "resolve user home").WithCause(err)
	}
	p := installPaths{
		homeDir:   home,
		stableBin: filepath.Join(stableBinDir(home), constants.BinaryName),
	}
	switch runtime.GOOS {
	case goosDarwin:
		p.unitPath = filepath.Join(userHome, "Library", "LaunchAgents", launchdLabel+".plist")
	case goosLinux:
		p.unitPath = filepath.Join(userHome, ".config", "systemd", "user", systemdUnitName)
	default:
		return installPaths{}, errcodes.Newf(errcodes.Unsupported, "daemon install on %s (darwin + linux only)", runtime.GOOS)
	}
	return p, nil
}

// copyBinary copies the running executable to the stable path (0755),
// replacing atomically via tmp+rename so a running daemon's binary is
// never truncated in place.
func copyBinary(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return errcodes.New(errcodes.Internal, "locate executable").WithCause(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }() // read side; error carries nothing

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) // #nosec G302 -- it IS an executable
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("flush %s: %w", tmp, err)
	}
	return os.Rename(tmp, dst)
}

// runSupervisor shells out to launchctl / systemctl. Failures surface the
// command's own words — supervisor errors are the useful part.
func runSupervisor(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return errcodes.Newf(errcodes.Internal, "%s %s: %v — %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func newDaemonCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "daemon",
		Short: "Install and control the OS-managed daemon (launchd / systemd)",
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Copy the binary to a stable path and register the login service",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveInstallPaths()
			if err != nil {
				return err
			}
			if err := copyBinary(p.stableBin); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p.unitPath), 0o755); err != nil { // #nosec G301 -- LaunchAgents/systemd dirs are conventionally 0755
				return errcodes.Newf(errcodes.Internal, "create %s", filepath.Dir(p.unitPath)).WithCause(err)
			}
			var unit string
			if runtime.GOOS == goosDarwin {
				unit = generateLaunchdPlist(p.stableBin, p.homeDir)
			} else {
				unit = generateSystemdUnit(p.stableBin, p.homeDir)
			}
			if err := os.WriteFile(p.unitPath, []byte(unit), 0o644); err != nil { // #nosec G306 -- launchd/systemd must READ the unit; it holds no secrets
				return errcodes.Newf(errcodes.Internal, "write %s", p.unitPath).WithCause(err)
			}
			if runtime.GOOS == goosDarwin {
				// bootout first makes install idempotent (re-install = upgrade).
				_ = runSupervisor(supervisorLaunchctl, launchctlBootout, fmt.Sprintf(launchdGUIService, os.Getuid(), launchdLabel))
				if err := runSupervisor(supervisorLaunchctl, launchctlBootstrap, fmt.Sprintf(launchdGUIDomain, os.Getuid()), p.unitPath); err != nil {
					return err
				}
			} else {
				if err := runSupervisor(supervisorSystemctl, systemctlUserFlag, "daemon-reload"); err != nil {
					return err
				}
				if err := runSupervisor(supervisorSystemctl, systemctlUserFlag, "enable", systemctlNowFlag, systemdUnitName); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed: %s\nservice:   %s\nThe daemon now starts at login and restarts on crash.\n", p.stableBin, p.unitPath)
			return nil
		},
	}

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the service and remove the registration (sessions die)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveInstallPaths()
			if err != nil {
				return err
			}
			if runtime.GOOS == goosDarwin {
				_ = runSupervisor(supervisorLaunchctl, launchctlBootout, fmt.Sprintf(launchdGUIService, os.Getuid(), launchdLabel))
			} else {
				_ = runSupervisor(supervisorSystemctl, systemctlUserFlag, "disable", systemctlNowFlag, systemdUnitName)
			}
			if err := os.Remove(p.unitPath); err != nil && !os.IsNotExist(err) {
				return errcodes.Newf(errcodes.Internal, "remove %s", p.unitPath).WithCause(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "uninstalled — the stable binary at "+p.stableBin+" was left in place")
			return nil
		},
	}

	start := &cobra.Command{
		Use:   "start",
		Short: "Start (or restart) the installed service now",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS == goosDarwin {
				return runSupervisor(supervisorLaunchctl, launchctlKickstart, "-k", fmt.Sprintf(launchdGUIService, os.Getuid(), launchdLabel))
			}
			return runSupervisor(supervisorSystemctl, systemctlUserFlag, "restart", systemdUnitName)
		},
	}

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon (KeepAlive/Restart will NOT revive an explicit stop)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS == goosDarwin {
				return runSupervisor("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel))
			}
			return runSupervisor(supervisorSystemctl, systemctlUserFlag, "stop", systemdUnitName)
		},
	}

	var statusJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Report daemon liveness from the discovery file",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := constants.Home()
			if err != nil {
				return errcodes.New(errcodes.Internal, "resolve home").WithCause(err)
			}
			raw, err := os.ReadFile(filepath.Join(home, constants.DiscoveryFilename))
			if err != nil {
				if os.IsNotExist(err) {
					return errcodes.New(errcodes.DaemonUnreachable, "no discovery file — daemon not running").
						WithHint("start it: " + constants.BinaryName + " daemon start (or `serve` for dev)")
				}
				return errcodes.New(errcodes.Internal, "read discovery").WithCause(err)
			}
			var d daemon.Discovery
			if err := json.Unmarshal(raw, &d); err != nil {
				return errcodes.New(errcodes.Internal, "parse discovery").WithCause(err)
			}
			alive := syscall.Kill(d.Pid, 0) == nil
			if statusJSON {
				return printJSON(cmd, map[string]any{"alive": alive, "discovery": d})
			}
			if !alive {
				return errcodes.Newf(errcodes.DaemonUnreachable, "stale discovery: pid %d is dead", d.Pid).
					WithHint("the supervisor should restart it; check `launchctl print gui/$UID/" + launchdLabel + "`")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "running: pid %d · version %s · protocol v%d\nsocket:  %s\n", d.Pid, d.Version, d.ProtocolVersion, d.SocketPath)
			if d.WSPort > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "ws:      "+constants.LoopbackHost+":%d (tokens: %s)\n", d.WSPort, d.TokenPath)
			}
			// The "restart daemon to update" flow (task-m6-upgrade): a version
			// drift between this binary and the running daemon is the ONE
			// moment sessions churn, so it is user-initiated, never automatic.
			if d.Version != version {
				fmt.Fprintf(cmd.OutOrStdout(), "\nUPDATE AVAILABLE: daemon runs %s, this binary is %s.\nSessions churn on daemon restart — when ready:\n  %s daemon install && %s daemon start\n",
					d.Version, version, constants.BinaryName, constants.BinaryName)
			}
			return nil
		},
	}
	status.Flags().BoolVar(&statusJSON, "json", false, "output as JSON")

	root.AddCommand(install, uninstall, start, stop, status)
	return root
}
