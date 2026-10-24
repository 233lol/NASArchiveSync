# NASArchiveSync

把本地 `zip` / `7z` / `rar` 归档通过 SMB 上传到 NAS，读回文件做 SHA256 双向校验，写入
`原名.txt` 上传记录，确认无误后再删除本地压缩包。

## 功能

- 递归扫描源目录中的 `zip` / `7z` / `rar`（扩展名可配置）
- 保持相对目录结构，远端目录不存在时自动创建
- 可选用 `--with-source-dir` 在目标下额外创建与源目录同名的文件夹
- 支持复用系统已添加的 SMB 路径：Windows 下可直接使用 UNC 路径或映射网络驱动器，沿用已登录凭据，无需在工具中填写密码
- 上传后从 NAS 读回文件重新计算 SHA256，与本地比对
- 校验一致后按 `--txt` 写入 `压缩包名.txt`（如 `z.7z.txt`）：默认 NAS 与本地各一份，也可只留一边或完全不生成
- 记录写入成功后才删除本地压缩包；本地 txt 记录会保留，任何失败都保留本地文件
- 目录层数限制、包含/排除模式
- `--dry-run` 演练
- 单文件进度条：百分比、已传/总大小、实时速度、ETA
- 幂等：重复运行时已上传且校验一致的文件自动跳过
- 先写临时文件再重命名，避免产生半截文件
- 存储层抽象，当前支持 SMB 与本地目录，方便后续扩展 FTP/SFTP/NFS

## 工作流程

```
扫描源目录（层数 / include / exclude）
  → 计算本地 SHA256
  → 上传到 远端根目录/相对目录/原文件名
     （加 --with-source-dir 时在远端根目录后插入源目录名）
  → 读回远端文件计算 SHA256
  → 一致：按 --txt 写入 原文件名.txt（默认 NAS + 本地各一份）
  → 记录写入成功：删除本地压缩包（保留本地 txt）
```

## 编译

需要 Go 1.26 或更高版本。

```powershell
go build -o dist\NASArchiveSync.exe .
```

或使用构建脚本（可注入版本号）：

```powershell
.\build.ps1 -Version 0.1.0
```

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
| `Z:\game` | 系统已映射的网络驱动器，直接当本地路径使用 |
| `local:C:\test` | 本地目录，用于测试或本地归档 |
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

## 命令行参数

| 参数 | 说明 |
|---|---|
| `-s, --source DIR` | 源目录，默认当前目录 |
| `-t, --target ADDR` | 必填，目标目录（UNC / smb:// / local:） |
| `--depth N` | 最大目录层数：`0` 仅源目录本身（默认），`-1` 不限，`1` 含一级子目录 |
| `--with-source-dir` | 在目标目录下创建与源目录同名的文件夹后再上传 |
| `-i, --include GLOB` | 只处理匹配的目录/文件，可重复指定 |
| `-e, --exclude GLOB` | 排除匹配的目录/文件，可重复指定 |
| `--ext EXT` | 归档扩展名，默认 `zip,7z,rar`，可重复指定 |
| `--dry-run` | 演练：只读检查并打印计划，不上传、不写记录、不删除 |
| `--keep-source` | 上传成功后保留本地压缩包 |
| `--force` | 目标已存在且内容不一致时覆盖 |
| `--recheck` | 跳过已上传文件时重新读回远端校验 SHA256 |
| `--case-sensitive` | 包含/排除模式区分大小写（默认不区分） |
| `--smb-user NAME` | SMB 用户名，留空为匿名；支持 `DOMAIN\user` 写法 |
| `--smb-pass PASS` | SMB 密码（建议使用环境变量） |
| `--smb-ask-pass` | 交互式输入密码，不回显 |
| `--smb-domain DOMAIN` | SMB 域/工作组 |
| `--smb-port PORT` | SMB 端口，默认 445（内置客户端使用） |
| `--smb-mode MODE` | `auto`（默认）/`native`/`builtin`，见上文“复用系统已添加的 SMB 路径” |
| `--txt MODE` | 上传记录保存位置：`both`（默认）/`local`/`remote`/`none` |
| `--no-progress` | 关闭进度条 |
| `-v, --verbose` | 输出调试日志 |
| `--version` | 显示版本 |

环境变量：`NAS_SMB_USER`、`NAS_SMB_PASS`、`NAS_SMB_DOMAIN`。

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
工具版本: v0.1.0
```

记录使用 UTF-8 编码。幂等判断依赖 `SHA256` 与 `文件大小(字节)`。

## 行为与安全说明

- **顺序上传**：一次处理一个文件，进度显示清晰，单流即可跑满千兆网络。
- **失败不删源**：上传失败、校验不一致、所需记录写入失败都会保留本地文件。
- **本地记录**：默认本地 txt 与 NAS txt 内容完全一致；即使压缩包被删除，本地 txt 也会保留。
- **记录位置**：`--txt both|local|remote|none`，详见“上传记录”章节。
- **校验不一致**：会删除远端刚上传的文件并保留本地文件，等待下次重试。
- **临时文件**：上传先写 `<文件名>.nassync.tmp`，成功后重命名为正式文件；记录先写 `.txt.tmp`。
- **已存在目标**：
  - 有匹配的上传记录且 SHA256/大小一致 → 跳过（`--recheck` 会重新读回校验）
  - 没有记录或内容不一致 → 默认报错并保留本地，使用 `--force` 才会覆盖
- **中断**：`Ctrl+C` 会停止后续文件，清理当前远端临时文件，已完成的文件不受影响。
- **防止误删**：删除前会再次检查源文件的大小和修改时间，上传期间被改动的文件会保留。
- **dry-run 只读**：会连接 NAS 检查目标是否存在，但不创建目录、不写文件、不删除。
- **上传后读回**：远端 SHA256 通过完整读取文件计算，因此上传一个文件会产生一次写 + 一次读的网络流量。

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

1. 在 `internal/storage/<proto>` 实现 `Backend`（可参考 `local` 与 `smb`）
2. 在 `internal/storage.ParseTarget` 中识别新的地址前缀
3. 在 `internal/backends/backends.go` 中注册实现

## 测试

```powershell
go test ./...
go vet ./...
```

端到端测试使用 `local:` 后端，覆盖上传、校验、记录、删源、dry-run、层数、幂等、冲突、强制覆盖和远端损坏检测。

## 常见问题

**Q：连接 SMB 失败 / 认证失败？**
确认主机、共享名、用户名密码与域；先执行 `--dry-run` 验证连接。本工具内置客户端使用 SMB2/SMB3（默认 445 端口），不支持 SMB1 和匿名登录。如果 Windows 资源管理器已经能打开目标路径，也可以不填凭据，默认会改用系统访问（`--smb-mode native`）。

**Q：内置客户端提示 Anonymous account is not supported？**
内置客户端不支持匿名，请使用 `guest` 账号、提供用户名密码，或改用系统访问（默认 `--smb-mode auto` 在无凭据时即走系统访问）。

**Q：密码放在命令行安全吗？**
不建议。优先使用环境变量 `NAS_SMB_PASS` 或 `--smb-ask-pass`，避免密码进入命令历史。

**Q：提示目标已存在且没有有效上传记录？**
说明远端已有同名文件但不是本工具写入的，或 SHA256 与本地不同。确认后可加 `--force`。

**Q：上传很慢？**
工具会在上传后再完整读取一次做校验，属正常现象。可先用 `--dry-run` 确认文件列表。

**Q：会删除哪些本地文件？**
只会删除成功上传、远端校验一致、且按 `--txt` 所需的记录都写入成功的源压缩包，并受 `--keep-source` 控制。本地的 `压缩包名.txt` 记录不会被删除。

**Q：`--txt none` 后重复运行会覆盖吗？**
不会。没有记录文件时，工具会读回远端文件计算 SHA256，与本地一致则跳过，不一致则报冲突（除非加 `--force`）。

**Q：支持分卷压缩包吗？**
默认支持 `.zip` / `.7z` / `.rar`。分卷可自行添加扩展名，例如 `--ext 7z.001`，但各分卷会被当作独立文件处理。

**Q：退出码？**
`0` 全部成功或跳过；`1` 有文件失败或运行中断；`2` 参数错误。
