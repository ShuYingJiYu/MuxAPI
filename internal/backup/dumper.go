package backup

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

var extraPgDumpPaths = []string{
	"/www/server/pgsql/bin/pg_dump",
	"/usr/lib/postgresql/18/bin/pg_dump",
	"/usr/lib/postgresql/17/bin/pg_dump",
	"/usr/lib/postgresql/16/bin/pg_dump",
	"/usr/lib/postgresql/15/bin/pg_dump",
	"/usr/local/pgsql/bin/pg_dump",
}

// findPgDump returns the first usable pg_dump binary.
func findPgDump() (string, error) {
	if path, err := exec.LookPath("pg_dump"); err == nil {
		return path, nil
	}
	for _, p := range extraPgDumpPaths {
		if path, err := exec.LookPath(p); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("pg_dump not found in PATH or common locations")
}

// Dump runs pg_dump against databaseURL and returns a streaming reader.
// The caller must close the returned ReadCloser; Close waits for the subprocess.
func Dump(ctx context.Context, databaseURL string) (io.ReadCloser, error) {
	pgDump, err := findPgDump()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, pgDump,
		"--format=plain",
		"--no-owner",
		"--no-acl",
		"--clean",
		"--if-exists",
		"--dbname="+databaseURL,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pg_dump: %w", err)
	}
	return &cmdReadCloser{ReadCloser: stdout, cmd: cmd}, nil
}

type cmdReadCloser struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReadCloser) Close() error {
	_ = c.ReadCloser.Close()
	if err := c.cmd.Wait(); err != nil {
		return fmt.Errorf("pg_dump: %w", err)
	}
	return nil
}
