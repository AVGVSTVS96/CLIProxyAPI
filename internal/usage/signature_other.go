//go:build !linux && !darwin

package usage

import (
	"fmt"
	"os"
)

func fileSignature(info os.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}
