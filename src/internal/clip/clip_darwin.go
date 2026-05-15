//go:build darwin

package clip

import (
	"os"
	"os/exec"
	"strings"
)

func ReadText() (string, error) {
	out, err := exec.Command("pbpaste").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func WriteText(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

// ReadFilePaths returns the POSIX paths of all files in the clipboard,
// or nil if the clipboard doesn't contain files.
func ReadFilePaths() ([]string, error) {
	script := `use framework "AppKit"
use scripting additions
set pb to current application's NSPasteboard's generalPasteboard()
set theFiles to pb's propertyListForType:"NSFilenamesPboardType"
if theFiles is not missing value then
	set fileList to theFiles as list
	set AppleScript's text item delimiters to (ASCII character 0)
	set theResult to fileList as text
	set AppleScript's text item delimiters to ""
	return theResult
end if
return ""`
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return nil, nil
	}
	raw := strings.TrimRight(string(out), "\n")
	if raw == "" {
		return nil, nil
	}
	var paths []string
	for _, p := range strings.Split(raw, "\x00") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
