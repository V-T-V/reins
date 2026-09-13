// deploy init —— playbook 模板生成器。
//
// 模板来自真实使用会话验证过的部署模式(2026-08-31 loan-kiosk-platform
// 连续 5 轮部署沉淀):worktree 构建 → zip 上传(SHA256 双边校验)→ 解压 →
// robocopy /MIR 镜像(排除运行时目录/.env)→ npm install(npmmirror)→
// 重启计划任务 → 健康检查重试。手工重复 5 轮的模式固化为一条命令。
//
// 用法:
//
//	go-rmm deploy init myapp                        # 基础模板(upload→exec→verify)
//	go-rmm deploy init myapp --template zip-node    # zip 包+镜像同步+服务重启+健康检查
//	go-rmm deploy init myapp -o dir/out.json --force
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// reorderInitArgs 把混合顺序的参数重排为 flags-first。
// Go flag 包在首个位置参数处停止解析——`deploy init myapp --force` 的
// --force 会被静默丢弃(force 恒 false)。此处把 flag 连同其取值挪到
// 位置参数之前,支持两种书写顺序。
func reorderInitArgs(args []string) []string {
	valueFlags := map[string]bool{"--template": true, "-template": true, "--o": true, "-o": true}
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if valueFlags[a] && i+1 < len(args) {
			flags = append(flags, a, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

// deployInit 生成 playbook 模板文件。
func deployInit(args []string) error {
	fs := flag.NewFlagSet("deploy init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: go-rmm deploy init <名称> [--template basic|zip-node] [-o 输出路径] [--force]")
		fs.PrintDefaults()
	}
	template := fs.String("template", "basic", "模板: basic(上传+重启+验证) / zip-node(zip 包+镜像同步+npm+计划任务)")
	out := fs.String("o", "", "输出路径(默认 <名称>.playbook.json)")
	force := fs.Bool("force", false, "覆盖已存在的文件")
	if err := fs.Parse(reorderInitArgs(args)); err != nil {
		return err
	}
	name := posArg(fs.Args(), 0)
	if name == "" {
		return fmt.Errorf("缺少 playbook 名称: go-rmm deploy init <名称> [--template basic|zip-node]")
	}

	var pb *PlayBook
	switch *template {
	case "basic":
		pb = basicPlaybook(name)
	case "zip-node":
		pb = zipNodePlaybook(name)
	default:
		return fmt.Errorf("未知模板: %s (可用: basic zip-node)", *template)
	}

	path := *out
	if path == "" {
		path = name + ".playbook.json"
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("文件已存在: %s (覆盖请加 --force)", path)
	}
	data, err := json.MarshalIndent(pb, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		return err
	}
	fmt.Printf("✅ 已生成 %s (模板 %s,%d 步)\n", path, *template, len(pb.Steps))
	fmt.Printf("   下一步: 修改 target/workspace/vars 后 go-rmm deploy validate %s\n", path)
	return nil
}

// basicPlaybook 基础模板:上传二进制 → 停旧 → 启新 → 健康检查。
func basicPlaybook(name string) *PlayBook {
	return &PlayBook{
		Name:      name,
		Target:    "agent-YOUR-AGENT-ID",
		Workspace: `C:\apps\myapp`,
		Vars:      map[string]string{"port": "8080"},
		Steps: []PlayStep{
			{Name: "上传产物", Action: "upload", Src: "./app.exe", Dst: "app.exe.new"},
			{Name: "停服务", Action: "exec", Cmd: "Stop-Service myapp -ErrorAction SilentlyContinue", Timeout: 30},
			{Name: "替换", Action: "exec", Cmd: "Move-Item app.exe.new app.exe -Force"},
			{Name: "启服务", Action: "exec", Cmd: "Start-Service myapp", Timeout: 30},
			{Name: "健康检查", Action: "exec", Cmd: `Invoke-WebRequest -UseBasicParsing http://localhost:${port}/health | Select -Expand StatusCode`,
				Timeout: 20, Retry: 5, Delay: "3s"},
		},
	}
}

// zipNodePlaybook zip 包部署模板(Node/Electron 类项目,会话验证模式)。
//
// 关键细节(每条都踩过):
//   - robocopy 退出码 0-7 都是成功(1=有文件复制),必须归一化 exit code,
//     否则引擎按非零判失败
//   - /XD 排除运行时目录与 .env,防镜像同步清掉远端运行态数据
//   - npm 用国内镜像,长步骤 timeout 必须调大(默认 60s 会中途杀树)
//   - 重启用 Restart-ScheduledTask(原生 cmdlet;schtasks /create 被
//     命令策略拦截——Register-ScheduledTask 创建,Restart 维持)
//   - 健康检查 retry+delay 组合覆盖服务启动窗口
func zipNodePlaybook(name string) *PlayBook {
	return &PlayBook{
		Name:      name,
		Target:    "agent-YOUR-AGENT-ID",
		Workspace: `D:\deploy-staging`,
		Vars: map[string]string{
			"app_dir":    `D:\myapp`,
			"zip_name":   "app.zip",
			"node_dir":   `D:\nodejs`,
			"task_name":  "MyAppTask",
			"health_url": "http://localhost:3100/health",
		},
		Steps: []PlayStep{
			{Name: "上传 zip", Action: "upload", Src: "./dist/app.zip", Dst: "${zip_name}"},
			{Name: "解压到暂存", Action: "exec",
				Cmd:     `Expand-Archive -Path '${workspace}/${zip_name}' -DestinationPath '${workspace}/new' -Force`,
				Timeout: 300},
			{Name: "镜像同步到应用目录", Action: "exec",
				Cmd: `robocopy '${workspace}/new' '${app_dir}' /MIR /XD runtime node_modules .git /XF .env; ` +
					`if ($LASTEXITCODE -ge 8) { exit 1 } else { exit 0 }`,
				Timeout: 600},
			{Name: "安装依赖", Action: "exec",
				Cmd: `$env:NPM_CONFIG_REGISTRY='https://registry.npmmirror.com'; Set-Location '${app_dir}'; ` +
					`& '${node_dir}/npm.cmd' install --omit=dev; if ($LASTEXITCODE -ne 0) { exit 1 }`,
				Timeout: 900},
			{Name: "重启计划任务", Action: "exec",
				Cmd:     `Restart-ScheduledTask -TaskName '${task_name}' -ErrorAction Stop; Start-Sleep -Seconds 3`,
				Timeout: 120},
			{Name: "健康检查", Action: "exec",
				Cmd:     `(Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 '${health_url}').StatusCode`,
				Timeout: 30, Retry: 10, Delay: "3s"},
		},
	}
}
