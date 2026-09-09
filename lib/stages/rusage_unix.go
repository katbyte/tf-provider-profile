//go:build unix

package stages

import (
	"os"
	"runtime"
	"syscall"
)

// maxRSSBytes returns the peak resident set size of a finished process in bytes (ru_maxrss is bytes on darwin,
// kilobytes on linux and the BSDs).
func maxRSSBytes(ps *os.ProcessState) int64 {
	if ps == nil {
		return 0
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) //nolint:unconvert // Maxrss is int32 on some platforms
	}
	return int64(ru.Maxrss) * 1024 //nolint:unconvert // Maxrss is int32 on some platforms
}
