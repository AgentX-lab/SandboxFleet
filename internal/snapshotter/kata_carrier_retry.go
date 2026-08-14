package snapshotter

import (
	"errors"
	"io/fs"
	"strings"
)

// kataErrLooksLikeENOENT reports agent/rpc errors that mean a guest path is not
// visible yet (virtiofs submount race), as opposed to permanent failures.
func kataErrLooksLikeENOENT(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "enoent") || strings.Contains(s, "no such file or directory")
}
