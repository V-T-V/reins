#!/usr/bin/env bash
# verify.sh — go-rmm 验证技能驱动器（服务形态：loopback relay + agent 真实回路）
#
# 形态：本地构建 relay+agent 真二进制 → loopback 起 relay（-token 生产模式，无 -dev）
# → agent 前台模式反向 WS 上线 → HTTP 探针断言（鉴权契约/Agent 清单/真实任务执行/审计 hash chain）。
#
# 用法（在任意 cwd 下均可，脚本自己解析仓根）：
#   bash .zcode/skills/verify-go-rmm/verify.sh doctor              # build+vet+proto 快测
#   bash .zcode/skills/verify-go-rmm/verify.sh launch [--port N]   # 构建+起 relay+agent（功能性就绪轮询）
#   bash .zcode/skills/verify-go-rmm/verify.sh probe <name>        # healthz|agents-auth|task-run|audit-chain|all
#   bash .zcode/skills/verify-go-rmm/verify.sh cleanup [--dry-run]
#   bash .zcode/skills/verify-go-rmm/verify.sh report [--json]
#
# 退出码：0=PASS  1=FAIL  2=INCONCLUSIVE
#
# ★ 坑位（e2e/README 在案）：relay 的 dataDir 是 **exe 同目录** 的 data/（与 cwd 无关）——
#   本技能每次 launch 构建到全新 runs/bin-<ts>/ 目录，任务/审计数据天然按实例隔离。
# ★ token 是本技能自生成的一次性测试 token（verify-tok-<ts>），非任何真实凭证。
# ★ Windows go.exe 直连 MSYS grep 管道非确定丢数据——go 命令一律落盘再 grep。
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RUNS="$SCRIPT_DIR/runs"
EVIDENCE_ROOT="$RUNS/evidence"
STATE="$RUNS/state.json"
AGENT_ID="verify-agent-01"

RC_PASS=0; RC_FAIL=1; RC_INCONC=2
WORST="PASS"; RUN_DIR=""

init_run() {
  local tag="$1"
  mkdir -p "$EVIDENCE_ROOT"
  RUN_DIR="$EVIDENCE_ROOT/$(date -u +%Y%m%dT%H%M%SZ)-$$-$tag"
  mkdir -p "$RUN_DIR"
  printf '%s\n' "$RUN_DIR" > "$EVIDENCE_ROOT/LAST"
  : > "$RUN_DIR/report.tsv"
}

record() {
  local st="$1" ck="$2" dt="$3"
  printf '%s\t%s\t%s\n' "$st" "$ck" "$dt" >> "$RUN_DIR/report.tsv"
  echo "[$st] $ck — $dt"
  if [ "$st" = "FAIL" ]; then WORST="FAIL"
  elif [ "$st" = "INCONCLUSIVE" ] && [ "$WORST" = "PASS" ]; then WORST="INCONCLUSIVE"
  fi
}

finish() {
  {
    echo "go-rmm verify — $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "worst: $WORST"
    echo "note: $1"
    echo
    cat "$RUN_DIR/report.tsv"
  } > "$RUN_DIR/report.txt"
  echo "# RESULT: $WORST"
  echo "# EVIDENCE: $(cygpath -m "$RUN_DIR" 2>/dev/null || echo "$RUN_DIR")"
  if [ "$WORST" = "PASS" ]; then exit $RC_PASS
  elif [ "$WORST" = "FAIL" ]; then exit $RC_FAIL
  else exit $RC_INCONC; fi
}

find_free_port() { # /dev/tcp 试探：连接失败=空闲（成功=占用，子shell退出自动关fd）
  local p i
  for i in $(seq 1 25); do
    p=$(( (RANDOM % 20000) + 20000 ))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
      echo "$p"; return 0
    fi
  done
  return 1
}

state_get() { [ -f "$STATE" ] && sed -n "s/.*\"$1\":\"\{0,1\}\([^\",}]*\)\"\{0,1\}.*/\1/p" "$STATE" | head -1; }
alive() { kill -0 "$1" 2>/dev/null; }

kill_pid() { # 杀 msys pid 对应的 Windows 进程树
  local m="$1" w
  [ -f "/proc/$m/winpid" ] && w="$(cat "/proc/$m/winpid")" || w="$m"
  taskkill //F //T //PID "$w" >/dev/null 2>&1 || kill -9 "$m" 2>/dev/null || true
}

bodyfile() { # api() 的 body 落盘名——单一出处（两处 tr 映射集不一致曾致轮询断言失明）
  echo "$RUN_DIR/api$(echo "$1" | tr '/?=' '___').body"
}

api() { # api <method> <path> [json_body] —— 带 state 里的一次性 token；body 落 $RUN_DIR
  local m="$1" p="$2" body="${3:-}"
  local out; out="$(bodyfile "$p")"
  local tok; tok="$(state_get token)"
  if [ "$m" = "GET" ]; then
    curl -s --max-time 20 -H "Authorization: Bearer $tok" -o "$out" -w "%{http_code}" "http://127.0.0.1:$(state_get port)$p" 2>/dev/null || echo 000
  else
    curl -s --max-time 20 -X "$m" -H "Authorization: Bearer $tok" -H "content-type: application/json" -d "$body" -o "$out" -w "%{http_code}" "http://127.0.0.1:$(state_get port)$p" 2>/dev/null || echo 000
  fi
}

# ---------------------------------------------------------------- doctor

cmd_doctor() {
  init_run "doctor"
  if ! command -v go >/dev/null 2>&1; then
    record INCONCLUSIVE "toolchain" "go 不在 PATH——下一步：安装 Go 后重跑 doctor"
    finish "环境缺失"
  fi
  go version > "$RUN_DIR/go-version.txt" 2>&1

  ( cd "$REPO_ROOT" && go build ./... > "$RUN_DIR/build.log" 2>&1 )
  if [ $? -eq 0 ]; then record PASS "go build ./..." "rc=0（7 核心包）"
  else record FAIL "go build ./..." "rc!=0——看 $RUN_DIR/build.log；下一步：先修编译再 launch"
  fi

  ( cd "$REPO_ROOT" && go vet ./... > "$RUN_DIR/vet.log" 2>&1 )
  if [ $? -eq 0 ]; then record PASS "go vet ./..." "rc=0"
  else record FAIL "go vet ./..." "rc!=0——看 $RUN_DIR/vet.log"
  fi

  ( cd "$REPO_ROOT" && go test ./proto/ -count=1 > "$RUN_DIR/test-proto.log" 2>&1 )
  if [ $? -eq 0 ]; then record PASS "go test ./proto/" "rc=0（快测档；relay 全包约 10s+、14 包全量按需手动）"
  else record FAIL "go test ./proto/" "rc!=0——看 $RUN_DIR/test-proto.log"
  fi

  local mpid
  mpid="$(state_get relay_msyspid)"
  if [ -n "$mpid" ] && alive "$mpid"; then
    local c; c="$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" "http://127.0.0.1:$(state_get port)/healthz" 2>/dev/null || echo 000)"
    if [ "$c" = "200" ]; then record PASS "活实例探活" "/healthz 200（port=$(state_get port)）"
    else record FAIL "活实例探活" "/healthz=$c——下一步：看 $RUNS/relay.log 或 cleanup 重 launch"
    fi
  else
    record PASS "活实例探活" "无活实例（launch 后 probe）"
  fi
  finish "doctor 完成"
}

# ---------------------------------------------------------------- launch

cmd_launch() {
  init_run "launch"
  local port="${1:-}"
  if [ -n "$(state_get relay_msyspid)" ] && alive "$(state_get relay_msyspid)"; then
    record INCONCLUSIVE "launch 拒绝" "已有活实例（port=$(state_get port)）——下一步：先 cleanup 再 launch（防状态覆盖）"
    finish "已有实例"
  fi
  if [ -z "$port" ]; then
    port="$(find_free_port)" || { record INCONCLUSIVE "动态端口" "25 次试探无空闲端口——下一步：重跑或 --port 显式指定"; finish "端口分配失败"; }
  fi
  local tok="verify-tok-$(date +%s)"
  local bindir="$RUNS/bin-$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "$bindir"

  # 1) 构建真二进制（bin 目录即 dataDir 宿主——全新目录=任务/审计数据按实例隔离，e2e 坑位的根治版）
  ( cd "$REPO_ROOT" && go build -o "$bindir/relay.exe" ./relay > "$RUN_DIR/build.log" 2>&1 \
    && go build -o "$bindir/agent.exe" ./agent >> "$RUN_DIR/build.log" 2>&1 ) || {
    record FAIL "构建 relay+agent" "看 $RUN_DIR/build.log；下一步：先 doctor 修编译"
    finish "构建失败"
  }
  record PASS "构建 relay+agent" "$bindir（dataDir=exe 同目录——全新目录天然隔离）"

  # 2) 起 relay：-addr loopback + -token 生产模式（无 -dev）；外层重定向防调用管道挂等
  ( cd "$bindir" && exec ./relay.exe -addr "127.0.0.1:$port" -token "$tok" > "$RUNS/relay.log" 2>&1 ) >/dev/null 2>&1 &
  local rpid=$!
  local c ok=0 i
  for i in $(seq 1 30); do
    c="$(curl -s --max-time 3 -o /dev/null -w "%{http_code}" "http://127.0.0.1:$port/healthz" 2>/dev/null || echo 000)"
    [ "$c" = "200" ] && { ok=1; break; }
    alive "$rpid" || break
    sleep 1
  done
  if [ "$ok" -ne 1 ]; then
    kill_pid "$rpid"
    record FAIL "relay 就绪" "/healthz 30s 内未 200（last=$c）——看 $RUNS/relay.log"
    finish "relay 起动失败"
  fi
  record PASS "relay 就绪" "/healthz 200（port=$port，-token 生产模式，data $bindir/data）"

  # 3) 起 agent：前台模式=AGENT-SETUP.md「手动方式」（与 Windows 服务安装同一条注册链路，免服务特权）
  ( cd "$bindir" && exec ./agent.exe run -relay "ws://127.0.0.1:$port/agent" -token "$tok" -id "$AGENT_ID" > "$RUNS/agent.log" 2>&1 ) >/dev/null 2>&1 &
  local apid=$!
  # 4) 功能性就绪：/api/agents 出现本 agent（装机流闭环：WS 注册→Registry→API；Go log 走 stderr 不作就绪信号）
  ok=0
  for i in $(seq 1 30); do
    c="$(curl -s --max-time 3 -H "Authorization: Bearer $tok" -o "$RUN_DIR/launch-agents.body" -w "%{http_code}" "http://127.0.0.1:$port/api/agents" 2>/dev/null || echo 000)"
    if [ "$c" = "200" ] && grep -q "\"$AGENT_ID\"" "$RUN_DIR/launch-agents.body" 2>/dev/null; then ok=1; break; fi
    alive "$apid" || break
    sleep 1
  done
  if [ "$ok" -ne 1 ]; then
    kill_pid "$apid"; kill_pid "$rpid"
    record FAIL "agent 上线" "/api/agents 30s 内未见 $AGENT_ID（last=$c）——看 $RUNS/agent.log 与 $RUNS/relay.log"
    finish "agent 上线失败"
  fi
  printf '{"relay_msyspid":"%s","agent_msyspid":"%s","port":"%s","token":"%s","bindir":"%s"}\n' \
    "$rpid" "$apid" "$port" "$tok" "$(cygpath -m "$bindir")" > "$STATE"
  record PASS "agent 上线" "$AGENT_ID 经反向 WS 注册（/api/agents 可见）——可开始 probe"
  finish "launch 完成"
}

# ---------------------------------------------------------------- probe

probe_healthz() {
  local port; port="$(state_get port)"
  local c out="$RUN_DIR/healthz-noauth.body"
  c="$(curl -s --max-time 10 -o "$out" -w "%{http_code}" "http://127.0.0.1:$port/healthz" 2>/dev/null || echo 000)"
  if [ "$c" = "200" ] && grep -q '"status":"ok"' "$out" 2>/dev/null && ! grep -q '"uptime_s"' "$out" 2>/dev/null; then
    record PASS "healthz 无鉴权浅形状" "200 且只 status=ok（无 agents/uptime 字段——P1-S7 安全设计：不泄漏组件信息）"
  else
    record FAIL "healthz 无鉴权浅形状" "HTTP $c——看 $out；浅形状若变深=未授权信息泄漏面扩大"
  fi
  local tok; tok="$(state_get token)"
  c="$(curl -s --max-time 10 -H "Authorization: Bearer $tok" -o "$RUN_DIR/healthz-auth.body" -w "%{http_code}" "http://127.0.0.1:$port/healthz" 2>/dev/null || echo 000)"
  if [ "$c" = "200" ] && grep -q '"relay":"ok"' "$RUN_DIR/healthz-auth.body" 2>/dev/null \
     && grep -q '"agents":1' "$RUN_DIR/healthz-auth.body" 2>/dev/null \
     && grep -q '"uptime_s"' "$RUN_DIR/healthz-auth.body" 2>/dev/null; then
    record PASS "healthz 带鉴权深形状" "200 + relay=ok + agents=1 + uptime_s（深度健康对有效 Bearer 可见）"
  else
    record FAIL "healthz 带鉴权深形状" "HTTP $c——看 $RUN_DIR/healthz-auth.body"
  fi
}

probe_agents_auth() {
  local port tok c out
  port="$(state_get port)"; tok="$(state_get token)"
  out="$RUN_DIR/agents-badtoken.body"
  c="$(curl -s --max-time 10 -H "Authorization: Bearer wrong-token-xxx" -o "$out" -w "%{http_code}" "http://127.0.0.1:$port/api/agents" 2>/dev/null || echo 000)"
  if [ "$c" = "401" ] && grep -q '"error":"无效的鉴权 token"' "$out" 2>/dev/null; then
    record PASS "错 token 401 结构化" "401 + error='无效的鉴权 token'（错误体逐字段）"
  else
    record FAIL "错 token 401 结构化" "HTTP $c——看 $out；401 变 2xx=鉴权被绕过（安全回归）"
  fi
  c="$(api GET /api/agents)"
  if [ "$c" = "200" ] && grep -q "\"$AGENT_ID\"" "$RUN_DIR/api_api_agents.body" 2>/dev/null && grep -q '"count":1' "$RUN_DIR/api_api_agents.body" 2>/dev/null; then
    record PASS "对 token Agent 清单" "200 含 $AGENT_ID 且 count=1"
  else
    record FAIL "对 token Agent 清单" "HTTP $c——看 $RUN_DIR/api_api_agents.body"
  fi
}

probe_task_run() { # loopback agent 真实执行 powershell（唯一 marker 防 dataDir 累积误判）
  local marker="verify-go-rmm-$(date +%s)" c i
  c="$(api POST /api/tasks "{\"task_id\":\"$marker\",\"type\":\"powershell\",\"agent_id\":\"$AGENT_ID\",\"payload\":{\"command\":\"Write-Output '$marker'\"},\"timeout\":60}")"
  echo "$marker" > "$RUNS/last-marker"
  if [ "$c" != "200" ] && [ "$c" != "202" ]; then
    record FAIL "task 下发" "HTTP $c（期望 200/202 dispatched）——看 $RUN_DIR/api_api_tasks.body"
    return
  fi
  if ! grep -q '"task_id":"'"$marker"'"' "$(bodyfile /api/tasks)" 2>/dev/null; then
    record FAIL "task 下发" "响应缺 task_id=$marker——看 $(bodyfile /api/tasks)"
    return
  fi
  # 轮询到 completed（真信号；不裸 sleep）
  local tbody; tbody="$(bodyfile "/api/tasks/$marker")"
  local done=0
  for i in $(seq 1 45); do
    c="$(api GET "/api/tasks/$marker")"
    if grep -q '"status":"completed"' "$tbody" 2>/dev/null; then done=1; break; fi
    sleep 1
  done
  if [ "$done" -ne 1 ]; then
    record FAIL "task 终态" "45s 未 completed——看 $tbody 与 $RUNS/agent.log"
    return
  fi
  if grep -q '"success":true' "$tbody" 2>/dev/null && grep -q "$marker" "$tbody" 2>/dev/null; then
    record PASS "task 真实执行" "powershell Write-Output → completed + stdout 含唯一 marker $marker（loopback agent 真执行非 mock）"
  else
    record FAIL "task 结果" "completed 但 success/stdout 异常——看 $tbody"
  fi
}

probe_audit_chain() { # 审计 hash chain 衔接核对：task_result.prev_hash == task_submitted.this_hash
  local marker c
  marker="$(cat "$RUNS/last-marker" 2>/dev/null)"
  if [ -z "$marker" ]; then
    record INCONCLUSIVE "audit-chain" "无 last-marker——下一步：先 probe task-run"
    return
  fi
  c="$(api GET "/api/audit/query?limit=10")"
  local body; body="$(bodyfile "/api/audit/query?limit=10")"
  if [ "$c" != "200" ]; then
    record FAIL "audit query" "HTTP $c——看 $body"
    return
  fi
  # 本任务两事件在场（task_id 与本 run 唯一 marker 绑定——dataDir 虽累积但 marker 唯一）
  local n_sub n_res
  n_sub="$(grep -c '"event":"task_submitted"' "$body" || true)"
  n_res="$(grep -c '"event":"task_result"' "$body" || true)"
  if [ "$n_sub" -lt 1 ] || [ "$n_res" -lt 1 ] || ! grep -q "$marker" "$body"; then
    record FAIL "audit 落账" "task_submitted=$n_sub task_result=$n_res marker 在场=$(grep -cq "$marker" "$body" && echo y || echo n)——看 $body"
    return
  fi
  # newest-first：第 1 条 prev_hash 应等于第 2 条 this_hash（hash chain 衔接逐位）
  local h1 h2
  h1="$(grep -o '"prev_hash":"[a-f0-9]*"' "$body" | head -1 | sed 's/.*:"//')"
  h2="$(grep -o '"this_hash":"[a-f0-9]*"' "$body" | sed -n 2p | sed 's/.*:"//')"
  if [ -n "$h1" ] && [ "$h1" = "$h2" ]; then
    record PASS "audit hash chain 衔接" "task_result.prev_hash == task_submitted.this_hash（${h1:0:16}…，防篡改链完整）"
  else
    record FAIL "audit hash chain 衔接" "prev=${h1:0:16}… vs this=${h2:0:16}… 不等——看 $body；下一步：核对 audit 写入序（newest-first 假设可能漂移）"
  fi
}

cmd_probe() {
  local name="${1:-}"
  [ -z "$name" ] && { echo "用法：verify.sh probe <healthz|agents-auth|task-run|audit-chain|all>——下一步：先 launch" >&2; exit $RC_INCONC; }
  init_run "probe-${name}"
  local mpid; mpid="$(state_get relay_msyspid)"
  if [ -z "$mpid" ] || ! alive "$mpid"; then
    record INCONCLUSIVE "probe $name" "无活 relay 实例——下一步：先 verify.sh launch"
    finish "无实例"
  fi
  case "$name" in
    healthz)    probe_healthz;;
    agents-auth) probe_agents_auth;;
    task-run)   probe_task_run; probe_audit_chain;;
    audit-chain) probe_audit_chain;;
    all)        probe_healthz; probe_agents_auth; probe_task_run; probe_audit_chain;;
    *) record INCONCLUSIVE "probe $name" "未知探针名（healthz|agents-auth|task-run|audit-chain|all）";;
  esac
  finish "probe $name 完成"
}

# ---------------------------------------------------------------- report / cleanup

cmd_report() {
  if [ ! -f "$EVIDENCE_ROOT/LAST" ] || [ ! -d "$(cat "$EVIDENCE_ROOT/LAST")" ]; then
    echo "INCONCLUSIVE：尚无运行记录——下一步：先跑 verify.sh doctor" >&2
    exit $RC_INCONC
  fi
  local d; d="$(cat "$EVIDENCE_ROOT/LAST")"
  if [ "${1:-}" = "--json" ]; then
    printf '{"run":"%s","result":"%s","checks":[\n' "$(cygpath -m "$d" 2>/dev/null || echo "$d")" "$(sed -n 's/^worst: //p' "$d/report.txt")"
    local first=1
    while IFS=$'\t' read -r st ck dt; do
      [ $first -eq 0 ] && printf ',\n'
      printf '{"status":"%s","check":"%s","detail":"%s"}' "$st" "$ck" "$(printf '%s' "$dt" | sed 's/"/\\"/g')"
      first=0
    done < "$d/report.tsv"
    printf '\n]}\n'
  else
    cat "$d/report.txt"
    echo "# EVIDENCE: $(cygpath -m "$d" 2>/dev/null || echo "$d")"
  fi
  local worst; worst="$(sed -n 's/^worst: //p' "$d/report.txt")"
  [ "$worst" = "PASS" ] && exit $RC_PASS
  [ "$worst" = "FAIL" ] && exit $RC_FAIL
  exit $RC_INCONC
}

cmd_cleanup() {
  local dry=0
  [ "${1:-}" = "--dry-run" ] && dry=1
  init_run "cleanup"
  local apid rpid port bindir
  apid="$(state_get agent_msyspid)"; rpid="$(state_get relay_msyspid)"
  port="$(state_get port)"; bindir="$(state_get bindir)"
  if { [ -n "$apid" ] && alive "$apid"; } || { [ -n "$rpid" ] && alive "$rpid"; }; then
    if [ "$dry" -eq 1 ]; then
      record PASS "cleanup --dry-run" "将 taskkill agent(pid=$apid)+relay(pid=$rpid) 进程树 + 删 bin 目录 $bindir"
    else
      [ -n "$apid" ] && kill_pid "$apid"
      [ -n "$rpid" ] && kill_pid "$rpid"
      sleep 1
      if [ -n "$port" ] && netstat -ano 2>/dev/null | grep -qE "[:.]$port[^0-9].*LISTENING"; then
        record FAIL "cleanup 进程" "port=$port 仍有监听——下一步：netstat -ano | grep :$port 找 PID 手动 taskkill //F //T //PID"
      else
        record PASS "cleanup 进程" "agent+relay 已杀且 port=$port 无监听"
      fi
      if [ -n "$bindir" ] && [ -d "$bindir" ]; then rm -rf "$bindir"; record PASS "cleanup bin 目录" "已删 $bindir（含 dataDir 任务/审计数据——每实例一次性）"; fi
      rm -f "$STATE"
      record PASS "cleanup 运行时产物" "state 已清；relay.log/agent.log 与 evidence 保留"
    fi
  else
    record PASS "cleanup 进程" "无活实例（幂等）"
    [ "${1:-}" != "--dry-run" ] && rm -f "$STATE"
  fi
  # 旧运行目录收敛（上一次验证运行的证据永不删）
  local prev_last=""; [ -f "$EVIDENCE_ROOT/LAST" ] && prev_last="$(cat "$EVIDENCE_ROOT/LAST")"
  local dirs total n victims
  dirs="$(ls -1d "$EVIDENCE_ROOT"/*/ 2>/dev/null | sort)"
  if [ -n "$prev_last" ] && [ -d "$prev_last" ]; then
    dirs="$(printf '%s\n' "$dirs" | grep -v -F "$prev_last" || true)"
  fi
  total="$(printf '%s\n' "$dirs" | grep -c . || true)"
  n=$(( total > 10 ? total - 10 : 0 ))
  victims="$(printf '%s\n' "$dirs" | head -n "$n")"
  if [ -n "${victims// /}" ] && [ "$n" -gt 0 ] && [ "$dry" -ne 1 ]; then
    printf '%s\n' "$victims" | while IFS= read -r v; do rm -rf "$v"; done
    record PASS "cleanup evidence" "已删除 $n 个旧运行目录（保留最近 10）"
  else
    record PASS "cleanup evidence" "无待清理（total=$total）——证据跨 cleanup 留存"
  fi
  finish "cleanup 完成"
}

case "${1:-help}" in
  doctor) shift; cmd_doctor "$@";;
  launch) shift; cmd_launch "$@";;
  probe)  shift; cmd_probe "$@";;
  report) shift; cmd_report "$@";;
  cleanup) shift; cmd_cleanup "$@";;
  help|--help|-h)
    sed -n '2,21p' "${BASH_SOURCE[0]}";;
  *) echo "未知子命令 $1——下一步：bash verify.sh help 看用法" >&2; exit $RC_INCONC;;
esac
