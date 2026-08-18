// CLI net 命令 — 从远端 Agent 视角的网络诊断。
//
// 用途:排查"Agent 能不能访问某服务""DNS 是否正确""路由在哪断"。
// 所有命令在 Agent 所在网络执行(非本地),这是核心价值。
//
// 用法:
//
//	go-rmm net ping -a <agent> <host> [-n 4]
//	go-rmm net dns -a <agent> <domain>
//	go-rmm net trace -a <agent> <host>
//	go-rmm net ports -a <agent> [--state listen|established]
//	go-rmm net check -a <agent> <host:port>   (TCP 连通性检测)
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// cmdNet net 子命令入口。
func cmdNet(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm net <ping|dns|trace|ports|check> -a <agent> [target]")
	}
	switch args[0] {
	case "ping":
		return netPing(cfg, args[1:])
	case "dns", "resolve":
		return netDNS(cfg, args[1:])
	case "trace", "traceroute":
		return netTrace(cfg, args[1:])
	case "ports", "connections", "netstat":
		return netPorts(cfg, args[1:])
	case "check", "connect", "tcping":
		return netCheck(cfg, args[1:])
	default:
		return fmt.Errorf("未知 net 子命令: %s (可用: ping dns trace ports check)", args[0])
	}
}

// netPing 从远端 ping 目标。
func netPing(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	target := posArg(args, 1)
	if agentID == "" || target == "" {
		return fmt.Errorf("用法: go-rmm net ping -a <agent> <host> [-n 4]")
	}
	count := 4
	if n := extractArg(args, "-n"); n != "" {
		fmt.Sscanf(n, "%d", &count)
	}
	// 平台感知:Linux 走 ping -c,Windows 走 Test-Connection。
	// (原双发 fallback 的 sh 命令也经 powershell 任务类型下发——Linux Agent
	// 无 powershell 执行器,该族命令在 Linux 上从未可用,fallback 是死代码。)
	var stdout string
	var err error
	if isLinuxAgent(cfg, agentID) {
		stdout, err = runOpsScript(cfg, agentID, fmt.Sprintf("ping -c %d -W 5 %s", count, target), 30)
	} else {
		stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("Test-Connection -ComputerName %s -Count %d | Select-Object Address,ResponseTime,StatusCode | Format-Table -AutoSize | Out-String", target, count), 30)
	}
	if err != nil {
		return err
	}
	fmt.Println(stdout)
	return nil
}

// netDNS 从远端解析 DNS。
func netDNS(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	domain := posArg(args, 1)
	if agentID == "" || domain == "" {
		return fmt.Errorf("用法: go-rmm net dns -a <agent> <domain>")
	}
	// 平台感知:Linux 走 dig/nslookup,Windows 走 Resolve-DnsName
	var stdout string
	var err error
	if isLinuxAgent(cfg, agentID) {
		stdout, err = runOpsScript(cfg, agentID, fmt.Sprintf("dig +short %s A 2>/dev/null || nslookup %s", domain, domain), 15)
	} else {
		stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("Resolve-DnsName -Name %s | Select-Object Name,Type,IPAddress | Format-Table -AutoSize | Out-String", domain), 15)
	}
	if err != nil {
		return err
	}
	fmt.Println(stdout)
	return nil
}

// netTrace 从远端 traceroute。
func netTrace(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	target := posArg(args, 1)
	if agentID == "" || target == "" {
		return fmt.Errorf("用法: go-rmm net trace -a <agent> <host>")
	}
	// 平台感知:Linux 走 traceroute/tracepath,Windows 走 tracert
	var stdout string
	var err error
	if isLinuxAgent(cfg, agentID) {
		stdout, err = runOpsScript(cfg, agentID, fmt.Sprintf("traceroute -m 15 %s 2>/dev/null || tracepath %s", target, target), 60)
	} else {
		stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("tracert -d -h 15 %s", target), 60)
	}
	if err != nil {
		return err
	}
	fmt.Println(stdout)
	return nil
}

// netPorts 查看远端端口/连接。
func netPorts(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	if agentID == "" {
		return fmt.Errorf("用法: go-rmm net ports -a <agent> [--state listen]")
	}
	state := extractArg(args, "--state")
	if state == "" {
		state = "listen"
	}
	// 平台感知:Linux 走 ss/netstat,Windows 走 Get-NetTCPConnection
	var stdout string
	var err error
	if isLinuxAgent(cfg, agentID) {
		cmd := "ss -tlnp 2>/dev/null || netstat -tlnp"
		if state != "listen" {
			cmd = fmt.Sprintf("ss -tnp state %s 2>/dev/null || netstat -tnp", state)
		}
		stdout, err = runOpsScript(cfg, agentID, cmd, 15)
	} else {
		// PowerShell -State 需首字母大写;strings.Title 已废弃,这里单词参数手动首字母大写即可
		titled := strings.ToUpper(state[:1]) + state[1:]
		stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("Get-NetTCPConnection -State %s | Select-Object LocalAddress,LocalPort,State,OwningProcess | Sort LocalPort | Format-Table -AutoSize | Out-String", titled), 15)
	}
	if err != nil {
		return err
	}
	fmt.Println(stdout)
	return nil
}

// netCheck TCP 连通性检测(从 Agent 到目标 host:port)。
func netCheck(cfg config, args []string) error {
	agentID := extractArg(args, "-a")
	target := posArg(args, 1)
	if agentID == "" || target == "" {
		return fmt.Errorf("用法: go-rmm net check -a <agent> <host:port>")
	}
	parts := strings.Split(target, ":")
	if len(parts) != 2 {
		return fmt.Errorf("target 格式应为 host:port")
	}
	host := parts[0]
	port := parts[1]
	// 平台感知:Linux 用 bash 内建 /dev/tcp(零依赖),Windows 用 TcpClient
	// (比 Test-NetConnection 更可靠 + 快)
	var stdout string
	var err error
	if isLinuxAgent(cfg, agentID) {
		stdout, err = runOpsScript(cfg, agentID, fmt.Sprintf("timeout 3 bash -c 'echo > /dev/tcp/%s/%s' 2>&1 && echo 'OPEN' || echo 'CLOSED/FILTERED'", host, port), 10)
	} else {
		stdout, err = runOpsTask(cfg, agentID, fmt.Sprintf("try{$c=New-Object System.Net.Sockets.TcpClient;$c.Connect('%s',%s);$c.Close();'OPEN'}catch{'CLOSED: '+$_.Exception.Message}", host, port), 10)
	}
	if err != nil {
		return err
	}
	if jsonMode {
		result := map[string]string{"target": target, "result": strings.TrimSpace(stdout)}
		data, _ := json.Marshal(result)
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("TCP 连通性检测: Agent → %s\n", target)
	fmt.Println(stdout)
	return nil
}
