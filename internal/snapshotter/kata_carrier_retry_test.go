package snapshotter

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
)

func TestKataErrLooksLikeENOENT(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "fs ErrNotExist", err: fs.ErrNotExist, want: true},
		{name: "wrapped ErrNotExist", err: fmt.Errorf("stat root: %w", fs.ErrNotExist), want: true},
		{name: "rpc ENOENT", err: errors.New(`CreateContainer: rpc error: code = Internal desc = ENOENT: No such file or directory`), want: true},
		{name: "lowercase no such file", err: errors.New("no such file or directory"), want: true},
		{name: "other", err: errors.New("connection refused"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := kataErrLooksLikeENOENT(tc.err); got != tc.want {
				t.Fatalf("kataErrLooksLikeENOENT(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
