package hlexchange

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Persist the observed projection floor before accepting terminal execution.
// Restart must not quote against a node predating a confirmed action.
func (c *Client) observeHeight(height uint64) error {
	if height > c.LastExecutionHeight {
		c.LastExecutionHeight = height
	}
	if !c.RequireExecutionHeight || c.StateDir == "" {
		return nil
	}
	path := filepath.Join(c.StateDir, c.journalIdentity()+".height")
	if data, e := os.ReadFile(path); e == nil {
		previous, e := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if e != nil {
			return fmt.Errorf("invalid execution height journal: %w", e)
		}
		if previous > c.LastExecutionHeight {
			c.LastExecutionHeight = previous
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if height == 0 {
		return nil
	}
	f, e := os.CreateTemp(c.StateDir, ".height-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = fmt.Fprintf(f, "%d\n", c.LastExecutionHeight)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return syncDirectory(c.StateDir)
}
func (c *Client) RestoreExecutionHeight() error { return c.observeHeight(0) }
