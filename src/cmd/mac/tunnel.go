//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"clipbridge/src/internal/config"
	"clipbridge/src/internal/server"

	"fyne.io/systray"
)

// The Cloudflare tunnel is not ours: cloudflared runs as its own LaunchAgent so
// it survives app restarts. This file only finds it, reports on it and pokes it.
// Setup lives in scripts/setup-tunnel.sh, bundled into Contents/Resources.

const setupGuideURL = repoURL + "#cloudflare-tunnel-setup"

// tunnelAgent is what we learn from the LaunchAgent plist.
type tunnelAgent struct {
	label      string
	plistPath  string
	configPath string // cloudflared --config, if given
	logPath    string
}

// findTunnelAgent locates the LaunchAgent by file name rather than a fixed
// label, so installs done by hand from the README (any reverse-DNS prefix)
// are picked up as well as ones written by setup-tunnel.sh.
func findTunnelAgent() *tunnelAgent {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(home, "Library", "LaunchAgents", "*clipbridge-tunnel.plist"))
	if len(matches) == 0 {
		return nil
	}
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", matches[0]).Output()
	if err != nil {
		return nil
	}
	var p struct {
		Label            string   `json:"Label"`
		ProgramArguments []string `json:"ProgramArguments"`
		StandardOutPath  string   `json:"StandardOutPath"`
		StandardErrPath  string   `json:"StandardErrorPath"`
	}
	if json.Unmarshal(out, &p) != nil || p.Label == "" {
		return nil
	}
	a := &tunnelAgent{label: p.Label, plistPath: matches[0], logPath: p.StandardErrPath}
	if a.logPath == "" {
		a.logPath = p.StandardOutPath
	}
	for i, arg := range p.ProgramArguments {
		if arg == "--config" && i+1 < len(p.ProgramArguments) {
			a.configPath = p.ProgramArguments[i+1]
		}
	}
	if a.configPath == "" {
		a.configPath = filepath.Join(home, ".cloudflared", "config.yml")
	}
	return a
}

func launchdTarget(label string) string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
}

// agentState reports launchd's view: not loaded, loaded but not running, or running.
func agentState(a *tunnelAgent) (loaded, running bool, pid string) {
	out, err := exec.Command("launchctl", "print", launchdTarget(a.label)).Output()
	if err != nil {
		return false, false, ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// Top-level keys only: nested endpoint blocks have their own "state = active".
		if line == "state = running" {
			running = true
		}
		if strings.HasPrefix(line, "pid = ") && pid == "" {
			pid = strings.TrimPrefix(line, "pid = ")
		}
	}
	return true, running, pid
}

// checkPublic hits /healthz through the public hostname. Unlike Funnel, a
// Cloudflare hostname resolves to Cloudflare's edge from the Mac too, so this
// really exercises the public path. It cannot see a corporate proxy block.
func checkPublic(base string) error {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(strings.TrimRight(base, "/") + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if string(body) != server.HealthBody {
		return fmt.Errorf("answered, but not by Clipbridge (wrong port in config.yml?)")
	}
	return nil
}

type tunnelMenu struct {
	root        *systray.MenuItem
	agentLine   *systray.MenuItem
	publicLine  *systray.MenuItem
	restart     *systray.MenuItem
	openLog     *systray.MenuItem
	editTunnel  *systray.MenuItem
	editConfig  *systray.MenuItem
	restartSelf *systray.MenuItem
	setup       *systray.MenuItem
	guide       *systray.MenuItem
	refresh     chan struct{}
}

func addTunnelMenu() {
	m := &tunnelMenu{refresh: make(chan struct{}, 1)}
	m.root = systray.AddMenuItem("Tunnel: checking…", "Public tunnel status and controls")
	m.agentLine = m.root.AddSubMenuItem("", "")
	m.agentLine.Disable()
	m.publicLine = m.root.AddSubMenuItem("", "")
	m.publicLine.Disable()
	m.restart = m.root.AddSubMenuItem("Restart Tunnel", "launchctl kickstart the cloudflared LaunchAgent")
	m.openLog = m.root.AddSubMenuItem("Open Tunnel Log", "")
	m.editTunnel = m.root.AddSubMenuItem("Edit Tunnel Config (config.yml)", "")
	m.editConfig = m.root.AddSubMenuItem("Edit Clipbridge Config (config.json)", "")
	m.restartSelf = m.root.AddSubMenuItem("Restart Clipbridge", "Needed after editing config.json")
	m.setup = m.root.AddSubMenuItem("Set Up Cloudflare Tunnel…", "Runs the guided setup in Terminal; safe to re-run to repair")
	m.guide = m.root.AddSubMenuItem("Setup Guide (README)", "")

	go m.statusLoop()
	go m.clicks()
}

func (m *tunnelMenu) kick() {
	select {
	case m.refresh <- struct{}{}:
	default:
	}
}

func (m *tunnelMenu) statusLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		m.update()
		select {
		case <-t.C:
		case <-m.refresh:
		}
	}
}

func (m *tunnelMenu) update() {
	a := findTunnelAgent()
	public := strings.TrimRight(cfg.PublicURL, "/")

	agentOK := true
	if a == nil {
		m.agentLine.SetTitle("cloudflared: no LaunchAgent installed")
		for _, it := range []*systray.MenuItem{m.restart, m.openLog, m.editTunnel} {
			it.Hide()
		}
	} else {
		for _, it := range []*systray.MenuItem{m.restart, m.openLog, m.editTunnel} {
			it.Show()
		}
		loaded, running, pid := agentState(a)
		switch {
		case !loaded:
			m.agentLine.SetTitle("cloudflared: not loaded (" + a.label + ")")
			m.restart.SetTitle("Start Tunnel")
			agentOK = false
		case !running:
			m.agentLine.SetTitle("cloudflared: loaded, not running")
			m.restart.SetTitle("Start Tunnel")
			agentOK = false
		default:
			m.agentLine.SetTitle("cloudflared: running (pid " + pid + ")")
			m.restart.SetTitle("Restart Tunnel")
		}
	}

	if public == "" {
		m.publicLine.SetTitle("public_url not set — using Tailscale Funnel")
		m.root.SetTitle("Tunnel: Tailscale Funnel")
		return
	}
	host := strings.TrimPrefix(strings.TrimPrefix(public, "https://"), "http://")
	if err := checkPublic(public); err != nil {
		m.publicLine.SetTitle(host + ": " + err.Error())
		m.root.SetTitle("Tunnel: ⚠ " + host + " unreachable")
		return
	}
	m.publicLine.SetTitle(host + ": reachable")
	if !agentOK {
		// Reachable without our agent: someone else runs the tunnel. Fine.
		m.root.SetTitle("Tunnel: ✓ " + host + " (external)")
		return
	}
	m.root.SetTitle("Tunnel: ✓ " + host)
}

func (m *tunnelMenu) clicks() {
	for {
		select {
		case <-m.restart.ClickedCh:
			go m.restartAgent()
		case <-m.openLog.ClickedCh:
			if a := findTunnelAgent(); a != nil && a.logPath != "" {
				run("open", "-a", "Console", a.logPath)
			}
		case <-m.editTunnel.ClickedCh:
			if a := findTunnelAgent(); a != nil {
				run("open", "-t", a.configPath)
			}
		case <-m.editConfig.ClickedCh:
			if p, err := config.Path(); err == nil {
				run("open", "-t", p)
			}
		case <-m.restartSelf.ClickedCh:
			restartSelf()
		case <-m.setup.ClickedCh:
			go runSetupScript()
		case <-m.guide.ClickedCh:
			run("open", setupGuideURL)
		}
	}
}

func (m *tunnelMenu) restartAgent() {
	a := findTunnelAgent()
	if a == nil {
		return
	}
	target := launchdTarget(a.label)
	var out []byte
	var err error
	if loaded, _, _ := agentState(a); loaded {
		out, err = exec.Command("launchctl", "kickstart", "-k", target).CombinedOutput()
	} else {
		out, err = exec.Command("launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), a.plistPath).CombinedOutput()
	}
	if err != nil {
		setStatus("Tunnel: " + strings.TrimSpace(string(out)))
		log.Printf("tunnel restart: %v — %s", err, out)
	} else {
		setStatus("Tunnel restarted")
	}
	// cloudflared needs a few seconds to register its connections.
	time.Sleep(5 * time.Second)
	m.kick()
}

// setupScriptPath finds setup-tunnel.sh: inside the .app bundle, or next to
// the repo when running a bare `make run` binary from build/.
func setupScriptPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	for _, p := range []string{
		filepath.Join(dir, "..", "Resources", "setup-tunnel.sh"),
		filepath.Join(dir, "..", "scripts", "setup-tunnel.sh"),
	} {
		if _, err := os.Stat(p); err == nil {
			return filepath.Clean(p)
		}
	}
	return ""
}

func runSetupScript() {
	p := setupScriptPath()
	if p == "" {
		setStatus("Setup script not found — opening guide")
		run("open", setupGuideURL)
		return
	}
	// Path passed as argv, quoted by AppleScript itself — no string splicing.
	const script = `on run argv
tell application "Terminal"
	activate
	do script "/bin/bash " & quoted form of (item 1 of argv)
end tell
end run`
	if out, err := exec.Command("osascript", "-e", script, p).CombinedOutput(); err != nil {
		log.Printf("setup script: %v — %s", err, out)
		setStatus("Could not open Terminal")
	}
}

// restartSelf relaunches the app so config.json changes take effect. A
// detached shell waits for this process to release :8457, then starts a new one.
func restartSelf() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	var cmd *exec.Cmd
	if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 {
		cmd = exec.Command("/bin/sh", "-c", `sleep 1; open -n "$0"`, exe[:i+len(".app")])
	} else {
		cmd = exec.Command("/bin/sh", "-c", `sleep 1; exec "$0"`, exe)
	}
	if err := cmd.Start(); err != nil {
		setStatus("Restart failed: " + err.Error())
		return
	}
	systray.Quit()
}

func run(name string, args ...string) {
	if err := exec.Command(name, args...).Start(); err != nil {
		log.Printf("%s %v: %v", name, args, err)
	}
}
