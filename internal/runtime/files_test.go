package runtime

import "testing"

func TestResolveUnderRoot(t *testing.T) {
	tests := []struct {
		rel     string
		want    string
		wantErr bool
	}{
		{rel: ".", want: "/app"},
		{rel: "", wantErr: true},
		{rel: "foo.txt", want: "/app/foo.txt"},
		{rel: "dir/a.txt", want: "/app/dir/a.txt"},
		{rel: "/foo", want: "/app/foo"},
		{rel: "../etc/passwd", wantErr: true},
		{rel: "foo/../../etc", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ResolveUnderRoot(DefaultFilesRoot, tc.rel)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ResolveUnderRoot(%q) error = nil, want error", tc.rel)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ResolveUnderRoot(%q) error = %v", tc.rel, err)
		}
		if got != tc.want {
			t.Fatalf("ResolveUnderRoot(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}

func TestValidateWriteName(t *testing.T) {
	if err := ValidateWriteName("ok.txt"); err != nil {
		t.Fatalf("ValidateWriteName(ok.txt) = %v", err)
	}
	for _, name := range []string{"", ".", "..", "dir/a.txt", "/a.txt", `a\b`} {
		if err := ValidateWriteName(name); err == nil {
			t.Fatalf("ValidateWriteName(%q) = nil, want error", name)
		}
	}
}
