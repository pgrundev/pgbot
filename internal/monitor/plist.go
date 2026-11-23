package monitor

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AgentSpec is everything that goes into the LaunchAgent plist. It never
// contains a credential: the program path, a fixed argument list, a log path,
// and the non-secret environment from Config.Env.
type AgentSpec struct {
	Label    string
	Program  string
	Schedule Schedule
	Log      string
	Env      map[string]string
}

// Plist renders a user LaunchAgent:
//
//   - ProgramArguments is [pgbot, monitor, _run] — no connection string, ever.
//   - hourly / every 6 hours → StartInterval; daily / weekly →
//     StartCalendarInterval (wall-clock, so DST keeps 9:00 AM at 9:00 AM).
//     launchd coalesces calendar runs missed while the Mac slept into one run
//     on wake; StartInterval firings missed during sleep are skipped. Nothing
//     runs while the user is logged out (gui/<uid> domain).
//   - RunAtLoad is false: logging in doesn't trigger a check by itself.
//   - ProcessType Background + LowPriorityIO: a check never competes with the
//     user's foreground work.
func Plist(s AgentSpec) ([]byte, error) {
	if err := s.Schedule.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(s.Program) {
		return nil, fmt.Errorf("program path must be absolute: %q", s.Program)
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	key(&b, "Label")
	b.WriteString("\t")
	str(&b, s.Label)
	key(&b, "ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range []string{s.Program, "monitor", "_run"} {
		b.WriteString("\t\t")
		str(&b, a)
	}
	b.WriteString("\t</array>\n")

	if iv := s.Schedule.Interval(); iv > 0 {
		key(&b, "StartInterval")
		fmt.Fprintf(&b, "\t<integer>%d</integer>\n", int(iv.Seconds()))
	} else {
		key(&b, "StartCalendarInterval")
		b.WriteString("\t<dict>\n")
		b.WriteString("\t\t<key>Hour</key>\n")
		fmt.Fprintf(&b, "\t\t<integer>%d</integer>\n", s.Schedule.Hour)
		b.WriteString("\t\t<key>Minute</key>\n")
		fmt.Fprintf(&b, "\t\t<integer>%d</integer>\n", s.Schedule.Minute)
		if s.Schedule.Kind == Weekly {
			b.WriteString("\t\t<key>Weekday</key>\n")
			fmt.Fprintf(&b, "\t\t<integer>%d</integer>\n", int(s.Schedule.Weekday))
		}
		b.WriteString("\t</dict>\n")
	}

	key(&b, "RunAtLoad")
	b.WriteString("\t<false/>\n")
	key(&b, "ProcessType")
	b.WriteString("\t")
	str(&b, "Background")
	key(&b, "LowPriorityIO")
	b.WriteString("\t<true/>\n")
	if s.Log != "" {
		key(&b, "StandardOutPath")
		b.WriteString("\t")
		str(&b, s.Log)
		key(&b, "StandardErrorPath")
		b.WriteString("\t")
		str(&b, s.Log)
	}

	env := map[string]string{"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "NO_COLOR": "1"}
	for k, v := range s.Env {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	key(&b, "EnvironmentVariables")
	b.WriteString("\t<dict>\n")
	for _, k := range keys {
		b.WriteString("\t\t")
		key(&b, k)
		b.WriteString("\t\t")
		str(&b, env[k])
	}
	b.WriteString("\t</dict>\n")
	b.WriteString("</dict>\n</plist>\n")
	// key() writes a leading tab; normalize the nested ones.
	out := strings.ReplaceAll(b.String(), "\t\t\t<key>", "\t\t<key>")
	return []byte(out), nil
}

func key(b *bytes.Buffer, k string) {
	b.WriteString("\t<key>")
	_ = xml.EscapeText(b, []byte(k))
	b.WriteString("</key>\n")
}

func str(b *bytes.Buffer, v string) {
	b.WriteString("<string>")
	_ = xml.EscapeText(b, []byte(v))
	b.WriteString("</string>\n")
}

// ProgramFromPlist extracts ProgramArguments[0] from a plist this package
// wrote, so `status` can tell when the scheduled binary has moved or been
// replaced by an upgrade at a different path.
func ProgramFromPlist(data []byte) string {
	s := string(data)
	i := strings.Index(s, "<key>ProgramArguments</key>")
	if i < 0 {
		return ""
	}
	s = s[i:]
	a := strings.Index(s, "<string>")
	z := strings.Index(s, "</string>")
	if a < 0 || z < a {
		return ""
	}
	var v struct {
		S string `xml:",chardata"`
	}
	if err := xml.Unmarshal([]byte("<x>"+s[a+len("<string>"):z]+"</x>"), &v); err != nil {
		return ""
	}
	return v.S
}

// EphemeralReason says why an executable path must not be scheduled (it may
// vanish: an npx cache, a `go run` build dir, a temp dir), or "" if it's fine.
func EphemeralReason(path string) string {
	p := filepath.ToSlash(path)
	tmp := filepath.ToSlash(os.TempDir())
	switch {
	case strings.Contains(p, "/_npx/") || strings.Contains(p, "/.npm/"):
		return "it is running from the npx cache, which npm clears"
	case strings.Contains(p, "/go-build"):
		return "it is a temporary `go run` build"
	case tmp != "" && tmp != "/" && strings.HasPrefix(p, strings.TrimSuffix(tmp, "/")+"/"),
		strings.HasPrefix(p, "/tmp/"), strings.HasPrefix(p, "/private/tmp/"),
		strings.HasPrefix(p, "/private/var/folders/"), strings.HasPrefix(p, "/var/folders/"):
		return "it lives in a temporary directory"
	}
	return ""
}
