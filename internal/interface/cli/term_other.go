//go:build !darwin && !linux && !windows

package cli

import "os"

func disableEcho(*os.File) (func(), bool) { return nil, false }
