// Package executors — 项目规则门禁(RulesGate)与规则文件发现。
//
// 机制:被部署项目可在项目根放 .reins/rules.md 写定制运维规则(如
// "勿在营业时段杀壳进程"/"部署排除 runtime 目录"/"重启用 Register-ScheduledTask")。
// Agent 开启 rules_gate 后,变更类任务(powershell/script/browse/guiclick/
// update/power)必须携带与当前规则内容一致的 rules_ack 才执行——
// 规则文件一旦变更,旧 ack 立即失效,强制操作方(AI/自动化)重新
// `reins rules read` 加载规则。未发现任何规则文件时不拦截(向后兼容:
// 没写规则的项目不受影响);file/screenshot/monitor 任务豁免(读规则与
// 只读观测通道,防鸡生蛋死锁)。
//
// ack 语义:规则内容的 SHA256 指纹(前 16 hex)。单文件 = 该文件指纹;
// 多文件 = 各文件指纹的组合指纹。门禁校验接受组合指纹或任一单文件
// 指纹——AI 在本地仓库读到的是单项目规则,不应被同机其他项目的
// 规则文件卡死;组合指纹则证明读全了所有项目规则。
package executors

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QiuShichang/reins/proto"
)

// 规则文件约定:项目根下 .reins/rules.md。
const (
	RulesDirName  = ".reins"
	RulesFileName = "rules.md"
	// rulesMaxFileBytes 单个规则文件大小上限(规则是给人/AI 读的文档,1MB 绰绰有余;
	// 防被当成数据通道滥用)。
	rulesMaxFileBytes = 1024 * 1024
	// rulesDiscoverDepth 自动发现的最大深度(root 下第 2 层目录内的 .reins,
	// 即 D:\proj\.reins 与 D:\group\proj\.reins 均可发现;更深的不扫)。
	rulesDiscoverDepth = 2
	// rulesCacheTTL 发现结果缓存时长。门禁对每个变更类任务都要校验,
	// 全量目录扫描有成本;短 TTL 保证"规则改了马上生效"。
	rulesCacheTTL = 30 * time.Second
)

// rulesSkipDirs 自动发现时跳过的目录名(大小写不敏感)——
// 系统目录/包目录/版本库内部要么无项目规则,要么扫描代价极高。
var rulesSkipDirs = map[string]bool{
	"windows": true, "program files": true, "program files (x86)": true,
	"programdata": true, "$recycle.bin": true, "system volume information": true,
	"perflogs": true, "node_modules": true, ".git": true, ".hg": true, ".svn": true,
	"appdata": true, "reins-chunks": true,
}

// gatedTaskTypes 受门禁约束的任务类型(会改变远端状态的执行类)。
// file(读规则必经)/screenshot/monitor(只读观测)豁免。
var gatedTaskTypes = map[string]bool{
	proto.TaskPowerShell: true,
	proto.TaskScript:     true,
	proto.TaskBrowse:     true,
	proto.TaskGUIClick:   true,
	proto.TaskUpdate:     true,
	proto.TaskPower:      true,
}

// IsGatedTaskType 判断任务类型是否受规则门禁约束。
func IsGatedTaskType(t string) bool { return gatedTaskTypes[t] }

// RuleFileEntry 单个规则文件的读取结果。
type RuleFileEntry struct {
	Path   string `json:"path"`            // 远端绝对路径
	Bytes  int    `json:"bytes"`           // 内容字节数
	SHA16  string `json:"sha256_16"`       // 内容 SHA256 前 16 hex(单文件 ack)
	Exists bool   `json:"exists"`          // 指定路径模式:文件是否存在
	Error  string `json:"error,omitempty"` // 单文件读失败原因(Exists=false 时)
	// Content 规则全文(op=rules 响应携带;RulesGate 校验路径不填充以省内存)。
	Content string `json:"content,omitempty"`
}

// RulesAckForContent 计算内容的规则指纹(SHA256 前 16 hex)。
// 单文件 ack 的唯一算法——CLI 本地 .reins/rules.md 模式复用同一公式,
// 本地与远端同内容即同 ack。此公式冻结,变更会破坏所有已缓存 ack。
func RulesAckForContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])[:16]
}

// RulesAckCombined 多文件组合指纹:按路径排序后拼接 "path\nsha16\n" 再取
// SHA256 前 16 hex。单文件时等价于其自身 sha16(调用方也可直接用单值)。
func RulesAckCombined(entries []RuleFileEntry) string {
	if len(entries) == 1 {
		return entries[0].SHA16
	}
	sorted := make([]RuleFileEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var b strings.Builder
	for _, e := range sorted {
		b.WriteString(e.Path)
		b.WriteByte('\n')
		b.WriteString(e.SHA16)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}

// RulesGate 规则门禁:发现规则文件 + 校验任务 ack。
//
// 并发安全:Dispatch 在任务 goroutine 中并发调用,发现缓存用互斥锁保护。
type RulesGate struct {
	Enabled bool // 是否启用门禁(仅约束 gatedTaskTypes)

	roots     []string // 自动发现的范围(通常是 FilePolicy.AllowedRoots)
	workspace string

	mu         sync.Mutex
	cachedAt   time.Time // 缓存时刻(零值=未扫描)
	cachedList []string  // 发现的规则文件绝对路径
	cachedAck  string    // 组合指纹("" = 无规则文件)
	cachedOK   []string  // 各文件单指纹(接受任一)
}

// NewRulesGate 构造门禁。roots 为自动发现范围,workspace 额外纳入
// (部署工作区里的项目也算)。Enabled=false 时 Check 恒放行。
func NewRulesGate(enabled bool, roots []string, workspace string) *RulesGate {
	all := make([]string, 0, len(roots)+1)
	all = append(all, roots...)
	if workspace != "" {
		all = append(all, workspace)
	}
	return &RulesGate{Enabled: enabled, roots: dedupDirs(all), workspace: workspace}
}

// discover 发现范围内的规则文件(带 TTL 缓存)。force 跳过缓存重扫。
func (g *RulesGate) discover(force bool) (list []string, stale bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !force && !g.cachedAt.IsZero() && time.Since(g.cachedAt) < rulesCacheTTL {
		return g.cachedList, false
	}
	// 扫描在锁内执行:规则目录都很浅(深度 2),毫秒级,不值得为它引入
	// 双扫竞态。持锁扫描同时天然串行化并发首查。
	files := DiscoverRuleFiles(g.roots)
	g.cachedAt = time.Now()
	g.cachedList = files
	return files, false
}

// snapshot 读取发现的规则文件并计算指纹组。读文件失败的单条跳过
// (权限/占用是暂时态,不因一个坏文件废掉整个门禁;该文件内容视为
// 未知,不参与 ack——操作方 rules read 也读不到它,两端一致)。
func (g *RulesGate) snapshot() (entries []RuleFileEntry, ack string, ok []string) {
	files, _ := g.discover(false)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil || len(data) > rulesMaxFileBytes {
			continue
		}
		entries = append(entries, RuleFileEntry{
			Path: f, Bytes: len(data), SHA16: RulesAckForContent(data),
		})
	}
	if len(entries) == 0 {
		return nil, "", nil
	}
	for _, e := range entries {
		ok = append(ok, e.SHA16)
	}
	return entries, RulesAckCombined(entries), ok
}

// Check 校验任务是否携带了当前有效的规则 ack。
// 放行条件(全部满足):门禁启用 ∧ 任务类型受约束 ∧ 存在规则文件。
// 此时 ack 必须等于组合指纹或任一单文件指纹。
// 拒绝信息刻意不回显当前指纹——直接给出答案会诱使调用方跳过"读规则"
// 直接重放指纹,违背"先加载规则再操作"的设计意图。
func (g *RulesGate) Check(taskType, ack string) error {
	if g == nil || !g.Enabled || !gatedTaskTypes[taskType] {
		return nil
	}
	_, combined, singles := g.snapshot()
	if len(singles) == 0 {
		return nil // 机器上没有任何规则文件:无规则可加载,不拦
	}
	if ack != "" && ack == combined {
		return nil
	}
	for _, s := range singles {
		if ack == s {
			return nil
		}
	}
	return fmt.Errorf(
		"规则门禁: 任务被拒——目标机器存在项目规则 .reins/rules.md,须先加载并遵守后再操作。\n" +
			"  请执行: reins rules read -a <agent_id>  (读取规则全文并自动缓存 ack)\n" +
			"  之后重试本命令会自动携带 rules_ack;规则文件变更后 ack 失效,需重新读取")
}

// Refresh 强制重扫(测试与规则文件刚部署后的即时生效入口)。
func (g *RulesGate) Refresh() { g.discover(true) }

// CurrentAcks 返回当前 (组合指纹, 各单文件指纹)。测试与诊断用。
func (g *RulesGate) CurrentAcks() (combined string, singles []string) {
	_, combined, singles = g.snapshot()
	return combined, singles
}

// DiscoverRuleFiles 在 roots 范围内做限深规则文件发现。
// 返回按字典序排序的绝对路径列表(确定性输出,ack 组合才稳定)。
//
// 深度语义:root 本身的 .reins 算深度 0;root/<d1>/<d2>/.reins/rules.md
// 算深度 2(rulesDiscoverDepth 内)。跳过系统/包管理目录(rulesSkipDirs)。
func DiscoverRuleFiles(roots []string) []string {
	var found []string
	seen := map[string]bool{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if seen[filepath.Clean(abs)] {
			continue
		}
		seen[filepath.Clean(abs)] = true
		walkForRules(abs, 0, &found)
	}
	sort.Strings(found)
	// 多根可能交叉命中同一文件(如 allowed_roots 含 D:\projects 而自动追加
	// 的 exeDir 又以深度 2 覆盖到同一 .reins)——对结果去重,ack 组合才稳定。
	if len(found) > 1 {
		deduped := found[:0]
		for i, f := range found {
			if i == 0 || f != found[i-1] {
				deduped = append(deduped, f)
			}
		}
		found = deduped
	}
	return found
}

// walkForRules 递归限深扫描:当前目录有 .reins/rules.md 则收录,
// 并按深度继续下探(规则文件与子目录扫描独立——命中后仍继续找兄弟项目)。
func walkForRules(dir string, depth int, found *[]string) {
	candidate := filepath.Join(dir, RulesDirName, RulesFileName)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Size() <= rulesMaxFileBytes {
		abs, err := filepath.Abs(candidate)
		if err == nil {
			*found = append(*found, abs)
		}
	}
	if depth >= rulesDiscoverDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if rulesSkipDirs[strings.ToLower(e.Name())] {
			continue
		}
		walkForRules(filepath.Join(dir, e.Name()), depth+1, found)
	}
}

// readRuleFile 读取单个规则文件并构造 entry(Path 指定模式)。
// 存在性由调用方决定如何呈现(指定路径不存在 ≠ 门禁开启)。
func readRuleFile(path string) (RuleFileEntry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return RuleFileEntry{}, fmt.Errorf("规则文件不存在: %s", path)
	}
	if info.IsDir() {
		return RuleFileEntry{}, fmt.Errorf("规则路径是目录: %s", path)
	}
	if info.Size() > rulesMaxFileBytes {
		return RuleFileEntry{}, fmt.Errorf("规则文件超过 %d 字节上限: %s", rulesMaxFileBytes, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RuleFileEntry{}, fmt.Errorf("读取规则文件失败: %w", err)
	}
	return RuleFileEntry{
		Path:    path,
		Bytes:   len(data),
		SHA16:   RulesAckForContent(data),
		Content: string(data),
	}, nil
}

// dedupDirs 去重目录列表(保持顺序)。
func dedupDirs(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		c := filepath.Clean(s)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, s)
	}
	return out
}
