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
temp file, keeps the source and continues; it must not abort the whole run. `--concurrency N`
(>1) instead opens N independent backends and processes N files at once, with upload and verify
sequential inside each worker. Transient failures are retried by `retryValue`, which asks
`storage.Reconnector` backends to rebuild their session first.

`--verify-only` is a read-only audit path: it hashes the local and remote copies without
uploading, writing records or deleting. `--clean-temp` uses `storage.Lister` to remove only
this tool's temporary files and then exits.

## Adding a storage protocol

The abstraction is `internal/storage/storage.go` (`Backend`, `Writer`, `SizedWriter`,
`RangeReader`, `RenameReplacer`, `Reconnector`, `AtomicCreator`, `Lister`, `Target`,
`Options`). A new protocol needs all of:

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
  On `--force` overwrites that backup is kept until the new copy passes verification, then
  discarded; a verification failure restores the previous file and its note.
  Clean remote temp/sidecar artifacts on failure, never the local source.
- Range-based parallel verification must compare the remote size with the expected size
  before hashing; otherwise extra trailing bytes would be ignored.
- Retries must only cover transient errors (timeouts, reset/broken connections, EOF, HTTP
  408/429/5xx). Never retry authentication, path conflicts, local file errors or cancellation.
  SMB/SFTP `Reconnect` must rebuild both the transport and the protocol session.
- `--concurrency > 1` disables the pipeline and requires one backend per worker. Workers must
  not share a `Writer`; they may share the bandwidth limiter, which is mutex-protected.
- `--verify-only` must never upload, write records or delete. It always hashes both copies and
  is incompatible with `--no-verify` and `--dry-run`.
- `--clean-temp` may only remove names matching the tool's `.nassync.*.tmp` /
  `.nassync.tmp` convention; never archives, completion notes or `.bak` backups.
- S3 direct writes (`AtomicCreator`) may only be used when no previous remote object has to be
  preserved. Overwriting an existing key still uses the temporary-object + rename path.
- The shared bandwidth limiter covers uploads and remote verification reads, not local hashing.
- `internal/report/report.go` is a parser contract, not just text: `key: value` lines,
  SHA256 required unless `校验方式: none`. Renaming keys/messages breaks skip detection.
- An existing remote file with no usable matching record is a conflict; only `--force`
  overwrites. Verification mismatch also deletes the remote copy and keeps the source.
- Case-insensitive targets (SMB, Windows local/native) must reject source trees whose
  remote paths collide after case folding; `--force` must never silently merge them.
  Source and target paths are compared after resolving symlinks and junctions, including
  for target directories that do not exist yet.
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
  virtual for domains. New objects use `CreateAtomic` (direct PutObject/CompleteMultipartUpload);
  overwrites and records still use temp-key + CopyObject + delete. `Rename` is CopyObject +
  delete; >5 GiB uses multipart copy.
- **WebDAV**: OpenList/AList's WebDAV root is `/dav/`, not the web root. MKCOL and PROPFIND
  follow exactly one trailing-slash redirect; other redirects are errors (credential leak /
  body replay). Bulk reads require `206` with exact `Content-Length`/`Content-Range`; if a
  server ignores `Range`, tell the user to use `--verify-concurrency 1`. MOVE falls back to
  COPY + DELETE.
- **Parallel verification** runs for every backend implementing `storage.RangeReader` (S3,
  WebDAV, SFTP, SMB, local/native) when concurrency > 1 and the file is ≥32 MiB. It uses
  16 MiB chunks read concurrently but hashed strictly in order; memory ≈
  `16 MiB × concurrency`, capped at 256 MiB per hash. SMB/SFTP open one handle per range.
- **Memory**: S3 upload ≈ `(concurrency + 1) × part size`. `--concurrency N` multiplies this
  and verification buffers by N. Low-memory recipe:
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
