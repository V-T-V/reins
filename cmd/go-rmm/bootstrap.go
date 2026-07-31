// CLI bootstrap 命令:从 Relay 获取一行安装命令(方案 B 远程部署)。
//
// 用法:
//   go-rmm bootstrap                      # 自动用本机 IP
//   go-rmm bootstrap --host 203.0.113.99 # 指定公网 IP
//   go-rmm bootstrap --port 443           # 指定端口
//
// 输出一行可直接复制粘贴到目标 Win10 执行的 PowerShell 命令。
package main

import (
	"flag"
	"fmt"
)

// cmdBootstrap 从 Relay 获取一行安装命令。
func cmdBootstrap(cfg config, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	host := fs.String("host", "", "覆盖自动检测的本机 IP(部署到公网时指定公网 IP)")
	port := fs.String("port", "", "覆盖端口")
	dev := fs.Bool("dev", false, "生成 dev 模式免 Token 命令")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// 构造查询参数
	path := "/api/bootstrap"
	q := []byte{}
	addParam := func(k, v string) {
		if v == "" {
			return
		}
		if len(q) == 0 {
			q = append(q, '?')
		} else {
			q = append(q, '&')
		}
		q = append(q, []byte(k+"="+v)...)
	}
	addParam("host", *host)
	addParam("port", *port)
	if *dev {
		addParam("dev", "1")
	}

	var info struct {
		ServerURL        string `json:"server_url"`
		RelayURL         string `json:"relay_url"`
		Token            string `json:"token"`
		OneLineCmd       string `json:"one_line_cmd"`
		OneLineDevCmd    string `json:"one_line_dev_cmd"`
		AgentBinaryReady bool   `json:"agent_binary_ready"`
		InstallScriptURL string `json:"install_script_url"`
		AgentDownloadURL string `json:"agent_download_url"`
	}
	if err := apiGet(cfg, path+string(q), &info); err != nil {
		return fmt.Errorf("获取 bootstrap 信息失败: %w", err)
	}

	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println(" go-rmm 一行命令远程部署")
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("Server:        %s\n", info.ServerURL)
	fmt.Printf("Relay:         %s\n", info.RelayURL)
	if info.Token != "" {
		masked := info.Token
		if len(masked) > 8 {
			masked = masked[:6] + "..." + masked[len(masked)-4:]
		}
		fmt.Printf("Token:         %s\n", masked)
	}
	fmt.Printf("Agent 二进制:  ")
	if info.AgentBinaryReady {
		fmt.Println("✅ 已就绪 (", info.AgentDownloadURL, ")")
	} else {
		fmt.Println("⚠️  未就绪(需把 agent.exe 拷到 Relay 的 dl/ 目录)")
	}
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println()
	fmt.Println("👉 在目标 Win10 机器(管理员 PowerShell)执行:")
	fmt.Println()
	if *dev || info.Token == "" {
		fmt.Println("   " + info.OneLineDevCmd)
	} else {
		fmt.Println("   " + info.OneLineCmd)
	}
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════════")
	if !info.AgentBinaryReady {
		fmt.Println()
		fmt.Println("⚠️  Agent 二进制未就绪,需先准备:")
		fmt.Println("   1. 编译: ./scripts/build-all.sh")
		fmt.Println("   2. 确认 bin/dl/go-rmm-agent.exe 存在")
		fmt.Println("   3. 重启 Relay")
	}
	return nil
}
