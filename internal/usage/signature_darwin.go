package usage

import (
	"fmt"
	"os"
	"syscall"
)

// fileSignature changes on any write or replacement, including same-size rewrites
// that restore the modification time.
func fileSignature(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", st.Dev, st.Ino, st.Size, st.Mtimespec.Nano(), st.Ctimespec.Nano())
}
