// Package backends dispatches a --target value to the matching storage
// implementation.
package backends

import (
	"fmt"

	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/storage/local"
	"github.com/fjf/NASArchiveSync/internal/storage/native"
	"github.com/fjf/NASArchiveSync/internal/storage/smb"
)

// Open opens the backend for t. When native is true an SMB target is accessed
// through the operating system instead of the built-in SMB client.
func Open(t storage.Target, opt storage.Options, nativeAccess bool) (storage.Backend, error) {
	switch t.Scheme {
	case "smb":
		if nativeAccess {
			return native.Open(t, opt)
		}
		return smb.Open(t, opt)
	case "local":
		return local.Open(t, opt)
	default:
		return nil, fmt.Errorf("暂不支持存储协议 %q", t.Scheme)
	}
}
