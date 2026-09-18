package main

import "testing"

func TestSnapshotTarget(t *testing.T) {
	cases := []struct {
		arg, revision string
		flagSet       bool
		repo, rev     string
		wantErr       bool
	}{
		{arg: "org/llama", repo: "org/llama"},
		{arg: "org/llama", revision: "v1", repo: "org/llama", rev: "v1"}, // --revision or $OPOD_MODEL_REVISION
		{arg: "org/llama@abc123", repo: "org/llama", rev: "abc123"},
		{arg: "org/llama@abc123", revision: "from-env", repo: "org/llama", rev: "abc123"}, // the argument is the explicit one
		{arg: "org/llama@abc123", revision: "abc123", flagSet: true, repo: "org/llama", rev: "abc123"},
		{arg: "org/llama@abc123", revision: "other", flagSet: true, wantErr: true},
		{arg: "org/llama@", wantErr: true},
		{arg: "@abc123", wantErr: true},
	}
	for _, c := range cases {
		repo, rev, err := snapshotTarget(c.arg, c.revision, c.flagSet)
		if (err != nil) != c.wantErr || repo != c.repo || rev != c.rev {
			t.Errorf("snapshotTarget(%q, %q, %v) = %q, %q, %v; want %q, %q, err=%v", c.arg, c.revision, c.flagSet, repo, rev, err, c.repo, c.rev, c.wantErr)
		}
	}
}
