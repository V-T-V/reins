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
	// Windows: Test-Connection; Linux: ping -c N
	cmd := fmt.Sprintf("Test-Connection -ComputerName %s -Count %d | Select-Object Address,ResponseTime,StatusCode | Format-Table -AutoSize | Out-String", target, count)
	stdout, err := runOpsTask(cfg, agentID, cmd, 30)
	if err != nil {
		// 可能是 Linux Agent,用 ping
		cmd = fmt.Sprintf("ping -c %d %s", count, target)
		stdout, err = runOpsTask(cfg, agentID, cmd, 30)
		if err != nil {
			return err
		}
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
	// Windows: Resolve-DnsName; Linux: dig/nslookup
	cmd := fmt.Sprintf("Resolve-DnsName -Name %s | Select-Object Name,Type,IPAddress | Format-Table -AutoSize | Out-String", domain)
	stdout, err := runOpsTask(cfg, agentID, cmd, 15)
	if err != nil {
		cmd = fmt.Sprintf("dig +short %s A 2>/dev/null || nslookup %s", domain, domain)
		stdout, err = runOpsTask(cfg, agentID, cmd, 15)
		if err != nil {
			return err
		}
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
	// Windows: tracert; Linux: traceroute
	cmd := fmt.Sprintf("tracert -d -h 15 %s", target)
	stdout, err := runOpsTask(cfg, agentID, cmd, 60)
	if err != nil {
		cmd = fmt.Sprintf("traceroute -m 15 %s 2>/dev/null || tracepath %s", target, target)
		stdout, err = runOpsTask(cfg, agentID, cmd, 60)
		if err != nil {
			return err
		}
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
	// Windows: Get-NetTCPConnection; Linux: ss/netstat
	cmd := fmt.Sprintf("Get-NetTCPConnection -State %s | Select-Object LocalAddress,LocalPort,State,OwningProcess | Sort LocalPort | Format-Table -AutoSize | Out-String", strings.Title(state))
	stdout, err := runOpsTask(cfg, agentID, cmd, 15)
	if err != nil {
		cmd = fmt.Sprintf("ss -tlnp 2>/dev/null || netstat -tlnp")
		stdout, err = runOpsTask(cfg, agentID, cmd, 15)
		if err != nil {
			return err
		}
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
	// 用 TcpClient(比 Test-NetConnection 更可靠 + 快)
	cmd := fmt.Sprintf("try{$c=New-Object System.Net.Sockets.TcpClient;$c.Connect('%s',%s);$c.Close();'OPEN'}catch{'CLOSED: '+$_.Exception.Message}", host, port)
	stdout, err := runOpsTask(cfg, agentID, cmd, 10)
	if err != nil {
		// Linux fallback
		cmd = fmt.Sprintf("timeout 3 bash -c 'echo > /dev/tcp/%s/%s' 2>&1 && echo 'OPEN' || echo 'CLOSED/FILTERED'", host, port)
		stdout, err = runOpsTask(cfg, agentID, cmd, 10)
		if err != nil {
			return err
		}
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
