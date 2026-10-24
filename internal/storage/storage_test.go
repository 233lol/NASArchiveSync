package storage

import "testing"

func TestParseTarget(t *testing.T) {
	tests := []struct {
		raw     string
		scheme  string
		user    string
		pass    string
		host    string
		share   string
		bucket  string
		path    string
		port    int
		tls     bool
		wantErr bool
	}{
		{raw: `\\192.168.12.1\mnt\hdd1\game`, scheme: "smb", host: "192.168.12.1", share: "mnt", path: "hdd1/game"},
		{raw: `//192.168.12.1/mnt/hdd1/game`, scheme: "smb", host: "192.168.12.1", share: "mnt", path: "hdd1/game"},
		{raw: `\\nas\share`, scheme: "smb", host: "nas", share: "share"},
		{raw: `smb://nas/mnt/hdd1/game`, scheme: "smb", host: "nas", share: "mnt", path: "hdd1/game"},
		{raw: `smb://nas:1445/mnt`, scheme: "smb", host: "nas", share: "mnt", port: 1445},
		{raw: `sftp://fjf@192.168.12.1/mnt/hdd1/game`, scheme: "sftp", user: "fjf", host: "192.168.12.1", path: "/mnt/hdd1/game"},
		{raw: `sftp://192.168.12.1:2222/mnt/game`, scheme: "sftp", host: "192.168.12.1", path: "/mnt/game", port: 2222},
		{raw: `scp://root@nas/mnt/game`, scheme: "sftp", user: "root", host: "nas", path: "/mnt/game"},
		{raw: `192.168.12.1:/mnt/hdd1/game`, scheme: "sftp", host: "192.168.12.1", path: "/mnt/hdd1/game"},
		{raw: `user@nas:/mnt/game`, scheme: "sftp", user: "user", host: "nas", path: "/mnt/game"},
		{raw: `nas:/mnt/game`, scheme: "sftp", host: "nas", path: "/mnt/game"},
		{raw: `s3://oss/game/test`, scheme: "s3", bucket: "oss", path: "game/test"},
		{raw: `s3://oss`, scheme: "s3", bucket: "oss"},
		{raw: `s3://oss//game/../test/`, scheme: "s3", bucket: "oss", path: "test"},
		{raw: `webdav://test:secret@192.168.1.5:5200/dav/game`, scheme: "webdav", user: "test", pass: "secret", host: "192.168.1.5", path: "/dav/game", port: 5200},
		{raw: `webdavs://user@nas/dav`, scheme: "webdav", user: "user", host: "nas", path: "/dav", tls: true},
		{raw: `dav://nas/dav/game`, scheme: "webdav", host: "nas", path: "/dav/game"},
		{raw: `davs://nas/dav`, scheme: "webdav", host: "nas", path: "/dav", tls: true},
		{raw: `http://user:pass@nas:8080/dav/../game`, scheme: "webdav", user: "user", pass: "pass", host: "nas", path: "/game", port: 8080},
		{raw: `https://nas/dav/%E4%B8%AD%E6%96%87`, scheme: "webdav", host: "nas", path: "/dav/中文", tls: true},
		{raw: `webdav://nas`, scheme: "webdav", host: "nas", path: "/"},
		{raw: `webdav://[::1]:5200/dav`, scheme: "webdav", host: "::1", path: "/dav", port: 5200},
		{raw: `local:C:\game`, scheme: "local", path: `C:\game`},
		{raw: `C:\game`, scheme: "local", path: `C:\game`},
		{raw: `C:/game`, scheme: "local", path: `C:/game`},
		{raw: `local:///mnt/game`, scheme: "local", path: "/mnt/game"},
		{raw: `file:///C:/backup`, scheme: "local", path: "C:/backup"},
		{raw: `file://localhost/C:/backup`, scheme: "local", path: "C:/backup"},
		{raw: `file:///mnt/backup`, scheme: "local", path: "/mnt/backup"},
		{raw: `file://server/share`, wantErr: true},
		{raw: `file:foo`, wantErr: true},
		{raw: `\\nas`, wantErr: true},
		{raw: `smb://nas`, wantErr: true},
		{raw: `sftp://`, wantErr: true},
		{raw: `sftp:foo`, wantErr: true},
		{raw: `smb:foo`, wantErr: true},
		{raw: `s3://`, wantErr: true},
		{raw: `s3:foo`, wantErr: true},
		{raw: `s3://oss:3900/game`, wantErr: true},
		{raw: `webdav://`, wantErr: true},
		{raw: `webdav://nas/dav?x=1`, wantErr: true},
		{raw: `webdav:foo`, wantErr: true},
		{raw: `http:foo`, wantErr: true},
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
		if got.Scheme != tc.scheme || got.User != tc.user || got.Password != tc.pass || got.Host != tc.host || got.Share != tc.share || got.Bucket != tc.bucket || got.Path != tc.path || got.Port != tc.port || got.TLS != tc.tls {
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
