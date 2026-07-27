// CLI shell completion(M6 神级提升)。
//
// 生成 bash/zsh/powershell 自动补全脚本。
// 零依赖实现:生成标准 completion 脚本,各 shell 原生支持。
package main

import (
	"fmt"
	"os"
)

// cmdCompletion completion 子命令。
func cmdCompletion(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: go-rmm completion <bash|zsh|powershell>")
	}
	shell := args[0]
	switch shell {
	case "bash":
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
	case "powershell", "pwsh":
		fmt.Print(powershellCompletion)
	case "fish":
		fmt.Print(fishCompletion)
	default:
		return fmt.Errorf("未知 shell: %s (可用: bash zsh powershell fish)", shell)
	}
	fmt.Fprintf(os.Stderr, "\n# 安装方法(写到文件后 source):\n")
	fmt.Fprintf(os.Stderr, "#   %s completion %s > ~/.go-rmm-completion.%s\n", os.Args[0], shell, shell)
	fmt.Fprintf(os.Stderr, "#   source ~/.go-rmm-completion.%s\n", shell)
	return nil
}

// 主命令列表(补全用)。
var mainCommands = []string{
	"agent", "task", "file", "shell", "cron", "deploy", "tunnel",
	"update", "sysinfo", "service", "proc", "power", "monitor",
	"net", "logs", "sync", "watch", "exec", "find", "config",
	"snapshot", "tag", "events", "topology", "diagnose", "compare", "group",
	"workflow", "alert", "health-check", "inventory", "history",
	"status", "completion", "version",
}

var bashCompletion = fmt.Sprintf(`# go-rmm bash completion
_go_rmm() {
    local cur prev cmds
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    cmds="%s"

    if [ $COMP_CWORD -eq 1 ]; then
        COMPREPLY=( $(compgen -W "$cmds" -- "$cur") )
        return 0
    fi

    case "${COMP_WORDS[1]}" in
        agent)
            COMPREPLY=( $(compgen -W "list revoke enroll-token" -- "$cur") )
            ;;
        task)
            COMPREPLY=( $(compgen -W "run list result cancel batch" -- "$cur") )
            ;;
        file)
            COMPREPLY=( $(compgen -W "upload download" -- "$cur") )
            ;;
        shell)
            COMPREPLY=( $(compgen -W "open" -- "$cur") )
            ;;
        cron)
            COMPREPLY=( $(compgen -W "list add delete" -- "$cur") )
            ;;
        deploy)
            COMPREPLY=( $(compgen -W "run validate" -- "$cur") )
            ;;
        update)
            COMPREPLY=( $(compgen -W "upload list push rollback" -- "$cur") )
            ;;
        service)
            COMPREPLY=( $(compgen -W "list start stop restart" -- "$cur") )
            ;;
        proc)
            COMPREPLY=( $(compgen -W "list kill" -- "$cur") )
            ;;
        power)
            COMPREPLY=( $(compgen -W "shutdown restart sleep lock" -- "$cur") )
            ;;
        monitor)
            COMPREPLY=( $(compgen -W "top cpu mem disk net gpu svc" -- "$cur") )
            ;;
        events)
            COMPREPLY=( $(compgen -W "--json --filter" -- "$cur") )
            ;;
        task)
            COMPREPLY=( $(compgen -W "run stream list result cancel batch" -- "$cur") )
            ;;
        tunnel)
            COMPREPLY=( $(compgen -W "-L" -- "$cur") )
            ;;
    esac
    return 0
}
complete -F _go_rmm go-rmm
`, spaceJoin(mainCommands))

var zshCompletion = fmt.Sprintf(`#compdef go-rmm
# go-rmm zsh completion
_go-rmm() {
    local -a commands
    commands=(
        'agent:Agent 管理'
        'task:任务管理'
        'file:文件传输'
        'shell:交互式终端'
        'cron:定时任务'
        'deploy:部署编排'
        'tunnel:端口转发'
        'update:自更新'
        'sysinfo:系统体检'
        'service:服务管理'
        'proc:进程管理'
        'power:电源操作'
        'monitor:资源监控'
        'status:Relay 状态'
        'completion:生成补全脚本'
        'version:版本'
    )
    _describe 'command' commands
}
_go-rmm "$@"
`)

var powershellCompletion = `# go-rmm PowerShell completion
Register-ArgumentCompleter -Native -CommandName go-rmm -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $commands = @(
        'agent', 'task', 'file', 'shell', 'cron', 'deploy', 'tunnel',
        'update', 'sysinfo', 'service', 'proc', 'power', 'monitor',
        'status', 'completion', 'version'
    )
    $commands | Where-Object { $_ -like "$wordToComplete*" } |
        ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
}
`

var fishCompletion = fmt.Sprintf(`# go-rmm fish completion
complete -c go-rmm -f
complete -c go-rmm -n '__fish_use_subcommand' -a '%s' -d 'go-rmm command'
`, spaceJoin(mainCommands))

func spaceJoin(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
