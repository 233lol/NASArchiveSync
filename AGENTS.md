# AGENTS.md

Single-binary Go CLI that uploads `zip`/`7z`/`rar` archives to SMB, SFTP, S3, WebDAV or a
local folder, verifies SHA256, writes `<archive>.txt` records, then deletes the local archive.
Module `github.com/fjf/NASArchiveSync`, Go 1.26+ (`go.mod`; local toolchain is 1.27.x).

## Commands

```powershell
gofmt -w .
go vet ./...
go test ./...
go test -race ./...

# one package / one test
go test ./internal/app -run TestRunPipelineUploadsAndVerifies -v

.\build.ps1                                   # dist\NASArchiveSync.exe
sh build.sh --goos linux --goarch amd64       # static Linux binary (CGO_ENABLED=0)
```

GitHub Actions (`.github/workflows/ci.yml`) runs formatting, vet, tests and builds on Linux
and Windows; Linux also runs `-race`. No linter config or task runner exists. `dist/`,
`*.exe`, `*.test` and `*.out` are gitignored.

The version string is duplicated in `main.go` (`var version`), `build.sh` and `build.ps1`
(currently `0.8.0`). Bump all three together (plus README examples); `--version` prints
`v` + the value.

## Execution flow

`main.go` parses flags with `go-arg` and maps them to `app.Options`; `app.Run` scans the
source, then per file: inspect, idempotency/record check, upload to a unique
`<name>.nassync.<pid>.<rand>.tmp`, rename, read back and hash, write records, delete the
local archive only if it is unchanged.

Pipeline mode is the default: upload N+1 and verify N concurrently using **two independent
backend sessions** (queue length 1). A verification failure removes the remote file/sidecar/
temp file, keeps the source and continues; it must not abort the whole run.

## Adding a storage protocol

The abstraction is `internal/storage/storage.go` (`Backend`, `Writer`, `SizedWriter`,
`RangeReader`, `RenameReplacer`, `Target`, `Options`). A new protocol needs all of:

1. Implementation under `internal/storage/<proto>` (`Open`, CRUD, `Rename`, `Close`).
2. Address parsing in `storage.ParseTarget` (and tests in `storage_test.go`).
3. Registration in `internal/backends/backends.go`.
4. Options wiring: `storage.Options` → `app.Options` → `main.go` flags
   (`arg:"--flag,env:NAS_<PROTO>_..."`), help text and README.

## Invariants easy to break

- `--no-verify` must not compute SHA256 anywhere. It rejects `--recheck`, disables the
  pipeline, and skips on record size + source mtime (second precision). Its records use
  `校验方式: none`, `SHA256: 未计算`, `校验结果: 未校验（已关闭校验）`. Skips also require the
  remote file size to match the source and the record's `上传路径` to match the current target.
- The source archive is deleted only after the records required by `--txt` are written.
  The local `.txt` survives archive deletion, and the archive is re-checked (size + mtime)
  immediately before deletion; a changed source is kept.
- A local `.txt` is source-scoped, not target-scoped. Never trust it for a different target:
  require `上传路径` to equal the current remote path; otherwise read the remote back (or
  conflict under `--no-verify`).
- Uploads are atomic: a unique `.nassync.<pid>.<rand>.tmp` → rename; records use a unique
  temp name too. Backends whose rename cannot replace an existing file (`RenameReplacer`
  false: SMB, Windows local) back up the old file first and restore it if the rename fails.
  Clean remote temp/sidecar artifacts on failure, never the local source.
- Range-based parallel verification must compare the remote size with the expected size
  before hashing; otherwise extra trailing bytes would be ignored.
- `internal/report/report.go` is a parser contract, not just text: `key: value` lines,
  SHA256 required unless `校验方式: none`. Renaming keys/messages breaks skip detection.
- An existing remote file with no usable matching record is a conflict; only `--force`
  overwrites. Verification mismatch also deletes the remote copy and keeps the source.
- Logs go to **stderr**; progress and `--list` go to **stdout**. `--list` prints only
  relative paths, needs no target and must not open a backend.
- User-facing messages/help/README are Chinese; code comments are English. Preserve that.
- `build.sh` must keep LF endings (`.gitattributes`).

## Backend gotchas

- **SMB `auto`**: if no credentials/domain were requested, use the OS redirector
  (`native`, Windows-only, ignores supplied credentials); explicit credentials or a
  non-445 port use the built-in client. `native` simply delegates to the local backend
  over the UNC path.
- **S3**: the bucket must already exist (`HeadBucket` at open, never created). Optional
  checksum headers are disabled for Garage/MinIO compatibility. `s3://bucket/prefix` is a
  key prefix, not a directory. `auto` style = path for IP/localhost/single-label hosts,
  virtual for domains. `Rename` is CopyObject + delete; >5 GiB uses multipart copy.
- **WebDAV**: OpenList/AList's WebDAV root is `/dav/`, not the web root. MKCOL and PROPFIND
  follow exactly one trailing-slash redirect; other redirects are errors (credential leak /
  body replay). Bulk reads require `206` with exact `Content-Length`/`Content-Range`; if a
  server ignores `Range`, tell the user to use `--verify-concurrency 1`. MOVE falls back to
  COPY + DELETE.
- **Parallel verification** only runs when the backend implements `storage.RangeReader`,
  concurrency > 1 and the file is ≥32 MiB. It uses 16 MiB chunks read concurrently but hashed
  strictly in order; memory ≈ `16 MiB × concurrency`, capped at 256 MiB.
- **Memory**: S3 upload ≈ `(concurrency + 1) × part size`. Low-memory recipe:
  `--s3-part-size 8 --s3-concurrency 4 --verify-concurrency 2`.

## Testing notes

- App/end-to-end tests use `local:` targets and temp dirs; they must not require network.
- `internal/storage/s3` `TestRealMultipart` skips unless `NAS_S3_TEST_ENDPOINT` **and**
  `NAS_S3_TEST_BUCKET` are set. Test Garage is `http://192.168.1.5:3900`, region `garage`,
  bucket `oss`; credentials come from `NAS_S3_ACCESS_KEY` / `NAS_S3_SECRET_KEY`. Never commit
  credentials or test passwords.
- SFTP throughput benchmark skips unless `NAS_SFTP_BENCH_HOST` (and friends) are set.
- WebDAV tests use an in-process fake server. Real testing uses OpenList at
  `http://192.168.1.5:5200/dav/`; credentials are supplied by the user/env, not stored here.
- Windows firewall can block test binaries under `%TEMP%`. If `go test` hangs with socket
  errors, build into `dist\` first: `go test -c -o dist\s3test.exe ./internal/storage/s3`,
  then run that exe.
- The harness PowerShell may garble UTF-8 console output and has no `Get-FileHash`; verify
  Chinese text by reading files instead of trusting terminal rendering.
