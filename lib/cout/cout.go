// Package cout provides verbosity-levelled, coloured console output.
package cout

import (
	"os"

	c "github.com/gookit/color"
)

// Verbosity levels (ordered from least to most output)
const (
	VerbositySilent = iota
	VerbosityQuiet
	VerbosityNormal
	VerbosityVerbose
)

// Level controls the output verbosity. Set before any output calls.
var Level = VerbosityNormal

// Printf prints normal output with color support (suppressed in quiet and silent modes)
func Printf(format string, args ...any) {
	if Level < VerbosityNormal {
		return
	}
	c.Printf(format, args...)
}

// Println prints normal output (suppressed in quiet and silent modes)
func Println(args ...any) {
	if Level < VerbosityNormal {
		return
	}
	c.Println(args...)
}

// Errorf prints an error to stderr in every mode except silent.
func Errorf(format string, args ...any) {
	if Level == VerbositySilent {
		return
	}
	c.Fprintf(os.Stderr, format, args...)
}

// Verbosef prints detailed output only when -v is set.
func Verbosef(format string, args ...any) {
	if Level < VerbosityVerbose {
		return
	}
	c.Printf(format, args...)
}
