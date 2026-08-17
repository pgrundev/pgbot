package main

import (
	"context"
	"fmt"
	"os"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/store"
)

// gather runs the full read-only collection and returns a computed Context plus
// the target host (for the header). It closes the connection before returning —
// findings and rendering need no live connection. Shared by `ask` and `indexes`;
// `inspect`/`explain` keep their own flow because they also render/exit.
func gather(ctx context.Context, connString string, f inspectFlags) (*model.Context, string, error) {
	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return nil, "", fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
	}
	defer target.Close()

	if target.Pooler.Detected {
		if f.strictPooler {
			fmt.Fprintln(os.Stderr, target.Pooler.StrictMessage())
			os.Exit(exitFailure)
		}
		fmt.Fprintln(os.Stderr, target.Pooler.Note())
	}

	c, err := collect.Run(ctx, target, collect.Options{Interval: f.interval, ASHHz: f.ashHz, ASHWindow: f.window})
	if err != nil {
		return nil, "", fmt.Errorf("collect: %s", conn.RedactConnString(err.Error()))
	}
	c.Server.ViaPooler = target.Pooler.Detected
	host, port := hostPort(target)
	c.Fingerprint = store.Fingerprint(host, port, c.Server.Database, target.Caps.SystemIdentifier)

	if !f.noStore {
		withStore(f.storePath, c)
	}
	if err := computeFindings(c, f); err != nil {
		return nil, "", err
	}
	return c, host, nil
}
