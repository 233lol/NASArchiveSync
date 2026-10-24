package storage

import "testing"

func TestParseTarget(t *testing.T) {
	tests := []struct {
		raw     string
		scheme  string
		host    string
		share   string
		path    string
		port    int
		wantErr bool
	}{
		{raw: `\\192.168.12.1\mnt\hdd1\game`, scheme: "smb", host: "192.168.12.1", share: "mnt", path: "hdd1/game"},
		{raw: `//192.168.12.1/mnt/hdd1/game`, scheme: "smb", host: "192.168.12.1", share: "mnt", path: "hdd1/game"},
		{raw: `\\nas\share`, scheme: "smb", host: "nas", share: "share"},
		{raw: `smb://nas/mnt/hdd1/game`, scheme: "smb", host: "nas", share: "mnt", path: "hdd1/game"},
		{raw: `smb://nas:1445/mnt`, scheme: "smb", host: "nas", share: "mnt", port: 1445},
		{raw: `local:C:\game`, scheme: "local", path: `C:\game`},
		{raw: `C:\game`, scheme: "local", path: `C:\game`},
		{raw: `local:///mnt/game`, scheme: "local", path: "/mnt/game"},
		{raw: `\\nas`, wantErr: true},
		{raw: `smb://nas`, wantErr: true},
		{raw: `ftp://nas/game`, wantErr: true},
		{raw: ``, wantErr: true},
	}

	for _, tc := range tests {
		got, err := ParseTarget(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.raw, err)
			continue
		}
		if got.Scheme != tc.scheme || got.Host != tc.host || got.Share != tc.share || got.Path != tc.path || got.Port != tc.port {
			t.Errorf("%q: got %+v", tc.raw, got)
		}
	}
}

func TestCleanRel(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: "a/b", want: "a/b"},
		{in: `a\b`, want: "a/b"},
		{in: "./a", want: "a"},
		{in: "a/../b", want: "b"},
		{in: "a/", want: "a"},
		{in: "../x", wantErr: true},
		{in: "a/../../x", wantErr: true},
		{in: "/abs", wantErr: true},
	}
	for _, tc := range tests {
		got, err := CleanRel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJoinRel(t *testing.T) {
	if got := JoinRel("", "a"); got != "a" {
		t.Errorf("got %q", got)
	}
	if got := JoinRel("a", ""); got != "a" {
		t.Errorf("got %q", got)
	}
	if got := JoinRel("a", "b/c"); got != "a/b/c" {
		t.Errorf("got %q", got)
	}
}

func TestUNCPath(t *testing.T) {
	tests := []struct {
		target Target
		want   string
	}{
		{
			target: Target{Scheme: "smb", Host: "192.168.12.1", Share: "mnt", Path: "hdd1/game"},
			want:   `\\192.168.12.1\mnt\hdd1\game`,
		},
		{
			target: Target{Scheme: "smb", Host: "nas", Share: "share"},
			want:   `\\nas\share`,
		},
	}
	for _, tc := range tests {
		if got := tc.target.UNCPath(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}
