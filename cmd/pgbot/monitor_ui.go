package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pgrundev/pgbot/internal/render"
	"golang.org/x/term"
)

// errAborted is returned when the user cancels the wizard (Ctrl-C, Ctrl-D, or
// answering "no" to a gate that can't be skipped).
var errAborted = errors.New("setup cancelled — nothing was changed")

// ui is the monitor's small prompt toolkit: line input, yes/no, hidden input,
// and an arrow-key menu with a numeric fallback. It deliberately avoids a TUI
// framework — lipgloss (already a dependency) supplies the colors.
type ui struct {
	in    *os.File
	r     *bufio.Reader
	out   io.Writer
	st    render.Styler
	color bool
	tty   bool // stdin AND stdout are terminals
}

func newUI() *ui {
	color := useColor(false)
	return &ui{
		in:    os.Stdin,
		r:     bufio.NewReader(os.Stdin),
		out:   os.Stdout,
		st:    render.NewStyler(color),
		color: color,
		tty:   term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())),
	}
}

func (u *ui) printf(format string, a ...any) { fmt.Fprintf(u.out, format, a...) }
func (u *ui) println(a ...any)               { fmt.Fprintln(u.out, a...) }

func (u *ui) ok(s string)   { u.println(u.st.Good("✓") + " " + s) }
func (u *ui) bad(s string)  { u.println(u.st.Crit("✗") + " " + s) }
func (u *ui) warn(s string) { u.println(u.st.Warn("!") + " " + s) }
func (u *ui) dim(s string)  { u.println(u.st.Dim(s)) }

func (u *ui) rule() { u.println(u.st.Dim(strings.Repeat("─", 33))) }

func (u *ui) header(title string) {
	u.println()
	u.println(u.st.Head("🐘 " + title))
	u.rule()
	u.println()
}

// kv prints an aligned "Key   value" row.
func (u *ui) kv(k, v string) { u.printf("  %-15s %s\n", k, v) }

// line reads one line, returning def on an empty answer.
func (u *ui) line(prompt, def string) (string, error) {
	if def != "" {
		u.printf("%s %s ", u.st.Head("❯"), prompt+u.st.Dim(" ["+def+"]"))
	} else {
		u.printf("%s %s ", u.st.Head("❯"), prompt)
	}
	s, err := u.r.ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		u.println()
		return "", errAborted
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return s, nil
}

// confirm asks a yes/no question; Enter takes def.
func (u *ui) confirm(q string, def bool) (bool, error) {
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	for {
		u.printf("%s %s ", q, u.st.Dim(hint))
		s, err := u.r.ReadString('\n')
		if err != nil && s == "" {
			u.println()
			return false, errAborted
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		u.dim("  Please answer y or n.")
	}
}

// secret reads a line without echo — connection URLs may carry a password.
func (u *ui) secret(prompt string) (string, error) {
	u.printf("%s %s\n%s ", prompt, u.st.Dim("(input is hidden)"), u.st.Head("❯"))
	if !term.IsTerminal(int(u.in.Fd())) {
		return "", errors.New("hidden input needs a terminal")
	}
	b, err := term.ReadPassword(int(u.in.Fd()))
	u.println()
	if err != nil {
		return "", errAborted
	}
	return strings.TrimSpace(string(b)), nil
}

// choose shows a menu and returns the chosen index. Arrow keys / j k / digits
// when the terminal supports raw mode; otherwise a numbered list.
func (u *ui) choose(q string, opts []string, def int) (int, error) {
	u.println(u.st.Head(q))
	u.println()
	if u.tty && os.Getenv("TERM") != "dumb" {
		if i, err := u.chooseRaw(opts, def); err == nil || errors.Is(err, errAborted) {
			u.println()
			return i, err
		}
	}
	for i, o := range opts {
		u.printf("  %d. %s\n", i+1, o)
	}
	for {
		s, err := u.line("Choose 1–"+strconv.Itoa(len(opts)), strconv.Itoa(def+1))
		if err != nil {
			return 0, err
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(opts) {
			u.println()
			return n - 1, nil
		}
		u.dim("  Please enter a number from the list.")
	}
}

var errNoRaw = errors.New("raw mode unavailable")

func (u *ui) chooseRaw(opts []string, cur int) (int, error) {
	fd := int(u.in.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return 0, errNoRaw
	}
	defer func() { _ = term.Restore(fd, old) }()

	draw := func(first bool) {
		if !first {
			fmt.Fprintf(u.out, "\x1b[%dA", len(opts))
		}
		for i, o := range opts {
			line := fmt.Sprintf("    %d. %s", i+1, o)
			if i == cur {
				line = u.st.Head("  ❯ ") + u.st.Good(fmt.Sprintf("%d. %s", i+1, o))
			}
			fmt.Fprintf(u.out, "\r\x1b[2K%s\r\n", line)
		}
	}
	fmt.Fprint(u.out, "\x1b[?25l") // hide cursor while the menu is live
	defer fmt.Fprint(u.out, "\x1b[?25h")
	draw(true)
	fmt.Fprint(u.out, u.st.Dim("  ↑/↓ to move · Enter to select")+"\r")
	buf := make([]byte, 8)
	for {
		n, err := u.in.Read(buf)
		if err != nil || n == 0 {
			return 0, errAborted
		}
		b := buf[:n]
		switch {
		case b[0] == 3 || b[0] == 4 || (n == 1 && b[0] == 27): // Ctrl-C, Ctrl-D, Esc
			fmt.Fprint(u.out, "\r\x1b[2K")
			return 0, errAborted
		case b[0] == '\r' || b[0] == '\n':
			fmt.Fprint(u.out, "\r\x1b[2K")
			return cur, nil
		case n >= 3 && b[0] == 27 && b[1] == '[' && b[2] == 'A', b[0] == 'k':
			cur = (cur - 1 + len(opts)) % len(opts)
		case n >= 3 && b[0] == 27 && b[1] == '[' && b[2] == 'B', b[0] == 'j':
			cur = (cur + 1) % len(opts)
		case b[0] >= '1' && b[0] <= '9' && int(b[0]-'1') < len(opts):
			cur = int(b[0] - '1')
		default:
			continue
		}
		fmt.Fprint(u.out, "\r\x1b[2K")
		draw(false)
		fmt.Fprint(u.out, u.st.Dim("  ↑/↓ to move · Enter to select")+"\r")
	}
}

// spin shows a spinner with msg while fn runs (plain line off a TTY).
func (u *ui) spin(msg string, fn func() error) error {
	if !u.tty || !u.color {
		u.dim("… " + msg)
		return fn()
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		t := time.NewTicker(90 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			fmt.Fprintf(u.out, "\r%s %s", u.st.Head(frames[i%len(frames)]), u.st.Dim(msg))
			select {
			case <-done:
				fmt.Fprint(u.out, "\r\x1b[2K")
				return
			case <-t.C:
			}
		}
	}()
	err := fn()
	close(done)
	wg.Wait()
	return err
}
