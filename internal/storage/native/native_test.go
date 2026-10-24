package native

import (
	"runtime"
	"testing"

	"github.com/fjf/NASArchiveSync/internal/storage"
)

func TestOpenBuildsUNCBackend(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native SMB access is Windows only")
	}
	b, err := Open(storage.Target{
		Scheme: "smb",
		Host:   "192.168.12.1",
		Share:  "mnt",
		Path:   "hdd1/game",
	}, storage.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()

	if got := b.Kind(); got != "smb-native" {
		t.Errorf("Kind = %q", got)
	}
	if got := b.Root(); got != `\\192.168.12.1\mnt\hdd1\game` {
		t.Errorf("Root = %q", got)
	}
	if got := b.Location("a/z.7z"); got != `\\192.168.12.1\mnt\hdd1\game\a\z.7z` {
		t.Errorf("Location = %q", got)
	}
}
