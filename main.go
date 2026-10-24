package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/alexflint/go-arg"
	"golang.org/x/term"

	"github.com/fjf/NASArchiveSync/internal/app"
	"github.com/fjf/NASArchiveSync/internal/console"
	"github.com/fjf/NASArchiveSync/internal/storage"
	"github.com/fjf/NASArchiveSync/internal/storage/sftp"
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

// version is overridden at build time via
// -ldflags "-X main.version=x.y.z".
var version = "0.8.0"

type cliArgs struct {
	Source         string   `arg:"-s,--source" default:"." help:"源目录，默认为当前目录"`
	Target         string   `arg:"-t,--target" help:"目标：\\\\host\\share\\dir、smb://host/share/dir、sftp://user@host/dir、s3://bucket/prefix、webdav://host/dav/dir，或本地文件夹 C:\\backup（--list 时可省略）"`
	Depth          int      `arg:"--depth" default:"0" help:"最大目录层数：0 仅源目录本身（默认），-1 不限，1 含一级子目录"`
	WithSourceDir  bool     `arg:"--with-source-dir" help:"在目标目录下创建与源目录同名的文件夹后再上传，如 源 C:\\game\\a → 目标\\game\\a"`
	List           bool     `arg:"-l,--list" help:"只列出匹配到的归档文件，不上传、不写记录、不删除（可省略 --target）"`
	Include        []string `arg:"-i,--include,separate" help:"只处理匹配的目录/文件，可重复使用"`
	Exclude        []string `arg:"-e,--exclude,separate" help:"排除匹配的目录/文件，可重复使用"`
	Exts           []string `arg:"--ext,separate" help:"归档扩展名，默认 zip,7z,rar，可重复使用"`
	DryRun         bool     `arg:"--dry-run" help:"演练模式：只打印计划，不上传、不写记录、不删除"`
	NoPipeline     bool     `arg:"--no-pipeline" help:"关闭上传/校验流水线，改为逐文件顺序处理（默认开启流水线）"`
	NoVerify       bool     `arg:"--no-verify" help:"关闭校验：不计算 SHA256，上传后不读回远端文件，记录标记为未校验"`
	KeepSource     bool     `arg:"--keep-source" help:"上传成功后保留本地压缩包"`
	Force          bool     `arg:"--force" help:"目标已存在且内容不一致时覆盖"`
	Recheck        bool     `arg:"--recheck" help:"跳过已上传文件时重新读回远端校验 SHA256"`
	VerifyConc     int      `arg:"--verify-concurrency" default:"4" help:"远端校验的并发读连接数，默认 4；1 表示顺序读取（需要后端支持分片读取，S3/WebDAV 支持）"`
	CaseSensitive  bool     `arg:"--case-sensitive" help:"包含/排除模式区分大小写（默认不区分）"`
	SMBUser        string   `arg:"--smb-user,env:NAS_SMB_USER" help:"SMB 用户名，留空表示匿名"`
	SMBPass        string   `arg:"--smb-pass,env:NAS_SMB_PASS" help:"SMB 密码，建议使用环境变量 NAS_SMB_PASS"`
	SMBAskPass     bool     `arg:"--smb-ask-pass" help:"交互式输入 SMB 密码（不回显）"`
	SMBDomain      string   `arg:"--smb-domain,env:NAS_SMB_DOMAIN" help:"SMB 域/工作组"`
	SMBPort        int      `arg:"--smb-port" default:"445" help:"SMB 端口，默认 445（内置客户端使用）"`
	SMBMode        string   `arg:"--smb-mode" default:"auto" help:"SMB 访问方式：auto 无凭据时复用系统访问；native 系统 UNC/映射盘；builtin 内置 SMB 客户端"`
	SFTPUser       string   `arg:"--sftp-user,env:NAS_SFTP_USER" help:"SFTP 用户名，默认使用当前系统用户"`
	SFTPPass       string   `arg:"--sftp-pass,env:NAS_SFTP_PASS" help:"SFTP 密码，建议使用环境变量 NAS_SFTP_PASS"`
	SFTPAskPass    bool     `arg:"--sftp-ask-pass" help:"交互式输入 SFTP 密码（不回显）"`
	SFTPKey        string   `arg:"--sftp-key,env:NAS_SFTP_KEY" help:"SFTP 私钥路径，默认自动尝试 ~/.ssh/id_ed25519、id_ecdsa、id_rsa"`
	SFTPKeyPass    string   `arg:"--sftp-key-pass,env:NAS_SFTP_KEY_PASS" help:"SFTP 私钥口令"`
	SFTPPort       int      `arg:"--sftp-port" default:"22" help:"SFTP 端口，默认 22"`
	SFTPCipher     string   `arg:"--sftp-cipher" default:"auto" help:"SFTP 加密算法：auto（默认，自动选择最快）、aes128-gcm@openssh.com、aes256-gcm@openssh.com、chacha20-poly1305@openssh.com、aes128-ctr、aes192-ctr、aes256-ctr"`
	SFTPKnown      string   `arg:"--sftp-known-hosts" help:"known_hosts 路径，默认 ~/.ssh/known_hosts"`
	SFTPInsecure   bool     `arg:"--sftp-insecure" help:"跳过 SFTP 主机密钥校验（不安全，仅建议测试环境使用）"`
	WebDAVUser     string   `arg:"--webdav-user,env:NAS_WEBDAV_USER" help:"WebDAV 用户名；也可写在目标地址中"`
	WebDAVPass     string   `arg:"--webdav-pass,env:NAS_WEBDAV_PASS" help:"WebDAV 密码，建议使用环境变量 NAS_WEBDAV_PASS"`
	WebDAVAskPass  bool     `arg:"--webdav-ask-pass" help:"交互式输入 WebDAV 密码（不回显）"`
	WebDAVInsecure bool     `arg:"--webdav-insecure" help:"跳过 WebDAV HTTPS 证书校验（不安全，仅建议测试环境使用）"`
	S3Endpoint     string   `arg:"--s3-endpoint,env:NAS_S3_ENDPOINT" help:"S3 endpoint，如 http://192.168.1.5:3900；留空使用 AWS 默认端点"`
	S3Region       string   `arg:"--s3-region,env:NAS_S3_REGION" default:"us-east-1" help:"S3 区域，Garage 常用 garage"`
	S3AccessKey    string   `arg:"--s3-access-key,env:NAS_S3_ACCESS_KEY" help:"S3 Access Key ID"`
	S3SecretKey    string   `arg:"--s3-secret-key,env:NAS_S3_SECRET_KEY" help:"S3 Secret Access Key，建议使用环境变量 NAS_S3_SECRET_KEY"`
	S3SessionToken string   `arg:"--s3-session-token,env:NAS_S3_SESSION_TOKEN" help:"S3 临时会话令牌（可选）"`
	S3Style        string   `arg:"--s3-style" default:"auto" help:"S3 访问方式：auto 自动、path 路径模式、virtual 虚拟主机模式"`
	S3Insecure     bool     `arg:"--s3-insecure" help:"跳过 S3 HTTPS 证书校验（不安全，仅测试环境使用）"`
	S3PartSize     int      `arg:"--s3-part-size" default:"16" help:"S3 分片大小（MiB），默认 16，最小 5"`
	S3Concurrency  int      `arg:"--s3-concurrency" default:"8" help:"S3 分片并发上传数，默认 8，最大 16"`
	Txt            string   `arg:"--txt" default:"both" help:"上传记录保存位置：both 双方（默认）、local 仅本地、remote 仅远程、none 不生成"`
	NoProgress     bool     `arg:"--no-progress" help:"关闭进度条"`
	Verbose        bool     `arg:"-v,--verbose" help:"输出调试日志"`
}

func (cliArgs) Description() string {
	return "NASArchiveSync - 把 zip/7z/rar 归档通过 SMB、SFTP、S3、WebDAV 或本地文件夹复制/上传，SHA256 双向校验后在目标和本地各写一份记录，再删除本地压缩包。"
}

func (cliArgs) Version() string {
	return "v" + version
}

func (cliArgs) Epilogue() string {
	return `示例:
  # 预览当前目录（含一级子目录）下所有 zip/7z/rar 的上传计划
  NASArchiveSync --target "\\192.168.12.1\mnt\hdd1\game" --depth 1 --dry-run

  # 只处理两层以内、排除 temp 目录，上传后删除源文件
  NASArchiveSync -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 2 --exclude temp

  # 只上传 2024 目录，保留源文件
  NASArchiveSync -s "D:\game" -t "smb://192.168.12.1/mnt/hdd1/game" --depth 1 --include "2024" --keep-source

  # 密码通过环境变量传入
  set NAS_SMB_PASS=secret
  NASArchiveSync -t "\\192.168.12.1\mnt\hdd1\game" --depth 1

  # 使用 Windows 已添加的 SMB 路径（无需输入凭据）
  NASArchiveSync -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --smb-mode native

  # 只在本地保留上传记录，不在 NAS 生成 txt
  NASArchiveSync -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --txt local

  # 完全不生成上传记录 txt
  NASArchiveSync -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --txt none

  # 在目标下创建与源目录同名的文件夹再上传
  # 源 C:\game\a → \\192.168.12.1\mnt\hdd1\game\a
  NASArchiveSync -s "C:\game\a" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --with-source-dir

  # 只列出匹配到的文件，不上传也不写记录（可省略 --target）
  NASArchiveSync -s "C:\game" --depth 1 --include "[[]*]" --list

  # 只列出所有 zip/7z/rar 的相对路径
  NASArchiveSync -s "C:\game" --depth -1 --list

  # 关闭流水线，改为逐文件上传后再校验
  NASArchiveSync -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --no-pipeline

  # 关闭校验：不计算 SHA256、不读回远端，记录标记为未校验
  NASArchiveSync -s "C:\game" -t "\\192.168.12.1\mnt\hdd1\game" --depth 1 --no-verify

  # 复制到本地文件夹（同样做 SHA256 校验、写记录、删源）
  NASArchiveSync -s "C:\game" -t "D:\backup\game" --depth 1

  # 本地文件夹也支持 file:// 和 local: 写法
  NASArchiveSync -s "C:\game" -t "file:///D:/backup/game" --depth 1

  # 通过 SFTP 上传（默认使用 ~/.ssh 下的私钥，known_hosts 校验主机）
  NASArchiveSync -s "C:\game" -t "sftp://user@192.168.12.1/mnt/hdd1/game" --depth 1

  # scp 风格地址（host:/path），同样使用 SFTP 子系统
  NASArchiveSync -s "C:\game" -t "192.168.12.1:/mnt/hdd1/game" --depth 1

  # 指定 SFTP 加密算法；auto 会优先使用实测最快的算法
  NASArchiveSync -s "C:\game" -t "sftp://user@192.168.12.1/mnt/hdd1/game" --sftp-cipher chacha20-poly1305@openssh.com

  # WebDAV（OpenList/AList 的 WebDAV 地址通常带 /dav）
  $env:NAS_WEBDAV_PASS = "password"
  NASArchiveSync -s "C:\game" -t "webdav://user@192.168.12.1:5200/dav/game" --depth 1

  # https:// 和 webdavs:// 走 HTTPS；自签名证书可加 --webdav-insecure
  NASArchiveSync -s "C:\game" -t "https://user@nas.example.com/dav/game" --webdav-insecure

  # S3 路径模式（IP/localhost endpoint 推荐）
  $env:NAS_S3_ACCESS_KEY = "access-key"
  $env:NAS_S3_SECRET_KEY = "secret-key"
  NASArchiveSync -s "C:\game" -t "s3://oss/game" --s3-endpoint "http://192.168.1.5:3900" --s3-region garage --s3-style path

  # S3 虚拟主机模式（endpoint 需要支持 oss.域名 解析）
  NASArchiveSync -s "C:\game" -t "s3://oss/game" --s3-endpoint "http://192.168.1.5.nip.io:3900" --s3-region garage --s3-style virtual

匹配模式:
  * 匹配任意字符，? 匹配单个字符，** 匹配多级目录，{a,b} 匹配多选一。
  不含 / 的模式匹配任意层级的目录名或文件名，例如 --exclude temp。
  含 / 的模式匹配相对路径，例如 --include "a/**"。
  大小写默认不敏感，可用 --case-sensitive 开启。`
}

func main() {
	os.Exit(run())
}

func run() int {
	var args cliArgs
	arg.MustParse(&args)

	logger := xlog.New(os.Stderr, args.Verbose, console.IsTerminal(os.Stderr))

	if args.Depth < -1 {
		logger.Errorf("--depth 不能小于 -1")
		return 2
	}
	if !args.List && strings.TrimSpace(args.Target) == "" {
		logger.Errorf("--target 必填（仅 --list 模式可省略，使用 --help 查看用法）")
		return 2
	}
	if args.SMBPort <= 0 || args.SMBPort > 65535 {
		logger.Errorf("--smb-port 必须在 1-65535 之间")
		return 2
	}
	if args.SFTPPort <= 0 || args.SFTPPort > 65535 {
		logger.Errorf("--sftp-port 必须在 1-65535 之间")
		return 2
	}
	if !validSFTPCipher(args.SFTPCipher) {
		logger.Errorf("--sftp-cipher 不支持 %q（可选: auto、%s）", args.SFTPCipher, strings.Join(sftp.SupportedCiphers, "、"))
		return 2
	}
	if args.NoVerify && args.Recheck {
		logger.Errorf("--recheck 需要读取远端并计算 SHA256，不能与 --no-verify 同时使用")
		return 2
	}

	smbMode := strings.ToLower(strings.TrimSpace(args.SMBMode))
	switch smbMode {
	case "auto", "native", "builtin":
	default:
		logger.Errorf("--smb-mode 必须是 auto、native 或 builtin")
		return 2
	}

	s3Style := strings.ToLower(strings.TrimSpace(args.S3Style))
	switch s3Style {
	case "auto", "path", "virtual":
	default:
		logger.Errorf("--s3-style 必须是 auto、path 或 virtual")
		return 2
	}
	if (args.S3AccessKey == "") != (args.S3SecretKey == "") {
		logger.Errorf("--s3-access-key 和 --s3-secret-key 必须同时提供")
		return 2
	}
	if args.S3PartSize < 5 || args.S3PartSize > 1024 {
		logger.Errorf("--s3-part-size 必须在 5-1024 MiB 之间")
		return 2
	}
	if args.S3Concurrency < 1 || args.S3Concurrency > 16 {
		logger.Errorf("--s3-concurrency 必须在 1-16 之间")
		return 2
	}
	if args.VerifyConc < 1 || args.VerifyConc > 16 {
		logger.Errorf("--verify-concurrency 必须在 1-16 之间")
		return 2
	}

	txtMode, txtErr := app.ParseTxtMode(args.Txt)
	if txtErr != nil {
		logger.Errorf("%v", txtErr)
		return 2
	}

	targetScheme := ""
	if !args.List {
		parsed, parseErr := storage.ParseTarget(args.Target)
		if parseErr != nil {
			logger.Errorf("%v", parseErr)
			return 2
		}
		targetScheme = parsed.Scheme
	}

	password := args.SMBPass
	if args.SMBAskPass && !args.List && targetScheme == "smb" && smbMode != "native" {
		if !console.IsTerminal(os.Stdin) {
			logger.Errorf("当前环境无法交互输入密码，请改用 --smb-pass 或 NAS_SMB_PASS")
			return 2
		}
		fmt.Fprint(os.Stderr, "SMB 密码: ")
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			logger.Errorf("读取 SMB 密码失败: %v", err)
			return 2
		}
		password = string(pw)
	}

	sftpPassword := args.SFTPPass
	if args.SFTPAskPass && !args.List && targetScheme == "sftp" {
		if !console.IsTerminal(os.Stdin) {
			logger.Errorf("当前环境无法交互输入密码，请改用 --sftp-pass 或 NAS_SFTP_PASS")
			return 2
		}
		fmt.Fprint(os.Stderr, "SFTP 密码: ")
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			logger.Errorf("读取 SFTP 密码失败: %v", err)
			return 2
		}
		sftpPassword = string(pw)
	}

	webdavPassword := args.WebDAVPass
	if args.WebDAVAskPass && !args.List && targetScheme == "webdav" {
		if !console.IsTerminal(os.Stdin) {
			logger.Errorf("当前环境无法交互输入密码，请改用 --webdav-pass 或 NAS_WEBDAV_PASS")
			return 2
		}
		fmt.Fprint(os.Stderr, "WebDAV 密码: ")
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			logger.Errorf("读取 WebDAV 密码失败: %v", err)
			return 2
		}
		webdavPassword = string(pw)
	}

	exts := args.Exts
	if len(exts) == 0 {
		exts = []string{"zip", "7z", "rar"}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := app.Run(ctx, app.Options{
		Source:            args.Source,
		Target:            args.Target,
		Depth:             args.Depth,
		WithSourceDir:     args.WithSourceDir,
		List:              args.List,
		Include:           args.Include,
		Exclude:           args.Exclude,
		Extensions:        exts,
		DryRun:            args.DryRun,
		KeepSource:        args.KeepSource,
		Force:             args.Force,
		Recheck:           args.Recheck,
		Pipeline:          !args.NoPipeline,
		NoVerify:          args.NoVerify,
		VerifyConcurrency: args.VerifyConc,
		CaseSensitive:     args.CaseSensitive,
		SMB: app.SMBOptions{
			User:     args.SMBUser,
			Password: password,
			Domain:   args.SMBDomain,
			Port:     args.SMBPort,
		},
		SFTP: app.SFTPOptions{
			User:           args.SFTPUser,
			Password:       sftpPassword,
			KeyPath:        args.SFTPKey,
			KeyPassphrase:  args.SFTPKeyPass,
			Port:           args.SFTPPort,
			Cipher:         args.SFTPCipher,
			KnownHostsPath: args.SFTPKnown,
			Insecure:       args.SFTPInsecure,
		},
		WebDAV: app.WebDAVOptions{
			User:     args.WebDAVUser,
			Password: webdavPassword,
			Insecure: args.WebDAVInsecure,
		},
		S3: app.S3Options{
			Endpoint:     args.S3Endpoint,
			Region:       args.S3Region,
			AccessKey:    args.S3AccessKey,
			SecretKey:    args.S3SecretKey,
			SessionToken: args.S3SessionToken,
			Style:        s3Style,
			Insecure:     args.S3Insecure,
			PartSize:     int64(args.S3PartSize) << 20,
			Concurrency:  args.S3Concurrency,
		},
		SMBMode:          smbMode,
		SMBAuthRequested: args.SMBAskPass || args.SMBUser != "" || args.SMBPass != "" || args.SMBDomain != "",
		Txt:              txtMode,
		NoProgress:       args.NoProgress,
		Version:          "v" + version,
		Logger:           logger,
		ProgressOut:      os.Stdout,
		ListOut:          os.Stdout,
	})
	if err != nil {
		logger.Errorf("%v", err)
		if res.Duration > 0 && !args.List {
			app.PrintResult(os.Stdout, res)
		}
		return 1
	}

	if args.List {
		if res.Failed > 0 {
			return 1
		}
		return 0
	}

	app.PrintResult(os.Stdout, res)
	if res.Failed > 0 {
		return 1
	}
	return 0
}

func validSFTPCipher(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == "auto" {
		return true
	}
	for _, c := range sftp.SupportedCiphers {
		if c == name {
			return true
		}
	}
	return false
}
