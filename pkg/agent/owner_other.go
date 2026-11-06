//go:build !unix

package agent

import "os"

// fileOwner is not available off Unix; ownership is then not compared.
func fileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
