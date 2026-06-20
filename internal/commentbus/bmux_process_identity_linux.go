//go:build linux

package commentbus

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func bmuxProcessIdentity(pid uint32) (string, error) {
	if pid == 0 {
		return "", ErrTmuxSessionMissing
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrTmuxSessionMissing
		}
		return "", err
	}
	stat := string(data)
	endComm := strings.LastIndex(stat, ")")
	if endComm < 0 || endComm+2 >= len(stat) {
		return "", errors.New("invalid proc stat")
	}
	fields := strings.Fields(stat[endComm+2:])
	if len(fields) < 20 {
		return "", errors.New("invalid proc stat")
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", err
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrTmuxSessionMissing
		}
		return "", err
	}
	boot := strings.TrimSpace(string(bootID))
	if boot == "" || strings.ContainsAny(boot, "\r\n\x00") {
		return "", errors.New("invalid proc boot id")
	}
	return fmt.Sprintf("linux:%s:%d", boot, startTicks), nil
}
