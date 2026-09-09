//go:build windows

package stages

import "os"

// maxRSSBytes is not available via rusage on windows; timing stages report 0 there.
func maxRSSBytes(_ *os.ProcessState) int64 { return 0 }
