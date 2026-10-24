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
	"github.com/fjf/NASArchiveSync/internal/xlog"
)

// version is overridden at build time via
// -ldflags "-X main.version=x.y.z".
var version = "0.1.0"

type cliArgs struct {
	Source        string   `arg:"-s,--source" default:"." help:"源目录，默认为当前目录"`
	Target        string   `arg:"-t,--target,required" help:"目标目录，如 \\\\192.168.12.1\\mnt\\hdd1\\game 或 smb://192.168.12.1/mnt/hdd1/game"`
	Depth         int      `arg:"--depth" default:"0" help:"最大目录层数：0 仅源目录本身（默认），-1 不限，1 含一级子目录"`
	WithSourceDir bool     `arg:"--with-source-dir" help:"在目标目录下创建与源目录同名的文件夹后再上传，如 源 C:\\game\\a → 目标\\game\\a"`
	Include       []string `arg:"-i,--include,separate" help:"只处理匹配的目录/文件，可重复使用"`
	Exclude       []string `arg:"-e,--exclude,separate" help:"排除匹配的目录/文件，可重复使用"`
	Exts          []string `arg:"--ext,separate" help:"归档扩展名，默认 zip,7z,rar，可重复使用"`
	DryRun        bool     `arg:"--dry-run" help:"演练模式：只打印计划，不上传、不写记录、不删除"`
	KeepSource    bool     `arg:"--keep-source" help:"上传成功后保留本地压缩包"`
	Force         bool     `arg:"--force" help:"目标已存在且内容不一致时覆盖"`
	Recheck       bool     `arg:"--recheck" help:"跳过已上传文件时重新读回远端校验 SHA256"`
	CaseSensitive bool     `arg:"--case-sensitive" help:"包含/排除模式区分大小写（默认不区分）"`
	SMBUser       string   `arg:"--smb-user" env:"NAS_SMB_USER" help:"SMB 用户名，留空表示匿名"`
	SMBPass       string   `arg:"--smb-pass" env:"NAS_SMB_PASS" help:"SMB 密码，建议使用环境变量 NAS_SMB_PASS"`
	SMBAskPass    bool     `arg:"--smb-ask-pass" help:"交互式输入 SMB 密码（不回显）"`
	SMBDomain     string   `arg:"--smb-domain" env:"NAS_SMB_DOMAIN" help:"SMB 域/工作组"`
	SMBPort       int      `arg:"--smb-port" default:"445" help:"SMB 端口，默认 445（内置客户端使用）"`
	SMBMode       string   `arg:"--smb-mode" default:"auto" help:"SMB 访问方式：auto 无凭据时复用系统访问；native 系统 UNC/映射盘；builtin 内置 SMB 客户端"`
	Txt           string   `arg:"--txt" default:"both" help:"上传记录保存位置：both 双方（默认）、local 仅本地、remote 仅远程、none 不生成"`
	NoProgress    bool     `arg:"--no-progress" help:"关闭进度条"`
	Verbose       bool     `arg:"-v,--verbose" help:"输出调试日志"`
}

func (cliArgs) Description() string {
	return "NASArchiveSync - 把 zip/7z/rar 归档通过 SMB 上传到 NAS，SHA256 双向校验后在 NAS 和本地各写一份记录，再删除本地压缩包。"
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
	if args.SMBPort <= 0 || args.SMBPort > 65535 {
		logger.Errorf("--smb-port 必须在 1-65535 之间")
		return 2
	}

	smbMode := strings.ToLower(strings.TrimSpace(args.SMBMode))
	switch smbMode {
	case "auto", "native", "builtin":
	default:
		logger.Errorf("--smb-mode 必须是 auto、native 或 builtin")
		return 2
	}

	txtMode, txtErr := app.ParseTxtMode(args.Txt)
	if txtErr != nil {
		logger.Errorf("%v", txtErr)
		return 2
	}

	password := args.SMBPass
	if args.SMBAskPass && smbMode != "native" {
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

	exts := args.Exts
	if len(exts) == 0 {
		exts = []string{"zip", "7z", "rar"}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := app.Run(ctx, app.Options{
		Source:        args.Source,
		Target:        args.Target,
		Depth:         args.Depth,
		WithSourceDir: args.WithSourceDir,
		Include:       args.Include,
		Exclude:       args.Exclude,
		Extensions:    exts,
		DryRun:        args.DryRun,
		KeepSource:    args.KeepSource,
		Force:         args.Force,
		Recheck:       args.Recheck,
		CaseSensitive: args.CaseSensitive,
		SMB: app.SMBOptions{
			User:     args.SMBUser,
			Password: password,
			Domain:   args.SMBDomain,
			Port:     args.SMBPort,
		},
		SMBMode:          smbMode,
		SMBAuthRequested: args.SMBAskPass || args.SMBUser != "" || args.SMBPass != "" || args.SMBDomain != "",
		Txt:              txtMode,
		NoProgress:       args.NoProgress,
		Version:          "v" + version,
		Logger:           logger,
		ProgressOut:      os.Stderr,
	})
	if err != nil {
		logger.Errorf("%v", err)
		if res.Duration > 0 {
			app.PrintResult(os.Stdout, res)
		}
		return 1
	}

	app.PrintResult(os.Stdout, res)
	if res.Failed > 0 {
		return 1
	}
	return 0
}
