// CLI 退出码定义。
//
// B6/B7 修复:之前所有错误都 exit 1,脚本无法区分"任务失败"vs"网络错误"vs"超时"。
// 现在按语义区分,便于 shell 脚本 $? 判断:
//
//	0  成功
//	1  一般错误(参数错、网络错、未知)
//	2  任务执行失败(Agent 返回 success=false 或非零退出码)
//	3  等待超时或被中断(任务可能仍在运行)
//	4  资源未找到(agent 不在线、task 不存在)
//	5  策略拦截(Relay 命令策略或文件策略拒绝)
package main

const (
	exitOK       = 0
	exitGeneric  = 1
	exitTaskFail = 2
	exitTimeout  = 3
	exitNotFound = 4
	exitBlocked  = 5
)

// exitError 携带期望的退出码。
// main() 检测 error 是否为此类型,决定退出码。
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func newExit(code int, msg string) error {
	return &exitError{code: code, msg: msg}
}

// 任务失败(Agent 返回失败结果)
func errTaskFailed(msg string) error {
	return &exitError{code: exitTaskFail, msg: msg}
}

// 等待超时/中断
func errWaitTimeout(msg string) error {
	return &exitError{code: exitTimeout, msg: msg}
}

// 资源未找到
func errNotFound(msg string) error {
	return &exitError{code: exitNotFound, msg: msg}
}

// 策略拦截
func errBlocked(msg string) error {
	return &exitError{code: exitBlocked, msg: msg}
}
