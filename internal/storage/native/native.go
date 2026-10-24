// Package native accesses SMB shares through the operating system's own SMB
// redirector instead of a userspace SMB client.
//
// On Windows a UNC path such as \\nas\share\dir is handled by the system, which
// reuses credentials that are already available (for example a mapped network
// drive, a session opened in Explorer, or the logged in account). No password
// has to be supplied to NASArchiveSync.
package native

import (
	"fmt"
	"runtime"

	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/storage/local"
)

// Backend delegates to the local file system layer while reporting a distinct
// kind so logs make the access path obvious.
type Backend struct {
	storage.Backend
}

// Open opens an SMB target through the operating system.
func Open(t storage.Target, opt storage.Options) (storage.Backend, error) {
	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("系统级 SMB 访问目前只支持 Windows；请提供 --smb-user/--smb-pass 使用内置客户端")
	}
	inner, err := local.Open(storage.Target{
		Scheme: "local",
		Path:   t.UNCPath(),
		Raw:    t.Raw,
	}, opt)
	if err != nil {
		return nil, fmt.Errorf("打开系统 SMB 路径 %s 失败（请确认 Windows 当前用户已能访问该路径，或提供 --smb-user/--smb-pass 使用内置客户端）: %w", t.UNCPath(), err)
	}
	return &Backend{Backend: inner}, nil
}

func (b *Backend) Kind() string { return "smb-native" }
