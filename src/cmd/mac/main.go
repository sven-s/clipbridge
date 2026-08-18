//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"clipbridge/src/internal/clip"
	"clipbridge/src/internal/config"
	"clipbridge/src/internal/server"

	"fyne.io/systray"
)

const maxMachines = 5

const (
	repoURL   = "https://github.com/sven-s/clipbridge"
	copyright = "© 2026 Sven Sönnichsen — MIT License"
)

// Overridden at build time: -ldflags "-X main.version=v1.2.3". See the Makefile.
var version = "dev"

type machineSlot struct {
	name      string
	mSendText *systray.MenuItem
	mSendFile *systray.MenuItem
	mReceive  *systray.MenuItem
}

var (
	cfg       *config.Config
	srv       *server.Server
	funnelURL string
	transport = "starting…" // human-readable, for the About window
	mStatus   *systray.MenuItem
	slots     [maxMachines]*machineSlot
)

func main() {
	var err error
	cfg, err = config.Load()
	if err != nil {
		log.Fatal(err)
	}
	slotsDir, err := config.SlotsDir()
	if err != nil {
		log.Fatal(err)
	}
	srv = server.New(cfg.Secret, slotsDir)
	systray.Run(onReady, func() {})
}

func onReady() {
	systray.SetIcon(makeIcon(color.RGBA{66, 133, 244, 255}))
	systray.SetTitle("")
	systray.SetTooltip("Clipbridge")

	mStatus = systray.AddMenuItem("Starting…", "")
	mStatus.Disable()
	systray.AddSeparator()

	// Pre-allocate machine slots (hidden until a machine registers)
	for i := 0; i < maxMachines; i++ {
		st := &machineSlot{
			mSendText: systray.AddMenuItem("Send Text →", ""),
			mSendFile: systray.AddMenuItem("Send File →", ""),
			mReceive:  systray.AddMenuItem("Receive from", ""),
		}
		st.mSendText.Hide()
		st.mSendFile.Hide()
		st.mReceive.Hide()
		slots[i] = st

		go func(s *machineSlot) {
			for {
				select {
				case <-s.mSendText.ClickedCh:
					if s.name != "" {
						go doSendText(s.name)
					}
				case <-s.mSendFile.ClickedCh:
					if s.name != "" {
						go doSendFile(s.name)
					}
				case <-s.mReceive.ClickedCh:
					if s.name != "" {
						go doReceive(s)
					}
				}
			}
		}(st)
	}

	systray.AddSeparator()
	mUIURL := systray.AddMenuItem("UI: (starting…)", "Click to copy full URL")
	systray.AddSeparator()
	mAbout := systray.AddMenuItem("About Clipbridge", "Version, paths and license")
	mQuit := systray.AddMenuItem("Quit", "")

	go startServer()
	go setupFunnel()
	go pollMachines()

	go func() {
		for {
			select {
			case <-mUIURL.ClickedCh:
				full := funnelURL + "/?secret=" + cfg.Secret
				clip.WriteText(full)
				setStatus("URL copied!")
			case <-mAbout.ClickedCh:
				go showAbout() // the dialog blocks until dismissed
			case <-mQuit.ClickedCh:
				systray.Quit()
			}
		}
	}()

	// Update UI URL menu item once funnelURL is set
	go func() {
		for range time.Tick(2 * time.Second) {
			if funnelURL != "" {
				mUIURL.SetTitle("Copy UI URL")
				return
			}
		}
	}()
}

func startServer() {
	addr := fmt.Sprintf(":%d", cfg.ServerPort)
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
	log.Printf("server listening on %s", addr)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Printf("server error: %v", err)
		setStatus("Server error: " + err.Error())
	}
}

func findTailscale() string {
	candidates := []string{
		"/opt/homebrew/bin/tailscale",
		"/usr/local/bin/tailscale",
		"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	return ""
}

func setupFunnel() {
	// An explicitly configured public URL wins: the machine fronting Clipbridge
	// may be behind something other than Funnel (Cloudflare Tunnel, a reverse
	// proxy). Nothing to set up in that case — the tunnel runs on its own.
	if u := strings.TrimRight(cfg.PublicURL, "/"); u != "" {
		funnelURL = u
		transport = "configured public_url (external tunnel)"
		setStatus("Ready")
		return
	}

	tsBin := findTailscale()
	if tsBin == "" {
		funnelURL = fmt.Sprintf("http://localhost:%d", cfg.ServerPort)
		transport = "local only (Tailscale CLI not found)"
		setStatus("Ready (Tailscale CLI not found)")
		return
	}
	statusOut, err := exec.Command(tsBin, "status", "--json").Output()
	if err != nil {
		funnelURL = fmt.Sprintf("http://localhost:%d", cfg.ServerPort)
		transport = "local only (Tailscale not running)"
		setStatus("Ready (no Tailscale)")
		return
	}
	s := string(statusOut)
	dnsName := ""
	if idx := strings.Index(s, `"DNSName"`); idx >= 0 {
		rest := strings.TrimSpace(s[idx+10:])
		rest = strings.TrimPrefix(rest, `"`)
		if end := strings.IndexByte(rest, '"'); end > 0 {
			dnsName = strings.TrimSuffix(rest[:end], ".")
		}
	}
	if dnsName == "" {
		funnelURL = fmt.Sprintf("http://localhost:%d", cfg.ServerPort)
		transport = "local only (no tailnet hostname)"
		setStatus("Ready")
		return
	}
	funnelURL = fmt.Sprintf("https://%s", dnsName)
	transport = "Tailscale Funnel"
	portStr := fmt.Sprintf("%d", cfg.ServerPort)
	if out, err := exec.Command(tsBin, "funnel", "--bg", portStr).CombinedOutput(); err != nil {
		log.Printf("tailscale funnel: %v — %s", err, out)
	}
	setStatus("Ready")
}

// showAbout opens a native dialog with version, paths and copyright.
//
// fyne.io/systray has no window of its own and the app deliberately carries no
// GUI toolkit, so the "window" is an osascript dialog. The details text is
// passed as an argv item rather than interpolated into the script — paths and
// machine names would otherwise need AppleScript quoting.
func showAbout() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "~"
	}
	machines := srv.GetMachines()
	connected := "none"
	if len(machines) > 0 {
		connected = strings.Join(machines, ", ")
	}

	details := fmt.Sprintf(`Clipbridge %s
Cross-machine clipboard relay.

Public URL:   %s
Transport:    %s
Local server: http://localhost:%d
Config:       %s
Slots:        %s
Machines:     %s

%s
%s`,
		version,
		funnelURL,
		transport,
		cfg.ServerPort,
		filepath.Join(home, ".clipbridge", "config.json"),
		filepath.Join(home, ".clipbridge", "slots"),
		connected,
		copyright,
		repoURL,
	)

	const script = `on run argv
display dialog (item 1 of argv) with title "About Clipbridge" buttons {"Copy Details", "GitHub", "OK"} default button "OK" with icon note
end run`

	out, err := exec.Command("osascript", "-e", script, details).Output()
	if err != nil {
		log.Printf("about dialog: %v", err)
		return
	}
	switch {
	case strings.Contains(string(out), "Copy Details"):
		clip.WriteText(details)
		setStatus("About details copied!")
	case strings.Contains(string(out), "GitHub"):
		if err := exec.Command("open", repoURL).Run(); err != nil {
			log.Printf("open %s: %v", repoURL, err)
		}
	}
}

func pollMachines() {
	for range time.Tick(3 * time.Second) {
		machines := srv.GetMachines()

		// Update slot visibility
		for i := 0; i < maxMachines; i++ {
			if i < len(machines) {
				name := machines[i]
				slots[i].name = name
				slots[i].mSendText.SetTitle("Send Text → " + name)
				slots[i].mSendFile.SetTitle("Send File → " + name)
				slots[i].mSendText.Show()
				slots[i].mSendFile.Show()

				// Check if this machine has sent something
				fromDir := "from-" + name
				meta := srv.PeekMeta(fromDir)
				if meta != nil {
					label := ""
					if meta.Type == "file" {
						label = fmt.Sprintf("Receive from %s: %s (%.1f MB)", name, meta.Filename, float64(meta.Size)/1e6)
					} else {
						label = fmt.Sprintf("Receive from %s: text (%d B)", name, meta.Size)
					}
					slots[i].mReceive.SetTitle(label)
					slots[i].mReceive.Enable()
					slots[i].mReceive.Show()
				} else {
					slots[i].mReceive.SetTitle("Receive from " + name)
					slots[i].mReceive.Disable()
					slots[i].mReceive.Show()
				}
			} else {
				slots[i].name = ""
				slots[i].mSendText.Hide()
				slots[i].mSendFile.Hide()
				slots[i].mReceive.Hide()
			}
		}

		// Show dot if any machine has something waiting
		anyWaiting := false
		for _, m := range machines {
			if srv.PeekMeta("from-"+m) != nil {
				anyWaiting = true
				break
			}
		}
		if anyWaiting {
			systray.SetTitle("●")
		} else {
			systray.SetTitle("")
		}
	}
}

func doSendText(machineName string) {
	text, err := clip.ReadText()
	if err != nil || text == "" {
		setStatus("No text in clipboard")
		return
	}
	setStatus("Sending text to " + machineName + "…")
	if err := srv.PutText("to-"+machineName, text); err != nil {
		setStatus("Error: " + err.Error())
		return
	}
	setStatus("Text sent to " + machineName + " ✓")
}

func doSendFile(machineName string) {
	paths, err := clip.ReadFilePaths()
	if err != nil || len(paths) == 0 {
		setStatus("No file in clipboard (copy a file in Finder first)")
		return
	}
	if len(paths) == 1 {
		setStatus("Sending file to " + machineName + "…")
		if err := srv.PutFile("to-"+machineName, paths[0]); err != nil {
			setStatus("Error: " + err.Error())
			return
		}
		setStatus("File sent to " + machineName + " ✓")
		return
	}
	setStatus(fmt.Sprintf("Zipping %d files for %s…", len(paths), machineName))
	zipName := fmt.Sprintf("clipbridge-%d-files-%d.zip", len(paths), time.Now().Unix())
	if err := srv.PutZip("to-"+machineName, zipName, paths); err != nil {
		setStatus("Error: " + err.Error())
		return
	}
	setStatus(fmt.Sprintf("%d files sent as zip to %s ✓", len(paths), machineName))
}

func doReceive(s *machineSlot) {
	fromDir := "from-" + s.name
	meta := srv.PeekMeta(fromDir)
	if meta == nil {
		setStatus("Nothing to receive from " + s.name)
		return
	}
	if meta.Type == "text" {
		text, err := srv.ReadText(fromDir)
		if err != nil {
			setStatus("Error: " + err.Error())
			return
		}
		clip.WriteText(text)
		srv.ClearSlot(fromDir)
		setStatus("Text from " + s.name + " → clipboard ✓")
	} else {
		path, err := srv.CopyFile(fromDir, downloadsDir())
		if err != nil {
			setStatus("Error: " + err.Error())
			return
		}
		srv.ClearSlot(fromDir)
		exec.Command("open", "-R", path).Start()
		setStatus("File from " + s.name + " → Downloads ✓")
	}
}

func setStatus(msg string) { mStatus.SetTitle(msg) }

func downloadsDir() string {
	home, _ := exec.Command("sh", "-c", "echo $HOME").Output()
	return strings.TrimSpace(string(home)) + "/Downloads"
}

func makeIcon(c color.RGBA) []byte {
	size := 22
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	cx, cy := float64(size)/2, float64(size)/2
	r := float64(size)/2 - 1
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if dx*dx+dy*dy <= r*r {
				img.SetRGBA(x, y, c)
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}
