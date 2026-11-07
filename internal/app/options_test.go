package app

import (
	"strings"
	"testing"
)

func TestValidateFlags(t *testing.T) {
	tests := []struct {
		name    string
		list    bool
		verify  bool
		clean   bool
		dryRun  bool
		noVerif bool
		recheck bool
		want    string
	}{
		{name: "all clear"},
		{name: "recheck without no-verify", recheck: true},
		{name: "recheck with no-verify", noVerif: true, recheck: true, want: "--recheck"},
		{name: "verify-only with no-verify", verify: true, noVerif: true, want: "--verify-only"},
		{name: "verify-only with dry-run", verify: true, dryRun: true, want: "--verify-only"},
		{name: "clean-temp with dry-run", clean: true, dryRun: true, want: "--clean-temp"},
		{name: "list with clean-temp", list: true, clean: true, want: "--list"},
		{name: "list with verify-only", list: true, verify: true, want: "--list"},
	}
	for _, tc := range tests {
		err := ValidateFlags(tc.list, tc.verify, tc.clean, tc.dryRun, tc.noVerif, tc.recheck)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestOptionsValidateUsesFlags(t *testing.T) {
	o := Options{NoVerify: true, Recheck: true}
	if err := o.Validate(); err == nil {
		t.Fatal("expected --no-verify with --recheck to be rejected")
	}
}
