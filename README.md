# NASArchiveSync

把本地 `zip` / `7z` / `rar` 归档通过 SMB、SFTP、S3 对象存储、WebDAV 或本地文件夹复制/上传，
读回文件做 SHA256 双向校验，写入 `原名.txt` 上传记录，确认无误后再删除本地压缩包。

## 功能

- 递归扫描源目录中的 `zip` / `7z` / `rar`（扩展名可配置）
- 保持相对目录结构，远端目录不存在时自动创建
- 可选用 `--with-source-dir` 在目标下额外创建与源目录同名的文件夹
- 支持复用系统已添加的 SMB 路径：Windows 下可直接使用 UNC 路径或映射网络驱动器，沿用已登录凭据，无需在工具中填写密码
- 支持 SFTP（含 `sftp://`、`scp://` 和 `user@host:/path` 写法），可用私钥或密码认证，默认校验 known_hosts
- 目标也可以是本地文件夹（`C:\backup`、`local:C:\backup`、`file:///C:/backup`），同样做 SHA256 校验、写记录、删源
- 支持 S3 兼容对象存储（Garage、MinIO、Ceph RGW、AWS S3），可选路径模式或虚拟主机/域名模式
- 支持 WebDAV（`webdav://`、`webdavs://`、`dav://`、`davs://`，也接受 `http://`/`https://`），支持 Basic 认证，兼容 OpenList/AList、Nextcloud、Apache mod_dav 等
- WebDAV 校验同样支持 Range 并发读取；服务器忽略 Range 时可用 `--verify-concurrency 1` 顺序校验
- SFTP 可选加密算法，默认 `aes128-gcm@openssh.com`（实测与 AES-256-GCM 并列最快且 CPU 更低），也可用 `--sftp-cipher` 指定
- 上传时顺带计算本地 SHA256（`io.TeeReader`），新文件只读一遍本地磁盘
- 上传后从 NAS 读回文件重新计算 SHA256，与本地比对
- S3/WebDAV 校验默认用 4 路 Range 并发读取（`--verify-concurrency`），哈希仍严格按字节顺序计算，结果与顺序读取完全一致
- 可选用 `--no-verify` 完全关闭校验：不计算 SHA256、上传后不读回远端，记录标记为未校验
- 校验一致后按 `--txt` 写入 `压缩包名.txt`（如 `z.7z.txt`）：默认 NAS 与本地各一份，也可只留一边或完全不生成
- 记录写入成功后才删除本地压缩包；本地 txt 记录会保留，任何失败都保留本地文件
- 目录层数限制、包含/排除模式
- `--dry-run` 演练
- `--list` 只列出匹配到的归档文件，可省略目标路径，适合检查 include/exclude 规则
- 默认开启上传/校验流水线：上传下一个文件的同时，用独立 SMB/SFTP 会话校验上一个文件（`--no-pipeline` 可关闭）
- 流水线模式显示两行动态进度（上传 / 校验），日志自动让位，不会打乱进度区域；终端不支持 ANSI 时自动退回普通日志
- 进度显示百分比、已传/总大小、实时速度、ETA；按终端宽度和中文显示宽度自适应，不会因长文件名换行
- 幂等：重复运行时已上传且校验一致的文件自动跳过
- 先写临时文件再重命名，避免产生半截文件
- 存储层抽象，当前支持 SMB、SFTP、S3、WebDAV 与本地目录，方便后续扩展 FTP/NFS

## 工作流程

```
扫描源目录（层数 / include / exclude）
  → 上传/复制到 目标根目录/相对目录/原文件名
     （加 --with-source-dir 时在目标根目录后插入源目录名）
     上传过程中同时计算本地 SHA256
  → 读回目标文件计算 SHA256
  → 一致：按 --txt 写入 原文件名.txt（默认目标 + 本地各一份）
  → 记录写入成功：删除本地压缩包（保留本地 txt）
```

默认流水线模式下，第 N 个文件上传完成后立即开始上传第 N+1 个文件，同时由另一个
连接（SMB/SFTP/S3/WebDAV 或本地目标）校验第 N 个文件；校验仍然是完整的 SHA256 读回，不做采样或只比大小。
S3 和 WebDAV 目标还会把读回拆成多个 Range 请求并发下载，再由主协程按顺序喂给 SHA256。

加 `--no-verify` 时会跳过“上传时哈希”和“读回校验”两步：文件上传成功后直接写记录
（记录中注明未校验），然后按记录写入结果删除本地压缩包。此模式不启用流水线。

## 编译

需要 Go 1.26 或更高版本。

Windows：

```powershell
go build -o dist\NASArchiveSync.exe .
.\build.ps1 -Version 0.8.0
```

Linux / macOS：

```bash
go build -o dist/NASArchiveSync .
./build.sh                      # 生成 dist/NASArchiveSync
./build.sh --version 0.8.0      # 注入版本号
./build.sh -o dist/NASArchiveSync-arm64 --goarch arm64
GOOS=linux GOARCH=arm64 ./build.sh   # 交叉编译
```

`build.sh` 默认使用 `CGO_ENABLED=0`，生成不依赖系统库的静态二进制，适合直接复制到 NAS
或精简 Linux 发行版。如果检出后提示没有执行权限，先运行 `chmod +x build.sh`。

## 快速开始

以你的场景为例：

```
源目录: C:\game
  a\z.7z

目标:   \\192.168.12.1\mnt\hdd1\game
```

先演练，不会做任何修改（`a\z.7z` 在子目录中，需要 `--depth 1`）：

```powershell
.\NASArchiveSync.exe --source "C:\game" --target "\\192.168.12.1\mnt\hdd1\game" --depth 1 --dry-run
```

确认计划后正式执行：

```powershell
$env:NAS_SMB_PASS = "你的密码"
.\NASArchiveSync.exe -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --smb-user "user"
```

结果：

```
\\192.168.12.1\mnt\hdd1\game\a\z.7z
\\192.168.12.1\mnt\hdd1\game\a\z.7z.txt

C:\game\a\z.7z.txt          # 本地记录保留
```

本地 `C:\game\a\z.7z` 会在两份记录都写入成功后被删除，`C:\game\a\z.7z.txt` 保留。

### 目标地址写法

| 写法 | 说明 |
|---|---|
| `\\192.168.12.1\mnt\hdd1\game` | UNC，共享名为 `mnt`，根目录为 `hdd1\game`；默认复用系统凭据 |
| `//192.168.12.1/mnt/hdd1/game` | 同上，正斜杠 |
| `smb://192.168.12.1/mnt/hdd1/game` | SMB URL，默认同样复用系统凭据 |
| `smb://192.168.12.1:1445/mnt/hdd1/game` | 指定端口（内置客户端） |
| `sftp://user@192.168.12.1/mnt/hdd1/game` | SFTP URL，默认 22 端口 |
| `sftp://user@192.168.12.1:2222/mnt/hdd1/game` | SFTP 指定端口 |
| `scp://user@192.168.12.1/mnt/hdd1/game` | scp 写法，实际使用 SFTP 子系统 |
| `user@192.168.12.1:/mnt/hdd1/game` | scp 简写，路径必须是绝对路径 |
| `192.168.12.1:/mnt/hdd1/game` | 省略用户名，默认使用当前系统用户 |
| `s3://oss/game` | S3 bucket `oss`，key 前缀 `game`；endpoint 用 `--s3-endpoint` 指定 |
| `webdav://user@host:5200/dav/game` | WebDAV（HTTP），路径按服务器 URL 路径解释 |
| `webdavs://user@host/dav/game` | WebDAV（HTTPS） |
| `https://user@host/dav/game` | 同样按 WebDAV 处理，等价于 `webdavs://` |
| `Z:\game` | 系统已映射的网络驱动器，直接当本地路径使用 |
| `local:C:\test` | 本地目录，用于测试或本地归档 |
| `file:///C:/test` | 本地目录的 file URL 写法 |
| `C:\test` | 同上 |

### 复用系统已添加的 SMB 路径

Windows 里已经能通过资源管理器打开、或已映射为网络驱动器的 SMB 路径，可以直接传给本工具：

```powershell
# UNC：使用 Windows 当前登录凭据，不需要 --smb-user/--smb-pass
.\NASArchiveSync.exe -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --dry-run

# 映射盘：Z: 已指向 \\192.168.12.1\mnt
.\NASArchiveSync.exe -s "C:\game" -t "Z:\game" --depth 1
```

访问方式由 `--smb-mode` 控制：

- `auto`（默认）：没有提供任何 SMB 凭据时使用系统访问；提供了 `--smb-user/--smb-pass/--smb-domain/--smb-ask-pass` 时使用内置客户端
- `native`：强制使用系统访问（Windows），复用已登录会话/映射盘凭据
- `builtin`：强制使用内置 SMB 客户端（需要可用的用户名密码，不支持匿名）

系统访问的前提是该路径在当前 Windows 用户下已经可读写。启动日志会显示 `(smb-native)` 表示走了系统访问。

## 复制到本地文件夹

`--target` 直接写本地路径即可，不经过 SMB/SFTP，适合把归档转移到另一块本地磁盘或本地备份目录。
流程与 NAS 完全一致：保持相对目录结构、上传后读回 SHA256 校验、写 txt 记录、成功后删除本地压缩包。

```powershell
# 直接写本地路径
.\NASArchiveSync.exe -s "C:\game" -t "D:\backup\game" --depth 1

# local: / file:// 写法
.\NASArchiveSync.exe -s "C:\game" -t "local:D:\backup\game" --depth 1
.\NASArchiveSync.exe -s "C:\game" -t "file:///D:/backup/game" --depth 1

# 本地复制时保留源压缩包
.\NASArchiveSync.exe -s "C:\game" -t "D:\backup\game" --depth 1 --keep-source
```

注意：

- 目标目录不存在会自动创建。
- 目标不能与源目录相同；目标位于源目录内时，扫描会自动跳过目标目录，避免递归。
- 本地目标同样会写 `<文件名>.txt`，默认目标侧和源侧各一份；可用 `--txt` 控制。
- 成功后默认删除源压缩包（与 NAS 行为一致），需要保留时加 `--keep-source`。
- 映射的网络驱动器（如 `Z:\`）会按本地路径处理，复用 Windows 已登录凭据。

## SFTP / SCP 上传

SFTP 与 SMB 共用同一套扫描、上传、SHA256 校验、记录和删源流程，只是把存储层换成 SSH。
`scp://` 和 `user@host:/path` 写法也走 SFTP 子系统（现代 OpenSSH 的 scp 本身也是这样实现的）。

### 认证

```powershell
# 默认自动尝试 ~/.ssh/id_ed25519、id_ecdsa、id_rsa
.\NASArchiveSync.exe -s "C:\game" -t "user@192.168.12.1:/mnt/hdd1/game" --depth 1

# 指定私钥
.\NASArchiveSync.exe -s "C:\game" -t "sftp://user@192.168.12.1/mnt/hdd1/game" --sftp-key "C:\keys\id_ed25519"

# 私钥有口令
$env:NAS_SFTP_KEY_PASS = "key-passphrase"
.\NASArchiveSync.exe -s "C:\game" -t "user@192.168.12.1:/mnt/hdd1/game" --depth 1

# 密码认证（推荐用环境变量或交互输入）
$env:NAS_SFTP_PASS = "password"
.\NASArchiveSync.exe -s "C:\game" -t "user@192.168.12.1:/mnt/hdd1/game" --depth 1

# 交互输入密码
.\NASArchiveSync.exe -s "C:\game" -t "user@192.168.12.1:/mnt/hdd1/game" --sftp-ask-pass
```

没有提供 `--sftp-user` 时默认使用当前 Windows 用户名；也可以把用户名写在目标地址里
（`user@host:/path` 或 `sftp://user@host/path`）。

### 主机密钥校验

默认读取 `~/.ssh/known_hosts` 并严格校验服务器主机密钥：

- 主机不在 known_hosts 中：连接失败，提示先添加主机密钥
- 主机密钥与记录不一致：连接失败（防止中间人攻击）
- 可用 `--sftp-known-hosts PATH` 指定其他 known_hosts
- 测试环境可用 `--sftp-insecure` 跳过校验（会打印警告，不建议用于生产）

known_hosts 中个别损坏行会被自动跳过（与 OpenSSH 的行为一致），不会因为一行坏数据导致
整个文件无法加载。

添加主机密钥：

```powershell
ssh-keyscan -H 192.168.12.1 >> $env:USERPROFILE\.ssh\known_hosts
```

### 加密算法

`--sftp-cipher` 可选：

- `auto`（默认）：优先使用 `aes128-gcm@openssh.com`，服务器不支持时按顺序回退
- `aes128-gcm@openssh.com`
- `aes256-gcm@openssh.com`
- `chacha20-poly1305@openssh.com`
- `aes128-ctr` / `aes192-ctr` / `aes256-ctr`

在 `192.168.12.1`（25 Gbps 链路，NAS 端 `/dev/shm` 内存盘，避免磁盘成为瓶颈）上实测，
每个算法传输 2 GiB、3 轮取中位数：

| 加密算法 | 上传 MiB/s | 读回 MiB/s |
|---|---:|---:|
| `aes128-gcm@openssh.com` | 588 | 694 |
| `aes256-gcm@openssh.com` | 586 | 709 |
| `chacha20-poly1305@openssh.com` | 328 | 344 |
| `aes128-ctr` | 216 | 237 |
| `aes192-ctr` | 214 | 235 |
| `aes256-ctr` | 210 | 233 |

AES-GCM 明显快于 ChaCha20 和 AES-CTR。AES-128-GCM 与 AES-256-GCM 在多次长测中互有胜负、
差距在噪声范围内（4 GiB × 5 轮：AES-128 约 565/690 MiB/s，AES-256 约 591/688 MiB/s），
因此默认选择 **AES-128-GCM**：吞吐相同、CPU 开销更低，需要更大密钥时改用
`--sftp-cipher aes256-gcm@openssh.com` 即可。

SFTP 数据包大小固定为 128 KiB（OpenSSH 单条消息上限 256 KiB，留出协议头空间）。
实测同一链路上 32 KiB 约 500 MiB/s 上传 / 460 MiB/s 读回，128 KiB 提升到约
590 MiB/s 上传 / 690 MiB/s 读回。

复现实测（需要一台可 SSH 登录的服务器）：

```powershell
$env:NAS_SFTP_BENCH_HOST = "192.168.12.1"
$env:NAS_SFTP_BENCH_USER = "root"
$env:NAS_SFTP_BENCH_DIR = "/dev/shm/nassync-bench"
$env:NAS_SFTP_BENCH_SIZE = "2147483648"
$env:NAS_SFTP_BENCH_ROUNDS = "3"
go test -c -o dist\sftpbench.test.exe .\internal\storage\sftp
.\dist\sftpbench.test.exe "--test.run=TestCipherThroughput" "--test.v" "--test.timeout=30m"
```

> 注意：Windows 防火墙可能拦截 `%TEMP%` 下的测试二进制，直接运行 `go test` 时若出现
> `dial tcp ... i/o timeout`，请像上面那样先把测试二进制编译到 `dist\` 再运行。

## WebDAV 上传

WebDAV 目标写成 `webdav://user@host:port/path`，HTTPS 用 `webdavs://` 或直接写
`https://`。`http://` 和 `https://` 地址也会按 WebDAV 处理。

```powershell
# 用户名密码写在 URL 中
.\NASArchiveSync.exe -s "C:\game" -t "webdav://user:password@192.168.12.1:5200/dav/game" --depth 1

# 或通过参数 / 环境变量提供（URL 中的凭据优先）
$env:NAS_WEBDAV_USER = "user"
$env:NAS_WEBDAV_PASS = "password"
.\NASArchiveSync.exe -s "C:\game" -t "webdav://192.168.12.1:5200/dav/game" --depth 1

# 交互输入密码
.\NASArchiveSync.exe -s "C:\game" -t "webdav://user@192.168.12.1:5200/dav/game" --webdav-ask-pass

# HTTPS（自签名证书测试环境可加 --webdav-insecure）
.\NASArchiveSync.exe -s "C:\game" -t "https://user@nas.example.com/dav/game" --webdav-insecure
```

注意 OpenList/AList 的 WebDAV 地址不是网页首页。以测试环境
`http://192.168.1.5:5200/` 为例，实际 WebDAV 根目录是
`http://192.168.1.5:5200/dav/`，因此目标应写成：

```powershell
.\NASArchiveSync.exe -s "C:\game" -t "webdav://192.168.1.5:5200/dav/game" --depth 1
```

### 认证

- `--webdav-user` / `--webdav-pass`，或环境变量 `NAS_WEBDAV_USER` / `NAS_WEBDAV_PASS`
- 也可以写成 `webdav://user:pass@host/path`；URL 中的用户名和密码优先于参数
- `--webdav-ask-pass` 交互输入密码，不回显
- HTTPS 自签名证书可用 `--webdav-insecure` 跳过证书校验（仅测试环境）

### 行为说明

- 远端目录用 WebDAV `MKCOL` 逐级创建；OpenList 的 `/dav` 会返回 307 到 `/dav/`，客户端会自动跟随
- 上传先写 `<文件名>.nassync.tmp`，完成后用 `MOVE` 重命名；服务器不支持 `MOVE` 时自动退回
  `COPY` + `DELETE`，文件不会经过客户端第二次传输
- 上传采用流式 PUT，不会把整个归档读进内存；如果大小已知会发送 `Content-Length`
- 32 MiB 以上的文件默认按 16 MiB 一个 Range 请求、4 路并发读回校验（`--verify-concurrency`），
  哈希仍由主协程按偏移顺序计算，结果与顺序读取完全一致
- 如果服务器忽略 `Range`，并行校验会明确报错；此时改用 `--verify-concurrency 1` 顺序校验即可
- 记录 txt 同样写入 WebDAV，例如 `game/a.zip.txt`

## S3 对象存储

目标写成 `s3://bucket/key前缀`，endpoint、区域和凭据通过参数或环境变量提供：

```powershell
$env:NAS_S3_ACCESS_KEY = "access-key"
$env:NAS_S3_SECRET_KEY = "secret-key"

# 路径模式（Garage / MinIO 用 IP 或 localhost 时推荐）
.\NASArchiveSync.exe -s "C:\game" -t "s3://oss/game" `
  --s3-endpoint "http://192.168.1.5:3900" --s3-region garage --s3-style path

# 虚拟主机 / 域名模式（endpoint 必须是域名，且 bucket.域名 能解析）
.\NASArchiveSync.exe -s "C:\game" -t "s3://oss/game" `
  --s3-endpoint "http://s3.example.com:3900" --s3-region garage --s3-style virtual

# auto（默认）：IP/localhost/单段主机名用 path，域名用 virtual
.\NASArchiveSync.exe -s "C:\game" -t "s3://oss/game" `
  --s3-endpoint "http://192.168.1.5:3900" --s3-region garage
```

### 路径模式与域名模式

| `--s3-style` | 实际请求 |
|---|---|
| `path` | `http://endpoint/bucket/key` |
| `virtual` | `http://bucket.endpoint/key` |
| `auto`（默认） | endpoint 是 IP、`localhost` 或单段主机名时用 `path`，否则用 `virtual` |

虚拟主机模式需要服务端支持：Garage 需要在 `garage.toml` 的 `[s3_api]` 中配置
`root_domain`（例如 `.s3.garage.localhost`），并为 `bucket.root_domain` 配置通配 DNS。
只配置了路径模式的 Garage，使用 `virtual` 会返回 `NoSuchBucket`；此时请改用
`--s3-style path`。路径模式在 Garage 上始终可用。

本次测试环境 `http://192.168.1.5:3900` 只启用了路径模式：`--s3-style path` 已完成上传、
校验、跳过、强制覆盖、`--no-verify`、multipart 上传和中文文件名测试；`--s3-style virtual`
会正确生成 `http://oss.<endpoint>/...` 请求，但服务端返回 `NoSuchBucket`，需要先在 Garage
配置 `root_domain` 和通配 DNS 才能使用。

### 认证与 bucket

- `--s3-access-key` / `--s3-secret-key`，或环境变量 `NAS_S3_ACCESS_KEY` / `NAS_S3_SECRET_KEY`
- `--s3-session-token` / `NAS_S3_SESSION_TOKEN`（临时凭据）
- 未提供静态凭据时，会走 AWS SDK 默认凭据链（环境变量 `AWS_*`、共享配置、实例角色等）
- `--s3-region`，Garage 通常为 `garage`
- `--s3-endpoint` 可省略，省略时使用 AWS 默认 endpoint；自定义服务建议显式填写
- bucket 必须已存在，工具不会自动创建 bucket
- 自签名 HTTPS 可用 `--s3-insecure` 跳过证书校验（仅测试环境）

### 行为说明

- S3 没有真实目录，`MkdirAll` 是空操作；key 前缀按 `/` 组织，控制台里会以目录形式展示
- 上传先写 `<key>.nassync.tmp`，再通过 `CopyObject` 复制到正式 key 并删除临时 key
- 超过 5 GiB 的对象自动使用分片复制（`UploadPartCopy`）
- 大文件上传默认按 16 MiB 分片、8 路并发；小文件直接 `PutObject`
- 分片大小可用 `--s3-part-size` 调整，并发数可用 `--s3-concurrency` 调整（1-16）
- 一次运行处理多个文件时会复用分片缓冲，峰值内存不随文件数量增长；单文件运行不保留共享池，
  上传结束后缓冲可尽快回收
- 超大文件会自动增大分片，避免超过 S3 的 10000 分片限制
- 读回校验默认按 16 MiB 一个 Range 请求、4 路并发下载（`--verify-concurrency`），
  每个分块下载后由主协程按偏移顺序写入 SHA256，结果与单连接顺序读取完全一致
- HTTP 连接池针对并发分片调优（每主机最多 32 条连接、64 KiB 读写缓冲）
- 为兼容 Garage/MinIO 等实现，默认不发送非必需的 checksum 头
- 单文件超过 5 GiB 的正式上传和校验流程与 SMB/SFTP 完全一致，仍然做完整 SHA256 读回
- 记录 txt 同样以对象形式写入 bucket，例如 `game/a.zip.txt`

测试环境示例（`192.168.1.5:3900`、region `garage`、bucket `oss`）：

```powershell
$env:NAS_S3_ACCESS_KEY = "你的 Access Key ID"
$env:NAS_S3_SECRET_KEY = "你的 Secret Access Key"

.\NASArchiveSync.exe -s "C:\game" -t "s3://oss/game" `
  --s3-endpoint "http://192.168.1.5:3900" --s3-region garage --s3-style path
```

## 命令行参数

| 参数 | 说明 |
|---|---|
| `-s, --source DIR` | 源目录，默认当前目录 |
| `-t, --target ADDR` | 必填，目标（UNC / smb:// / sftp:// / scp:// / host:/path / s3://bucket/prefix / webdav:// / webdavs:// / http(s):// / 本地路径 / local: / file://） |
| `--depth N` | 最大目录层数：`0` 仅源目录本身（默认），`-1` 不限，`1` 含一级子目录 |
| `--with-source-dir` | 在目标目录下创建与源目录同名的文件夹后再上传 |
| `-i, --include GLOB` | 只处理匹配的目录/文件，可重复指定 |
| `-e, --exclude GLOB` | 排除匹配的目录/文件，可重复指定 |
| `--ext EXT` | 归档扩展名，默认 `zip,7z,rar`，可重复指定 |
| `--dry-run` | 演练：只读检查并打印计划，不上传、不写记录、不删除 |
| `--no-pipeline` | 关闭上传/校验流水线，改为逐文件“上传 → 校验”顺序处理（默认开启流水线） |
| `--no-verify` | 关闭校验：不计算 SHA256，上传后不读回远端，记录标记为未校验（不能与 `--recheck` 同用） |
| `-l, --list` | 只列出匹配到的归档文件（相对路径），不上传、不写记录、不删除，可省略 `--target` |
| `--keep-source` | 上传成功后保留本地压缩包 |
| `--force` | 目标已存在且内容不一致时覆盖 |
| `--recheck` | 跳过已上传文件时重新读回远端校验 SHA256 |
| `--verify-concurrency N` | 远端校验的并发读连接数，默认 `4`，`1` 表示顺序读取；后端支持 Range 时生效（S3、WebDAV），范围为 1-16 |
| `--case-sensitive` | 包含/排除模式区分大小写（默认不区分） |
| `--smb-user NAME` | SMB 用户名，留空为匿名；支持 `DOMAIN\user` 写法 |
| `--smb-pass PASS` | SMB 密码（建议使用环境变量） |
| `--smb-ask-pass` | 交互式输入密码，不回显 |
| `--smb-domain DOMAIN` | SMB 域/工作组 |
| `--smb-port PORT` | SMB 端口，默认 445（内置客户端使用） |
| `--smb-mode MODE` | `auto`（默认）/`native`/`builtin`，见上文“复用系统已添加的 SMB 路径” |
| `--sftp-user NAME` | SFTP 用户名，默认当前系统用户；也可写在目标地址里 |
| `--sftp-pass PASS` | SFTP 密码（建议使用环境变量 `NAS_SFTP_PASS`） |
| `--sftp-ask-pass` | 交互式输入 SFTP 密码，不回显 |
| `--sftp-key PATH` | SFTP 私钥路径，默认自动尝试 `~/.ssh/id_ed25519`、`id_ecdsa`、`id_rsa` |
| `--sftp-key-pass PASS` | SFTP 私钥口令（环境变量 `NAS_SFTP_KEY_PASS`） |
| `--sftp-port PORT` | SFTP 端口，默认 22 |
| `--sftp-cipher NAME` | SFTP 加密算法，默认 `auto`（优先 `aes128-gcm@openssh.com`） |
| `--sftp-known-hosts PATH` | known_hosts 路径，默认 `~/.ssh/known_hosts` |
| `--sftp-insecure` | 跳过 SFTP 主机密钥校验（不安全，仅建议测试环境） |
| `--webdav-user NAME` | WebDAV 用户名；也可写在目标地址中 |
| `--webdav-pass PASS` | WebDAV 密码（建议使用环境变量 `NAS_WEBDAV_PASS`） |
| `--webdav-ask-pass` | 交互式输入 WebDAV 密码，不回显 |
| `--webdav-insecure` | 跳过 WebDAV HTTPS 证书校验（不安全，仅测试环境） |
| `--s3-endpoint URL` | S3 endpoint，如 `http://192.168.1.5:3900`；留空使用 AWS 默认端点 |
| `--s3-region NAME` | S3 区域，默认 `us-east-1`；Garage 通常填 `garage` |
| `--s3-access-key ID` | S3 Access Key ID（建议用 `NAS_S3_ACCESS_KEY`） |
| `--s3-secret-key KEY` | S3 Secret Access Key（建议用 `NAS_S3_SECRET_KEY`） |
| `--s3-session-token TOKEN` | S3 临时会话令牌（可选） |
| `--s3-style MODE` | `auto`（默认）/`path`/`virtual`，见“路径模式与域名模式” |
| `--s3-insecure` | 跳过 S3 HTTPS 证书校验（不安全，仅测试环境） |
| `--s3-part-size MIB` | S3 分片大小（MiB），默认 `16`，最小 `5` |
| `--s3-concurrency N` | S3 分片并发上传数，默认 `8`，最大 `16` |
| `--txt MODE` | 上传记录保存位置：`both`（默认）/`local`/`remote`/`none` |
| `--no-progress` | 关闭进度条 |
| `-v, --verbose` | 输出调试日志 |
| `--version` | 显示版本 |

环境变量：`NAS_SMB_USER`、`NAS_SMB_PASS`、`NAS_SMB_DOMAIN`、`NAS_SFTP_USER`、`NAS_SFTP_PASS`、
`NAS_SFTP_KEY`、`NAS_SFTP_KEY_PASS`、`NAS_WEBDAV_USER`、`NAS_WEBDAV_PASS`、`NAS_S3_ENDPOINT`、
`NAS_S3_REGION`、`NAS_S3_ACCESS_KEY`、`NAS_S3_SECRET_KEY`、`NAS_S3_SESSION_TOKEN`。

## 目录层数

`--depth` 指压缩文件所在目录相对源目录的层数：

```
C:\game                          depth 0
├─ root.zip                      depth 0
└─ a                             depth 1
   ├─ one.7z                     depth 1
   └─ b                          depth 2
      └─ two.7z                  depth 2
```

- `--depth 0`：仅 `root.zip`
- `--depth 1`：`root.zip`、`a\one.7z`
- `--depth 2`：再加上 `a\b\two.7z`
- 默认 `0`：仅扫描源目录根层，不进入子目录

## 源目录名作为子目录（--with-source-dir）

默认会把源目录里的内容直接铺到目标目录下：

```
源:   C:\game\a
目标: \\192.168.12.1\mnt\hdd1\game

默认结果:
\\192.168.12.1\mnt\hdd1\game\z.7z
\\192.168.12.1\mnt\hdd1\game\sub\y.zip
```

加上 `--with-source-dir` 后，会先在目标下创建与源目录同名的文件夹 `a`，再按原来的相对结构上传：

```
\\192.168.12.1\mnt\hdd1\game\a\z.7z
\\192.168.12.1\mnt\hdd1\game\a\sub\y.zip
```

- 文件夹名取自源目录的最后一层名称（`C:\game\a` → `a`）
- 目标下已存在同名文件夹时直接复用，不会清空
- 磁盘根目录（如 `C:\`）无法推导名称，会直接报错
- 目标目录恰好是源目录的上一级时（如源 `C:\game\a`、目标 `C:\game`），加此选项会指回源目录本身，工具会拒绝执行

```powershell
# 上传到 \\192.168.12.1\mnt\hdd1\game\a
.\NASArchiveSync.exe -s "C:\game\a" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --with-source-dir

# 先看计划
.\NASArchiveSync.exe -s "C:\game\a" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --with-source-dir --dry-run
```

## 包含与排除

模式使用 `/` 分隔，支持 `*`、`?`、`**`、`{a,b}`：

- 不含 `/` 的模式匹配任意层级的目录名或文件名：
  - `--exclude temp` 排除所有名为 `temp` 的目录
  - `--exclude "*.zip"` 只排除 zip
- 含 `/` 的模式匹配相对路径：
  - `--include "a/**"` 只处理 `a` 目录及其子目录
  - `--exclude "**/cache"` 排除任意层级的 `cache` 目录
- 模式命中目录时，其下所有文件都会生效
- 大小写默认不敏感，可加 `--case-sensitive`

示例：

```powershell
# 只上传第一、二层，排除 temp 和 cache
.\NASArchiveSync.exe -s "C:\game" -t "\\nas\share\game" --depth 2 --exclude temp --exclude cache

# 只上传 2024 和 2025 目录，保留源文件
.\NASArchiveSync.exe -s "D:\game" -t "\\nas\share\game" --depth 1 --include 2024 --include 2025 --keep-source
```

## 只列出匹配文件（--list）

`--list` 只扫描并打印匹配到的归档相对路径，不连接 NAS，不需要 `--target`，也不会创建目录、写记录或删除文件。路径输出到 stdout，一行一个，便于管道处理。

```powershell
# 列出当前目录（含一级子目录）所有 zip/7z/rar
.\NASArchiveSync.exe -s "C:\game" --depth 1 --list

# 只列出 [xxx] 这类文件夹中的归档
.\NASArchiveSync.exe -s "C:\game" --depth -1 --include "[[]*]" --list

# 只列出 [ 开头、] 结尾的文件夹中的归档
.\NASArchiveSync.exe -s "C:\game" --depth -1 --include "[[]*]" --list
```

输出示例：

```text
[abc]/a.7z
[abc]/sub/b.zip
```

说明：

- `--list` 与 `--dry-run` 同时使用时，只输出文件列表，不打印上传计划
- `--list` 不解析也不连接 `--target`；即使提供了目标路径也会忽略
- stdout 只有文件路径；警告和错误仍输出到 stderr
- 文件列表按相对路径排序，与实际上传顺序一致

## 上传记录

每个上传成功的压缩包默认会在两个位置各写一份相同的记录：

- NAS：`\\192.168.12.1\mnt\hdd1\game\a\z.7z.txt`
- 本地：`C:\game\a\z.7z.txt`（压缩包删除后保留，作为本地归档凭据）

### 记录保存位置（--txt）

| 值 | 行为 |
|---|---|
| `both`（默认） | NAS 和本地各写一份，内容完全相同 |
| `local` | 只在源压缩包旁写本地 txt |
| `remote` | 只在 NAS 压缩包旁写 txt |
| `none` | 不生成任何 txt |

说明：

- 选择 `local` 时，重复运行可依赖本地 txt 判断是否已上传；本地 txt 丢失时会读回远端文件校验后补写。
- 选择 `remote` 时，本地不生成记录。
- 选择 `none` 时没有记录文件，重复运行会读回远端文件计算 SHA256，确认一致后才跳过，不会直接覆盖。
- 切换保存位置不会主动删除之前已生成的另一侧 txt。

```powershell
# 只在本地保存记录
.\NASArchiveSync.exe -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --txt local

# 完全不生成记录
.\NASArchiveSync.exe -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --txt none
```

`z.7z.txt` 示例：

```
# NASArchiveSync 上传记录
原始文件名: z.7z
上传路径: \\192.168.12.1\mnt\hdd1\game\a\z.7z
文件大小(字节): 2610988544
SHA256: 3f2a...e91c
上传时间: 2026-10-24 15:04:05 +08:00
校验结果: 本地与远端 SHA256 一致
源路径: C:\game\a\z.7z
源文件修改时间: 2026-10-20 09:12:31 +08:00
上传耗时: 28.4s
工具版本: v0.8.0
```

记录使用 UTF-8 编码。幂等判断依赖 `SHA256` 与 `文件大小(字节)`。

## 关闭校验（--no-verify）

`--no-verify` 适合“网络和存储可信、优先追求速度”的场景，会同时关闭：

- 上传时的本地 SHA256 计算
- 上传后的远端读回与 SHA256 比对
- `--recheck` 重新校验（两者不能同时使用，会直接报错）

上传成功后仍会写 `原名.txt`，但内容会明确标记为未校验：

```text
# NASArchiveSync 上传记录
原始文件名: z.7z
上传路径: \\192.168.12.1\mnt\hdd1\game\a\z.7z
文件大小(字节): 2610988544
校验方式: none
SHA256: 未计算
上传时间: 2026-10-24 15:04:05 +08:00
校验结果: 未校验（已关闭校验）
源路径: C:\game\a\z.7z
源文件修改时间: 2026-10-20 09:12:31 +08:00
上传耗时: 28.4s
工具版本: v0.8.0
```

注意：

- 记录写入成功后仍会删除本地压缩包（可用 `--keep-source` 保留）。
- 幂等判断只能依赖记录中的“文件大小 + 源文件修改时间”，无法发现大小相同、修改时间未变但内容已损坏的情况。
- 远端已有同名文件但没有匹配记录时，默认报错并保留本地文件；确认后可用 `--force` 覆盖。
- `--txt none` 且关闭校验时没有记录可用，重复运行会遇到上述冲突，需要 `--force` 才会重新上传。
- 关闭校验时不会启用流水线（没有校验步骤可以重叠）。

```powershell
# 只上传，不计算哈希、不读回远端
.\NASArchiveSync.exe -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --no-verify

# 覆盖远端已有的同名文件
.\NASArchiveSync.exe -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --no-verify --force
```

## 行为与安全说明

- **流水线（默认）**：上传第 N+1 个文件的同时，用另一个 SMB/SFTP 会话校验第 N 个文件；两者互不阻塞，适合多个大文件。校验仍是完整读回 + SHA256。加 `--no-pipeline` 可恢复逐文件顺序处理；`--no-verify` 下没有校验步骤，自动按顺序上传。
- **SFTP 主机密钥**：默认严格校验 `~/.ssh/known_hosts`；`--sftp-insecure` 会关闭校验并打印警告。密码优先用环境变量或 `--sftp-ask-pass`，避免进入命令历史。
- **S3 桶**：只使用已存在的 bucket，不会自动创建；`s3://bucket/prefix` 中的 prefix 是对象 key 前缀，不是真实目录。
- **S3 重命名**：对象存储没有 rename，工具用 copy + delete 实现；超过 5 GiB 自动改用分片复制。
- **S3 兼容性**：默认不发送非必需的 checksum 头，避免 Garage/MinIO 等实现拒绝请求；对象存储的最终一致性由服务端保证。
- **上传即哈希（默认）**：新文件上传时用 `io.TeeReader` 顺带计算本地 SHA256，本地只读一遍；跳过判断、`--recheck` 等场景仍会单独读本地文件。`--no-verify` 下不计算哈希。
- **关闭校验**：`--no-verify` 不计算 SHA256、不读回远端，记录标记为未校验；成功后仍按记录写入结果删除本地文件，详见“关闭校验”章节。
- **失败不删源**：上传失败、校验不一致、所需记录写入失败都会保留本地文件。
- **本地记录**：默认本地 txt 与 NAS txt 内容完全一致；即使压缩包被删除，本地 txt 也会保留。
- **记录位置**：`--txt both|local|remote|none`，详见“上传记录”章节。
- **校验不一致**：会删除目标侧刚上传/复制的文件并保留本地文件，等待下次重试。
- **临时文件**：上传先写 `<文件名>.nassync.tmp`，成功后重命名为正式文件；记录先写 `.txt.tmp`。
- **已存在目标**：
  - 有匹配的上传记录且 SHA256/大小一致 → 跳过（`--recheck` 会重新读回校验）
  - 没有记录或内容不一致 → 默认报错并保留本地，使用 `--force` 才会覆盖
- **中断**：`Ctrl+C` 会停止后续文件，清理当前远端临时文件，已完成的文件不受影响。
- **防止误删**：删除前会再次检查源文件的大小和修改时间，上传期间被改动的文件会保留。
- **dry-run 只读**：会连接目标检查是否存在，但不创建目录、不写文件、不删除。
- **上传后读回（默认）**：目标侧 SHA256 通过完整读取文件计算，因此上传一个文件会产生一次写 + 一次读的流量（本地目标则是磁盘读）；流水线只是把“下一个文件的写”和“上一个文件的读”重叠起来。`--no-verify` 下没有这次读回。S3 后端会把读回拆成多个 Range 请求并发下载，默认 4 路，见 `--verify-concurrency`。
- **源文件变化**：上传前后各检查一次源文件大小和修改时间，若上传期间被改动会删除目标侧刚上传的文件并保留本地文件。
- **耗时统计**：结束时会额外打印本地哈希、上传累计、校验累计耗时，便于定位瓶颈。

## 性能说明

- **本地只读一遍**：新文件在上传时计算 SHA256；旧流程是“先读本地算哈希，再读本地上传”，现在少一次本地全量读取。
- **流水线重叠**：上传与校验使用两个独立的 SMB/SFTP 会话，队列长度为 1。上传第 N+1 个文件时，第 N 个文件正在读回校验；校验失败只删除对应远端文件并保留本地文件，其余文件继续处理。
- **复制缓冲区**：默认 2 MiB（源码中的 `maxCopyBufferSize`）。在 `\\192.168.12.1\mnt\hdd1\game\test` 上用 3 个 512 MiB 文件实测，512 KiB / 1 MiB 约 8.6–8.9s，2 MiB / 4 MiB 约 7.8–8.2s，再往上收益不明显。该值针对大文件；小于缓冲区的文件会按实际大小分配。
- **实测流水线收益**：同一组 3×512 MiB 文件，顺序模式约 7.7–8.4s，流水线约 7.5s；当校验读取相对上传更慢时收益更明显。流水线不改变校验强度，仍是完整 SHA256 读回。
- **关闭校验的收益**：`--no-verify` 省掉本地 SHA256 计算和一次完整的远端读回，网络流量减半、耗时最短；代价是无法发现传输或存储损坏。
- **SFTP 加密与数据包**：默认 `aes128-gcm@openssh.com`，实测上传约 590 MiB/s、读回约 690 MiB/s（25 Gbps 链路）。AES-GCM 约为 ChaCha20 的 1.8 倍、AES-CTR 的 2.8 倍。SFTP 数据包固定 128 KiB，比 32 KiB 约快 15%–50%。详见“SFTP / SCP 上传”章节。
- **S3 分片并发**：同一 Garage 环境、512 MiB 文件、`--no-verify` 隔离校验耗时的实测结果：

| 上传方式 | 吞吐 |
|---|---:|
| 优化前：32 MiB 分片顺序上传 | 29.6 MiB/s |
| 16 MiB × 4 并发 | 102 MiB/s |
| **16 MiB × 8 并发（新默认）** | **108 MiB/s** |
| 8 MiB × 8 并发 | 107 MiB/s |
| 32 MiB × 8 并发 | 108 MiB/s |

  该测试环境约 1 Gbps，108 MiB/s 已接近实际上限。并发分片把上传阶段的耗时从约 17s 降到约 4.7s；HTTP 连接池也同步做了调优，避免并发请求反复建连。分片/并发可用 `--s3-part-size` 和 `--s3-concurrency` 按网络调整。
- **S3 校验并发**：同一环境、512 MiB 文件，只统计读回 + SHA256 的耗时：

| 读回方式 | 吞吐 | 耗时 |
|---|---:|---:|
| `--verify-concurrency 1`（单连接顺序读取） | 181 MiB/s | 2.8s |
| **`--verify-concurrency 4`（新默认）** | **238 MiB/s** | **2.2s** |
| `--verify-concurrency 8` | 236 MiB/s | 2.2s |
| `--verify-concurrency 16` | 235 MiB/s | 2.2s |

  该环境读回上限约 240 MiB/s，2–4 路已经跑满，再增加连接没有收益。校验仍然是逐字节的完整
  SHA256：分块只是并发下载，哈希由单一协程按偏移顺序计算，不做采样，也不会漏掉任何字节。
  单个文件小于 32 MiB 时仍走单连接，避免额外请求开销。其他后端只要实现 `storage.RangeReader`
  也能获得同样的并行校验能力。

### 内存占用

Windows 发布版实测（每 40 ms 采样一次进程峰值，S3 endpoint 为 Garage）：

| 场景 | 峰值工作集 | 峰值私有内存 |
|---|---:|---:|
| `--list` 只扫描 | 10 MiB | 48 MiB |
| 本地文件夹目标，512 MiB | 19 MiB | 57 MiB |
| S3 单文件 512 MiB（默认，含校验） | 219 MiB | 256 MiB |
| S3 流水线 2 个文件（512 + 320 MiB） | 245 MiB | 284 MiB |
| S3 流水线 4 × 256 MiB | 260 MiB | 299 MiB |
| S3 只校验 512 MiB，`--recheck` | 86 MiB | 123 MiB |
| S3 单文件 512 MiB，`--no-verify` | 170 MiB | 207 MiB |
| 低内存配置（8 MiB × 4，校验 ×2） | 102 MiB | 138 MiB |

规律：

- 上传缓冲约为 `(并发数 + 1) × 分片大小`，默认 16 MiB × 8 约 144 MiB
- 校验缓冲约为 `16 MiB × --verify-concurrency`，默认 4 路约 64 MiB，内部上限 256 MiB
- 内存与文件大小无关：512 MiB 和 1 GiB 单文件都是约 219 MiB
- 一次运行处理多个文件时会复用上传/校验缓冲，峰值不会随文件数量增长；单文件运行则不保留共享池，
  上传结束后缓冲可被回收
- Go 运行时的 GC 目标是存活堆的 2 倍，因此峰值会略高于缓冲本身；上表已包含这部分开销

内存紧张时优先减小分片和并发：

```powershell
.\NASArchiveSync.exe -s "C:\game" -t "s3://oss/game" --s3-style path `
  --s3-part-size 8 --s3-concurrency 4 --verify-concurrency 2
```

512 MiB 文件实测峰值约 102 MiB，耗时 7.9s（默认配置约 7.5s）。

## 扩展其他协议

存储接口定义在 `internal/storage/storage.go`：

```go
type Backend interface {
    Kind() string
    Root() string
    Location(rel string) string
    MkdirAll(rel string) error
    Stat(rel string) (FileInfo, error)
    Open(rel string) (io.ReadCloser, error)
    Create(rel string) (Writer, error)
    Remove(rel string) error
    Rename(oldRel, newRel string) error
    Close() error
}
```

新增协议只需：

1. 在 `internal/storage/<proto>` 实现 `Backend`（可参考 `local`、`smb`、`sftp` 与 `s3`）
2. 在 `internal/storage.ParseTarget` 中识别新的地址前缀
3. 在 `internal/backends/backends.go` 中注册实现

## 测试

```powershell
go test ./...
go vet ./...
```

端到端测试使用 `local:` 后端，覆盖上传、校验、记录、删源、dry-run、层数、幂等、冲突、强制覆盖、远端损坏检测，以及 `--no-verify` 的上传、按记录跳过、冲突和强制覆盖。

SFTP 相关单元测试覆盖目标地址解析、加密算法选择、known_hosts 损坏行容错和主机密钥重试判断。
S3 相关单元测试覆盖目标地址解析、auto 模式选择、endpoint 规范化、CopySource 转义，以及
用本地 httptest 服务器验证 path/virtual 两种请求布局、上传、复制重命名、Stat、Range 读取
（服务器忽略 Range 时会报错而不是按错误数据算哈希）。并行校验另有单元测试，用模拟的
RangeReader 验证多路并发下得到的 SHA256 与顺序读取完全一致、每个分块都被读取、错误和取消
能正确传播。

S3 的真实 multipart 集成测试默认跳过，设置环境变量后运行：

```powershell
$env:NAS_S3_TEST_ENDPOINT = "http://192.168.1.5:3900"
$env:NAS_S3_TEST_BUCKET   = "oss"
$env:NAS_S3_REGION        = "garage"
$env:NAS_S3_ACCESS_KEY    = "..."
$env:NAS_S3_SECRET_KEY    = "..."
go test -race ./internal/storage/s3 -run TestRealMultipart -v
```

测试会上传 17 MiB（4 个分片）、重命名、读回并删除对象；对象名带时间戳，成功后会自行清理。
如果 Windows 防火墙拦截 `%TEMP%` 下的测试二进制，可先 `go test -race -c -o dist\s3test.exe ./internal/storage/s3`
再运行 `dist\s3test.exe "--test.run=TestRealMultipart" "--test.v"`。

真实链路的加密算法吞吐测试见“SFTP / SCP 上传”章节，默认跳过，不会在普通 `go test` 中访问网络。

## 常见问题

**Q：连接 SMB 失败 / 认证失败？**
确认主机、共享名、用户名密码与域；先执行 `--dry-run` 验证连接。本工具内置客户端使用 SMB2/SMB3（默认 445 端口），不支持 SMB1 和匿名登录。如果 Windows 资源管理器已经能打开目标路径，也可以不填凭据，默认会改用系统访问（`--smb-mode native`）。

**Q：内置客户端提示 Anonymous account is not supported？**
内置客户端不支持匿名，请使用 `guest` 账号、提供用户名密码，或改用系统访问（默认 `--smb-mode auto` 在无凭据时即走系统访问）。

**Q：密码放在命令行安全吗？**
不建议。优先使用环境变量 `NAS_SMB_PASS` / `NAS_SFTP_PASS` 或 `--smb-ask-pass` / `--sftp-ask-pass`，避免密码进入命令历史。

**Q：SFTP 连接失败 / 认证失败？**
确认主机、端口、用户名，以及 `~/.ssh/known_hosts` 是否已有该主机密钥。默认自动尝试
`~/.ssh/id_ed25519`、`id_ecdsa`、`id_rsa`；也可用 `--sftp-key` 指定私钥，或用
`--sftp-pass` / `NAS_SFTP_PASS` / `--sftp-ask-pass` 使用密码。先用 `--dry-run` 验证连接。

**Q：SFTP 提示 known_hosts 中没有主机密钥或密钥不匹配？**
先用 `ssh-keyscan -H 192.168.12.1 >> $env:USERPROFILE\.ssh\known_hosts` 添加。测试环境可临时加
`--sftp-insecure`（会打印警告），不建议长期用于生产。

**Q：SFTP 选哪个加密算法？**
默认 `auto`，优先 `aes128-gcm@openssh.com`。实测它和 `aes256-gcm@openssh.com` 吞吐基本一致，
明显快于 ChaCha20 和 AES-CTR。需要更大密钥时用 `--sftp-cipher aes256-gcm@openssh.com`。

**Q：可以复制到本地文件夹吗？**
可以。`--target` 直接写 `D:\backup\game`、`local:D:\backup\game` 或 `file:///D:/backup/game`。
流程与 NAS 一样：自动建目录、SHA256 校验、写 txt、成功后删除源压缩包（`--keep-source` 可保留）。
目标不能与源目录相同；目标在源目录内时会被自动跳过。

**Q：S3 连接失败 / 认证失败？**
确认 `--s3-endpoint` 带协议（`http://` 或 `https://`）、`--s3-region`（Garage 常用 `garage`）
和 Access Key/Secret Key 是否正确。先执行 `--dry-run`，它只做 `HeadBucket` 和 `Stat`，不会写入。
如果使用临时凭据，补上 `--s3-session-token`。

**Q：S3 路径模式和域名模式怎么选？**
IP、`localhost` 或 MinIO/Garage 的普通地址用 `--s3-style path`。域名模式需要服务端支持
virtual-host：Garage 要配置 `[s3_api].root_domain` 和通配 DNS，MinIO 也要能解析
`bucket.域名`。如果 `virtual` 返回 `NoSuchBucket`，说明服务端没有启用该模式，改用 `path`。

**Q：S3 bucket 不存在会自动创建吗？**
不会。请先在 Garage/MinIO/AWS 控制台或 CLI 创建 bucket；工具启动时会用 `HeadBucket` 检查，
不存在会直接报错。

**Q：提示目标已存在且没有有效上传记录？**
说明远端已有同名文件但不是本工具写入的，或 SHA256 与本地不同。确认后可加 `--force`。`--no-verify` 下没有 SHA256，只能比较记录中的大小和修改时间，没有匹配记录时同样需要 `--force`。

**Q：如何关闭校验？**
加 `--no-verify`。它不计算 SHA256、上传后不读回远端，记录中会写 `校验方式: none` 和 `校验结果: 未校验（已关闭校验）`。适合可信网络/存储，代价是无法发现传输或存储损坏。不能与 `--recheck` 同时使用。

**Q：上传很慢？**
工具默认会在上传后再完整读取一次做校验，属正常现象；默认流水线会让下一个文件的上传与上一个文件的校验重叠，减少等待。可先用 `--dry-run` 确认文件列表。本地哈希已并入上传过程，不再额外读一遍源文件。如果完全不需要校验，可用 `--no-verify` 省掉本地哈希和远端读回。

**Q：会删除哪些本地文件？**
默认只会删除成功上传、远端校验一致、且按 `--txt` 所需的记录都写入成功的源压缩包，并受 `--keep-source` 控制。`--no-verify` 下只要上传完成且记录写入成功就会删除。本地的 `压缩包名.txt` 记录不会被删除。

**Q：`--txt none` 后重复运行会覆盖吗？**
不会。默认模式下没有记录文件时，工具会读回远端文件计算 SHA256，与本地一致则跳过，不一致则报冲突（除非加 `--force`）。`--no-verify` 下无法读回比对，没有记录时一律按冲突处理，需要 `--force` 才会重新上传。

**Q：进度条为什么有时会重复很多行？**
进度条会按终端实际宽度和中文/全角字符的显示宽度截断。顺序模式占一行；默认流水线模式占两行（上传 / 校验），并通过 ANSI 光标移动原地刷新，日志出现时会先清空进度区域、打印日志、再重绘。进度条写入 stdout，普通日志写入 stderr，避免 Windows PowerShell 把 stderr 的回车刷新拆成多行。如果输出被重定向、经过日志包装器，或所在终端不支持 ANSI，工具会自动退回普通日志（流水线模式下不再显示动态进度）；这类环境也可以直接加 `--no-progress`。

**Q：为什么结束时有“本地哈希 / 上传累计 / 校验累计”？**
用于区分三个阶段各自花了多少时间。新文件上传时本地哈希与上传同时进行，所以“本地哈希”通常只统计跳过判断、`--recheck` 等单独读本地文件的耗时。

**Q：支持分卷压缩包吗？**
默认支持 `.zip` / `.7z` / `.rar`。分卷可自行添加扩展名，例如 `--ext 7z.001`，但各分卷会被当作独立文件处理。

**Q：退出码？**
`0` 全部成功或跳过；`1` 有文件失败或运行中断；`2` 参数错误。
