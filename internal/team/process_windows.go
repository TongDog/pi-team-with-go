//go:build windows

package team

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

func prepareCommand(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		_ = kill.Run()
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 2 * time.Second
}
func acquireLock(dir string) (func(), error) {
	path := filepath.Join(dir, "controller.lockdir")
	if e := os.Mkdir(path, 0700); e != nil {
		return nil, e
	}
	return func() { _ = os.Remove(path) }, nil
}
