package main

import (
	"os"
	"strconv"

	"github.com/pgrundev/pgbot/internal/conn"
	"golang.org/x/term"
)

func argAt(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// pgServiceFallback lets a bare $PGSERVICE select a connection when neither an
// argument nor $DATABASE_URL/$PGBOT_DATABASE_URL is set. pggo.ParseConfig
// already reads a connection service file (PGSERVICEFILE, or the libpq
// default path) once it gets a "service=..." string — this just builds that
// string so users who manage connections through a service file don't have
// to also pass one explicitly.
func pgServiceFallback() string {
	if svc := os.Getenv("PGSERVICE"); svc != "" {
		return "service=" + svc
	}
	return ""
}

// isInteractive reports whether stdin is a terminal — used to decide whether to
// prompt for confirmation (skip the prompt when piped/scripted).
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// useColor honors --no-color, the NO_COLOR convention, and a non-TTY stdout.
func useColor(noColorFlag bool) bool {
	if noColorFlag || os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// terminalWidth returns the stdout width, or 100 when not a TTY (piped output).
// The renderer clamps to a minimum of 80.
func terminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	// Not a TTY (piped/redirected): honor COLUMNS if the caller set it, else 100.
	if c := os.Getenv("COLUMNS"); c != "" {
		if w, err := strconv.Atoi(c); err == nil && w > 0 {
			return w
		}
	}
	return 100
}

// hostPort pulls the host/port off the pool's config for the baseline
// fingerprint fallback (used only when the system identifier isn't readable).
func hostPort(t *conn.Target) (string, string) {
	cfg := t.Pool.Config()
	port := "5432"
	if cfg.Port != 0 {
		port = strconv.Itoa(cfg.Port)
	}
	return cfg.Host, port
}
