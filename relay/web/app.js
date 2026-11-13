// reins 控制台前端 —— 零依赖原生 JS
//
// 状态机：
//   agents: Map<agentID, Register>  当前已知 Agent
//   selected: string|null           当前选中 Agent
//   evtSource: EventSource          SSE 订阅
(function () {
  "use strict";

  // 自动检测 base path（支持反向代理子路径如 /rmm/）
  // 页面 URL 为 https://IP/rmm/ → basePath = "/rmm"
  // 页面 URL 为 https://IP/ → basePath = ""
  var basePath = location.pathname.replace(/\/[^/]*$/, ""); // 剥掉文件名
  if (basePath === "/" || basePath === ".") basePath = "";
  function api(path) { return basePath + path; }

  const state = { agents: new Map(), selected: null, evtSource: null, seenAgentIds: [], token: "" };

  // --- Token 鉴权(Relay -token 模式下控制台必须携带;否则全部 401) ---
  // 优先级:URL ?token= 引导(用后即从地址栏抹掉)> localStorage > 401 时提示输入。
  function getToken() {
    if (state.token) return state.token;
    try { state.token = localStorage.getItem("reins_token") || ""; } catch (e) { state.token = ""; }
    return state.token;
  }
  function setToken(t) {
    state.token = t || "";
    try {
      if (state.token) localStorage.setItem("reins_token", state.token);
      else localStorage.removeItem("reins_token");
    } catch (e) { /* 隐私模式等无 localStorage 环境,仅内存态 */ }
    // 同源 cookie:EventSource(SSE)无法携带 Authorization header,服务端authorize
    // 支持 reins_token cookie 回退(UI-P3;query token 因 H4 安全红线被服务端拒绝)。
    try {
      document.cookie = "reins_token=" + encodeURIComponent(state.token) + "; path=/; SameSite=Strict";
    } catch (e) { /* 非 http(s) 环境等 */ }
  }
  (function consumeUrlToken() {
    try {
      var q = new URLSearchParams(location.search);
      var t = q.get("token");
      if (t) {
        setToken(t);
        q.delete("token");
        var rest = q.toString();
        history.replaceState(null, "", location.pathname + (rest ? "?" + rest : "") + location.hash);
      }
    } catch (e) { /* 老浏览器无 URLSearchParams,跳过 */ }
  })();

  // 统一 API 请求:自动带 Authorization;401 时提示输入 Token 后重试一次。
  // (SSE 不走这里——EventSource 由 setToken 写的同源 cookie 携带凭证。)
  async function apiFetch(path, opts) {
    opts = opts || {};
    var t = getToken();
    if (t) {
      opts.headers = Object.assign({}, opts.headers || {}, { "Authorization": "Bearer " + t });
    }
    var r = await fetch(api(path), opts);
    if (r.status === 401 && !opts.__retriedAuth) {
      var input = window.prompt("鉴权失败:请输入 Relay Token(Relay 启动时 -token 参数)");
      if (input !== null && input !== "") {
        setToken(input);
        opts.__retriedAuth = true;
        return apiFetch(path, opts);
      }
    }
    return r;
  }

  // SSE 地址:凭证由 setToken 写入的同源 cookie 自动携带(无需 query——H4 红线服务端拒绝 query token)。
  function sseURL(path) { return api(path); }

  // --- Agent 列表 ---
  async function refreshAgents() {
    try {
      const r = await apiFetch("/api/agents");
      const data = await r.json();
      // 标记：列表里没有的视为离线
      const onlineIds = new Set();
      (data.agents || []).forEach((a) => {
        state.agents.set(a.agent_id, a);
        onlineIds.add(a.agent_id);
      });
      renderAgentList();
      // P2：0 Agent 时显示安装引导
      var overlay = document.getElementById("setup-overlay");
      if (state.agents.size === 0 && overlay && !overlay.dataset.dismissed) {
        overlay.style.display = "flex";
        // 自动检测 Relay IP（用当前页面的 hostname）
        var relayIP = location.hostname + (location.port ? ":" + location.port : "");
        // 填充 IP 占位符(命令中仅 1 处)
        var ipEl = document.getElementById("relay-ip");
        if (ipEl) ipEl.textContent = relayIP;
        // 从 /api/bootstrap 拉取一行安装命令:安装命令已由服务端烘焙地址,
        // 直接用 bootstrap 返回的一行命令整条替换(Token 场景含 scriptblock 形式);
        // 失败时保留上面的占位命令。
        if (!state.bootstrapLoaded) {
          state.bootstrapLoaded = true;
          apiFetch("/api/bootstrap?host=" + location.hostname + "&port=" + (location.port || "7780"))
            .then(function (r) { return r.ok ? r.json() : null; })
            .then(function (info) {
              var cmdEl = document.getElementById("install-cmd");
              if (cmdEl && info && info.one_line_cmd) {
                cmdEl.textContent = info.one_line_cmd;
              } else if (cmdEl && info && info.one_line_dev_cmd) {
                cmdEl.textContent = info.one_line_dev_cmd;
              }
            })
            .catch(function () { /* dev 模式或无 Token,忽略 */ });
        }
      } else if (overlay && state.agents.size > 0) {
        overlay.style.display = "none";
      }
    } catch (e) {
      console.error("刷新 Agent 失败", e);
    }
  }

  function renderAgentList() {
    const list = document.getElementById("agent-list");
    const count = document.getElementById("agent-count");
    count.textContent = state.agents.size;
    if (state.agents.size === 0) {
      list.innerHTML = '<div class="empty">等待 Agent 上线...</div>';
      return;
    }
    list.innerHTML = "";
    state.agents.forEach((a) => {
      const div = document.createElement("div");
      div.className = "agent-item" + (state.selected === a.agent_id ? " active" : "");
      div.onclick = () => selectAgent(a.agent_id);
      const caps = (a.capabilities || [])
        .map((c) => `<span class="cap">${c}</span>`)
        .join("");
      const tags = Object.entries(a.tags || {})
        .map(([k, v]) => `<span class="agent-tag">${esc(k)}=${esc(v)}</span>`)
        .join("");
      const uptime = a.online_since ? `<div class="agent-uptime">● 在线 ${fmtUptime(Date.now() - a.online_since)}</div>` : "";
      const ver = a.version ? (String(a.version).startsWith("v") ? a.version : "v" + a.version) : "?";
      div.innerHTML = `
        <div class="agent-id"><span class="status-dot"></span>${esc(a.agent_id)}</div>
        <div class="agent-meta">${esc(a.hostname || "")} · ${esc(a.os || "")} · ${esc(ver)}</div>
        ${uptime}
        <div class="caps">${caps}</div>
        ${tags ? `<div>${tags}</div>` : ""}
      `;
      list.appendChild(div);
    });
    // Agent ID datalist 供历史/审计筛选
    const dl = document.getElementById("hist-agent-list");
    if (dl) {
      const ids = new Set(Array.from(state.agents.keys()));
      (state.seenAgentIds || []).forEach((id) => ids.add(id));
      dl.innerHTML = Array.from(ids).map((id) => `<option value="${esc(id)}">`).join("");
    }
  }

  // 在线时长格式化(ms → "3 分"/"2 小时 15 分"/"5 天 3 小时")
  function fmtUptime(ms) {
    if (ms < 0 || ms === undefined || ms === null) return "—";
    const min = Math.floor(ms / 60000);
    if (min < 1) return "不到 1 分钟";
    if (min < 60) return min + " 分钟";
    const h = Math.floor(min / 60);
    if (h < 24) return h + " 小时 " + (min % 60) + " 分";
    return Math.floor(h / 24) + " 天 " + (h % 24) + " 小时";
  }

  function selectAgent(id) {
    state.selected = id;
    renderAgentList();
    renderTaskForm();
  }

  // --- 任务表单（按类型动态渲染字段）---
  function renderTaskForm() {
    const host = document.getElementById("task-form-host");
    if (!state.selected) {
      host.innerHTML = '<div class="empty" id="no-agent-hint">请先在左侧选择一个 Agent</div>';
      return;
    }
    const agent = state.agents.get(state.selected);
    const caps = agent ? agent.capabilities : [];

    host.innerHTML = `
      <div class="row">
        <div>
          <label>任务类型</label>
          <select id="task-type" onchange="window.__renderFields()">
            ${taskTypeOptions(caps)}
          </select>
        </div>
        <div>
          <label>超时(秒)</label>
          <input id="task-timeout" type="number" value="30" min="1" max="600">
        </div>
      </div>
      <div id="task-fields"></div>
      <div style="margin-top:16px">
        <button onclick="window.__submitTask()">下发任务</button>
        <span class="hint">目标 Agent: <strong>${esc(state.selected)}</strong></span>
      </div>
    `;
    window.__renderFields();
  }

  function taskTypeOptions(caps) {
    const all = ["powershell", "script", "file", "screenshot", "browse", "guiclick"];
    // 优先显示该 Agent 支持的，其余也允许（可能 Agent 后续注册）
    return all
      .map((t) => {
        const supported = caps.includes(t);
        return `<option value="${t}"${supported ? "" : " disabled"}>${t}${supported ? "" : " (不支持)"}</option>`;
      })
      .join("");
  }

  window.__renderFields = function () {
    const type = document.getElementById("task-type").value;
    const f = document.getElementById("task-fields");
    f.innerHTML = fieldsHTML(type);
  };

  function fieldsHTML(type) {
    switch (type) {
      case "powershell":
        return `<label>PowerShell 命令</label><textarea id="f-command" placeholder="Write-Output &#39;hello&#39;">Get-Process | Select-Object Name,Id -First 5</textarea>`;
      case "script":
        return `<div class="row"><div><label>语言</label><select id="f-lang"><option value="ps1">ps1</option><option value="bat">bat</option></select></div></div>
          <label>脚本内容</label><textarea id="f-content" placeholder="Write-Output &#39;脚本内容&#39;">Write-Output "脚本测试"</textarea>`;
      case "file":
        return `<div class="row"><div><label>操作</label><select id="f-op"><option value="download">download (读)</option><option value="upload">upload (写)</option></select></div></div>
          <label>目标路径(绝对路径)</label><input id="f-path" placeholder="C:\\\\Users\\\\test.txt">
          <label>内容(base64，仅 upload)</label><textarea id="f-contentb64" placeholder="upload 时填"></textarea>`;
      case "screenshot":
        return `<div class="row"><div><label>目标(进程名/标题)</label><input id="f-target" placeholder="留空=全屏"></div></div>
          <label><input type="checkbox" id="f-full"> 强制全屏</label>
          <label><input type="checkbox" id="f-activate" checked> 截图前激活窗口</label>`;
      case "browse":
        return `<label>URL</label><input id="f-url" placeholder="https://example.com">
          <label>动作(JSON 数组)</label><textarea id="f-actions" placeholder='[{"op":"screenshot"}]'>[{"op":"screenshot"}]</textarea>`;
      case "guiclick":
        return `<div class="row"><div><label>目标窗口(进程名/标题)</label><input id="f-target" placeholder="留空=全屏"></div>
          <div><label>最大步数</label><input id="f-maxsteps" type="number" value="5" min="1" max="20"></div></div>
          <label>意图(自然语言)</label><textarea id="f-intent" placeholder="点击登录按钮">点击登录按钮</textarea>`;
      default:
        return "";
    }
  }

  // --- 任务下发 ---
  window.__submitTask = async function () {
    if (!state.selected) return;
    const type = document.getElementById("task-type").value;
    const timeout = parseInt(document.getElementById("task-timeout").value, 10) || 30;
    const payload = buildPayload(type);
    if (payload === null) return;

    const taskId = "t-" + Date.now() + "-" + Math.random().toString(36).slice(2, 6);
    const body = { task_id: taskId, type, agent_id: state.selected, payload, timeout };
    try {
      const r = await apiFetch("/api/tasks", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      const data = await r.json();
      if (r.ok) {
        appendResult({ pending: true, task_id: taskId, type, ts: Date.now() });
      } else {
        alert("下发失败: " + (data.error || r.status));
      }
    } catch (e) {
      alert("下发异常: " + e.message);
    }
  };

  function buildPayload(type) {
    const val = (id) => (document.getElementById(id) || {}).value || "";
    const chk = (id) => (document.getElementById(id) || {}).checked;
    switch (type) {
      case "powershell":
        return { command: val("f-command") };
      case "script":
        return { lang: val("f-lang"), content: val("f-content") };
      case "file":
        return { op: val("f-op"), path: val("f-path"), content_b64: val("f-contentb64") };
      case "screenshot":
        return { target: val("f-target"), full: chk("f-full"), activate: chk("f-activate") };
      case "browse":
        try {
          return { url: val("f-url"), actions: JSON.parse(val("f-actions") || "[]") };
        } catch (e) {
          alert("actions JSON 解析失败: " + e.message);
          return null;
        }
      case "guiclick":
        return {
          intent: val("f-intent"),
          target: val("f-target"),
          max_steps: parseInt(val("f-maxsteps"), 10) || 5,
        };
      default:
        return {};
    }
  }

  // --- SSE 订阅（D6: 加自动重连）---
  function subscribeSSE() {
    if (state.evtSource) state.evtSource.close();
    const es = new EventSource(sseURL("/api/events"));
    state.evtSource = es;
    es.addEventListener("hello", () => {
      document.getElementById("relay-info").textContent = "Relay: 已连接 ✓";
      // 重连后补拉近期任务结果（防丢事件）
      fetchRecentResults();
    });
    es.addEventListener("agent_online", (e) => {
      const data = JSON.parse(e.data).data;
      state.agents.set(data.agent_id, data);
      renderAgentList();
    });
    es.addEventListener("agent_offline", (e) => {
      const data = JSON.parse(e.data).data;
      const id = data.agent_id;
      state.agents.delete(id);
      if (state.selected === id) state.selected = null;
      renderAgentList();
      renderTaskForm();
    });
    es.addEventListener("task_progress", (e) => {
      const data = JSON.parse(e.data).data;
      updatePending(data.task_id, { progress: data });
    });
    es.addEventListener("task_result", (e) => {
      const data = JSON.parse(e.data).data;
      updatePending(data.task_id, { result: data });
      // 任务历史/审计页打开时实时跟进
      maybeRefreshTables();
    });
    // D6 P2-4 修复：onerror 检测连接关闭后主动重连
    es.onerror = () => {
      document.getElementById("relay-info").textContent = "Relay: 连接断开，3s 后重连...";
      es.close();
      setTimeout(subscribeSSE, 3000);
    };
  }

  // D6：重连后补拉近期任务结果（防 SSE 丢事件）
  async function fetchRecentResults() {
    try {
      const r = await apiFetch("/api/tasks?limit=10");
      const data = await r.json();
      (data.tasks || []).forEach((t) => {
        if (t.status === "completed" || t.status === "failed") {
          updatePending(t.request.task_id, { result: { task_id: t.request.task_id, success: t.status === "completed" } });
        }
      });
    } catch (e) {}
  }

  // D6：取消任务
  window.__cancelTask = async function (taskId) {
    if (!confirm("确认取消任务 " + taskId + "？")) return;
    const agentID = state.selected || "";
    try {
      await apiFetch("/api/tasks/cancel", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ task_id: taskId, agent_id: agentID, reason: "用户手动取消" }),
      });
    } catch (e) {
      alert("取消失败: " + e.message);
    }
  };

  // D6：metrics 面板轮询
  async function refreshMetrics() {
    try {
      const r = await apiFetch("/api/metrics");
      const m = await r.json();
      const el = document.getElementById("metrics-panel");
      if (el) {
        el.innerHTML = `
          <div class="metric"><span class="metric-label">在线 Agent</span><span class="metric-value">${m.agents_online}</span></div>
          <div class="metric"><span class="metric-label">提交</span><span class="metric-value">${m.tasks_submitted}</span></div>
          <div class="metric"><span class="metric-label">投递</span><span class="metric-value">${m.tasks_dispatched}</span></div>
          <div class="metric"><span class="metric-label">完成</span><span class="metric-value ok">${m.tasks_completed}</span></div>
          <div class="metric"><span class="metric-label">失败</span><span class="metric-value fail">${m.tasks_failed}</span></div>
          <div class="metric"><span class="metric-label">拦截</span><span class="metric-value fail">${m.tasks_blocked}</span></div>
          <div class="metric"><span class="metric-label">uptime</span><span class="metric-value">${m.uptime_seconds}s</span></div>
          <div class="metric"><span class="metric-label">goroutines</span><span class="metric-value">${m.goroutines}</span></div>
        `;
      }
    } catch (e) {}
  }

  // --- 结果流 ---
  function appendResult(item) {
    const stream = document.getElementById("result-stream");
    // 清空"暂无结果"占位
    if (stream.querySelector(".empty")) stream.innerHTML = "";
    const div = document.createElement("div");
    div.className = "result-item";
    div.id = "result-" + item.task_id;
    div.innerHTML = renderResultHTML(item);
    stream.prepend(div);
  }

  function updatePending(taskId, patch) {
    const div = document.getElementById("result-" + taskId);
    if (!div) {
      // 找不到现有 pending 项，可能是页面刷新后到达，直接追加
      appendResult(Object.assign({ task_id: taskId }, patch.result || {}));
      return;
    }
    // 更新内容
    const existing = div.__data || {};
    Object.assign(existing, patch);
    div.__data = existing;
    div.innerHTML = renderResultHTML(existing);
  }

  function renderResultHTML(item) {
    const ts = new Date(item.ts || Date.now()).toLocaleTimeString();
    if (item.result) {
      const r = item.result;
      const statusCls = r.success ? "ok" : "fail";
      let body = "";
      if (r.stdout) body += `<div class="result-stdout">${esc(r.stdout)}</div>`;
      if (r.stderr) body += `<div class="result-stdout" style="border-color:#f85149">${esc(r.stderr)}</div>`;
      if (r.error) body += `<div class="result-stdout" style="border-color:#f85149">错误: ${esc(r.error)}</div>`;
      // 截图/数据
      if (r.data) {
        const dataObj = typeof r.data === "string" ? safeParse(r.data) : r.data;
        if (dataObj && dataObj.image) {
          body += `<img class="result-img" src="${dataObj.image}">`;
        }
        if (dataObj && dataObj.screenshot) {
          body += `<img class="result-img" src="${dataObj.screenshot}">`;
        }
        if (dataObj && dataObj.content_b64) {
          const decoded = atob(dataObj.content_b64);
          body += `<div class="result-stdout">文件内容 (${dataObj.bytes || decoded.length} bytes):\n${esc(decoded)}</div>`;
        }
        if (dataObj && dataObj.steps) {
          body += `<div class="result-stdout">交互步骤: ${dataObj.steps.length}\n${esc(JSON.stringify(dataObj.steps, null, 2))}</div>`;
        }
      }
      return `<span class="ts">[${ts}]</span> <strong>${esc(r.task_id)}</strong>
        <span class="${statusCls}">${r.success ? "✓ 成功" : "✗ 失败"}</span>
        ${r.duration_ms ? `<span class="ts">${r.duration_ms}ms</span>` : ""}${body}`;
    }
    if (item.pending) {
      const taskID = esc(item.task_id);
      return `<span class="ts">[${ts}]</span> <strong>${taskID}</strong>
        <span style="color:#d29922">⏳ 执行中 (${esc(item.type || "")})...</span>
        <button class="cancel-btn" onclick="window.__cancelTask('${taskID}')">取消</button>`;
    }
    if (item.progress) {
      const taskID = esc(item.progress.task_id);
      return `<span class="ts">[${ts}]</span> <strong>${taskID}</strong>
        <span style="color:#58a6ff">${esc(item.progress.step)}: ${esc(item.progress.message)} (${item.progress.percent}%)</span>
        <button class="cancel-btn" onclick="window.__cancelTask('${taskID}')">取消</button>`;
    }
    return `<span class="ts">[${ts}]</span> ${esc(JSON.stringify(item))}`;
  }

  // --- 辅助 ---
  function esc(s) {
    if (s == null) return "";
    return String(s).replace(/[&<>"']/g, (c) => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
    })[c]);
  }
  function safeParse(s) {
    try { return JSON.parse(s); } catch { return null; }
  }

  // --- 启动 ---
  refreshAgents();
  subscribeSSE();
  refreshMetrics();
  setInterval(refreshAgents, 10000); // 兜底轮询
  setInterval(refreshMetrics, 5000); // metrics 轮询

  // H5：交互式终端（零依赖原生实现，不用 xterm.js 保持单文件部署）
  state.shellSession = null;
  state.shellOutput = "";

  window.__openShell = async function () {
    if (!state.selected) { alert("请先选择一个 Agent"); return; }
    const panel = document.getElementById("shell-panel");
    const term = document.getElementById("terminal");
    panel.style.display = "block";
    term.innerHTML = '<span style="color:#8b949e">正在开启终端...</span>';

    try {
      const r = await apiFetch("/api/shell/open", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ agent_id: state.selected, cols: 80, rows: 24 }),
      });
      const data = await r.json();
      if (!r.ok) { term.innerHTML = '<span style="color:#f85149">开启失败: ' + esc(data.error || "") + "</span>"; return; }
      state.shellSession = data.session_id;
      state.shellOutput = "";
      // I1：内联终端 UI（输出区 + 输入框 + 关闭按钮）
      term.innerHTML =
        '<div style="margin-bottom:8px"><span style="color:#3fb950">● 终端已连接</span> ' +
        '<span style="color:#8b949e;font-size:12px">session=' + esc(data.session_id) + '</span>' +
        ' <button class="cancel-btn" onclick="window.__closeShell()">关闭</button></div>' +
        '<pre id="shell-content" style="white-space:pre-wrap;word-break:break-all;max-height:400px;overflow-y:auto;margin:0"></pre>' +
        '<div style="margin-top:8px;display:flex;gap:8px">' +
        '<input id="shell-input" placeholder="输入命令，Enter 发送..." ' +
        'style="flex:1;background:#0d1117;border:1px solid #30363d;color:#c9d1d9;padding:6px;border-radius:4px;font-family:monospace" ' +
        'onkeydown="if(event.key===\'Enter\'){window.__sendInput(this.value);this.value=\'\';}">' +
        '<button onclick="window.__sendInput(document.getElementById(\'shell-input\').value+\'\\n\');document.getElementById(\'shell-input\').value=\'\'">发送</button>' +
        '</div>';
      // 自动聚焦输入框
      setTimeout(function() { var i = document.getElementById("shell-input"); if (i) i.focus(); }, 100);
    } catch (e) {
      term.innerHTML = '<span style="color:#f85149">错误: ' + esc(e.message) + "</span>";
    }
  };

  window.__shellSend = async function (text) {
    if (!state.shellSession) return;
    await window.__sendInput(text);
  };

  window.__sendInput = async function (text) {
    if (!state.shellSession) return;
    try {
      await apiFetch("/api/shell/input", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          session_id: state.shellSession,
          agent_id: state.selected,
          data_b64: btoa(unescape(encodeURIComponent(text))),
        }),
      });
    } catch (e) {}
  };

  window.__closeShell = async function () {
    if (!state.shellSession) return;
    var sid = state.shellSession;
    state.shellSession = null;
    try {
      await apiFetch("/api/shell/close", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ session_id: sid, agent_id: state.selected }),
      });
    } catch (e) {}
    var content = document.getElementById("shell-content");
    if (content) { content.innerHTML += '\n<span style="color:#8b949e">[会话已关闭]</span>'; }
  };

  // SSE 已订阅 shell_output 事件（在 subscribeSSE 里加监听）

  // J3：大文件传输进度（SSE）
  if (state.evtSource) {
    state.evtSource.addEventListener("file_xfer_progress", (e) => {
      const data = JSON.parse(e.data).data;
      const el = document.getElementById("xfer-progress");
      if (el) {
        const mb = (data.bytes_done / 1048576).toFixed(1);
        const totalMB = data.total > 0 ? (data.total / 1048576).toFixed(1) : "?";
        const speedMB = data.speed_bps > 0 ? (data.speed_bps / 1048576).toFixed(1) : "0";
        el.innerHTML = '<div style="background:#21262d;border-radius:4px;padding:8px">' +
          '<div style="display:flex;justify-content:between">' +
          '<span>' + data.percent + '% (' + mb + '/' + totalMB + ' MB)</span>' +
          '<span style="color:#8b949e">' + speedMB + ' MB/s</span></div>' +
          '<div style="background:#0d1117;border-radius:2px;height:20px;margin-top:4px">' +
          '<div style="background:#238636;height:100%;width:' + data.percent + '%;border-radius:2px"></div>' +
          '</div></div>';
      }
    });
    state.evtSource.addEventListener("file_xfer_complete", (e) => {
      const data = JSON.parse(e.data).data;
      const el = document.getElementById("xfer-progress");
      if (el && data.success) {
        el.innerHTML += '<div style="color:#3fb950;margin-top:4px">✓ 传输完成 sha256=' + (data.sha256_actual || "").substring(0, 16) + '...</div>';
      } else if (el) {
        el.innerHTML += '<div style="color:#f85149;margin-top:4px">✗ 传输失败: ' + esc(data.error || "") + '</div>';
      }
    });
  }

  // J3：上传文件（B2：用 XMLHttpRequest 显示上传进度）
  window.__uploadFile = function () {
    if (!state.selected) { alert("请先选择 Agent"); return; }
    var path = document.getElementById("xfer-upload-path").value;
    var fileInput = document.getElementById("xfer-file-input");
    if (!path || !fileInput.files[0]) { alert("请填写路径并选择文件"); return; }
    var file = fileInput.files[0];
    var transferID = "xfer-" + Date.now();
    var el = document.getElementById("xfer-progress");
    el.innerHTML = '<span style="color:#d29922">⏳ 上传中... 0%</span>';

    var xhr = new XMLHttpRequest();
    xhr.open("POST", api("/api/transfer/upload?agent_id=" + encodeURIComponent(state.selected) +
      "&path=" + encodeURIComponent(path) + "&transfer_id=" + transferID));
    if (getToken()) xhr.setRequestHeader("Authorization", "Bearer " + getToken());
    xhr.upload.onprogress = function (ev) {
      if (ev.lengthComputable) {
        var pct = Math.round(ev.loaded * 100 / ev.total);
        var mb = (ev.loaded / 1048576).toFixed(1);
        var totalMB = (ev.total / 1048576).toFixed(1);
        el.innerHTML = '<div style="background:#21262d;border-radius:4px;padding:8px">' +
          '<div>上传: ' + pct + '% (' + mb + '/' + totalMB + ' MB)</div>' +
          '<div style="background:#0d1117;border-radius:2px;height:16px;margin-top:4px">' +
          '<div style="background:#58a6ff;height:100%;width:' + pct + '%;border-radius:2px"></div></div></div>';
      }
    };
    xhr.onload = function () {
      if (xhr.status === 200) {
        var result = JSON.parse(xhr.responseText);
        el.innerHTML = '<div style="color:#3fb950">✓ 上传成功 ' + result.bytes + ' bytes</div>' +
          '<div style="color:#8b949e;font-size:12px">SHA256: ' + (result.sha256 || "").substring(0, 32) + '...</div>';
      } else {
        try { var e = JSON.parse(xhr.responseText); el.innerHTML = '<span style="color:#f85149">上传失败: ' + esc(e.error || "") + '</span>'; }
        catch (_) { el.innerHTML = '<span style="color:#f85149">上传失败: HTTP ' + xhr.status + '</span>'; }
      }
    };
    xhr.onerror = function () { el.innerHTML = '<span style="color:#f85149">网络错误</span>'; };
    xhr.send(file);
  };

  // J3/B1：下载文件（用浏览器原生流式下载，不用 blob() 防 OOM）
  window.__downloadFile = function () {
    if (!state.selected) { alert("请先选择 Agent"); return; }
    var path = document.getElementById("xfer-download-path").value;
    if (!path) { alert("请填写路径"); return; }
    var transferID = "xfer-" + Date.now();
    document.getElementById("xfer-progress").innerHTML = '<span style="color:#d29922">⏳ 下载中（浏览器原生流式下载）...</span>';
    // B1 修复：用隐藏 <a> 让浏览器原生流式落盘，不用 blob() 全载内存
    var url = api("/api/transfer/download?agent_id=" + encodeURIComponent(state.selected) +
      "&path=" + encodeURIComponent(path) + "&transfer_id=" + transferID);
    // 凭证由同源 cookie(reins_token)自动携带——<a> 下载与 SIEM 导出同此
    var a = document.createElement("a");
    a.href = url;
    a.download = path.split("\\").pop().split("/").pop();
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    setTimeout(function () {
      document.getElementById("xfer-progress").innerHTML = '<span style="color:#3fb950">✓ 下载已启动（浏览器原生下载管理器）</span>';
    }, 2000);
  };

  // shell_output 事件处理
  if (state.evtSource) {
    state.evtSource.addEventListener("shell_output", (e) => {
      const data = JSON.parse(e.data).data;
      if (data.exit) {
        const content = document.getElementById("shell-content");
        if (content) { content.innerHTML += '<span style="color:#8b949e">\n[进程退出 code=' + (data.exit_code !== undefined ? data.exit_code : "?") + "]</span>\n"; }
        state.shellSession = null;
        return;
      }
      if (data.data_b64) {
        const decoded = decodeURIComponent(escape(atob(data.data_b64)));
        state.shellOutput += decoded;
        const content = document.getElementById("shell-content");
        if (content) {
          content.textContent += decoded;
          // 自动滚动到底
          const term = document.getElementById("terminal");
          if (term) { term.scrollTop = term.scrollHeight; }
        }
      }
    });
    state.evtSource.addEventListener("shell_close", (e) => {
      const content = document.getElementById("shell-content");
      if (content) { content.innerHTML += '<span style="color:#8b949e">\n[会话已关闭]</span>\n'; }
      state.shellSession = null;
    });
  }

  // === P2-8 批量分发(POST /api/batch 异步批次 + 轮询进度可视化) ===
  let batchTimer = null;

  window.__submitBatch = async function () {
    const tag = (document.getElementById("batch-tag") || {}).value || "";
    const cmd = (document.getElementById("batch-cmd") || {}).value || "";
    const timeout = parseInt((document.getElementById("batch-timeout") || {}).value, 10) || 60;
    const retries = parseInt((document.getElementById("batch-retries") || {}).value, 10) || 0;
    if (!tag || !cmd) { alert("目标标签与命令必填"); return; }
    try {
      const r = await apiFetch("/api/batch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ tag, type: "powershell", payload: { command: cmd }, timeout, max_retries: retries }),
      });
      const data = await r.json();
      if (!r.ok) { alert("批量下发失败: " + (data.error || r.status)); return; }
      document.getElementById("batch-progress").style.display = "block";
      document.getElementById("batch-summary").textContent = "批次 " + data.batch_id.slice(0, 16) + "… 已提交,目标 " + data.total + " 个";
      document.getElementById("batch-bar").style.width = "0%";
      document.getElementById("batch-targets").innerHTML = "";
      if (batchTimer) clearInterval(batchTimer);
      const poll = async function () {
        try {
          const pr = await apiFetch("/api/batch/" + encodeURIComponent(data.batch_id));
          if (!pr.ok) { clearInterval(batchTimer); return; }
          const snap = await pr.json();
          renderBatchProgress(snap);
          if (snap.status === "finished") clearInterval(batchTimer);
        } catch (e) { /* 网络抖动,下轮重试 */ }
      };
      poll();
      batchTimer = setInterval(poll, 1000);
    } catch (e) { alert("批量下发异常: " + e.message); }
  };

  function renderBatchProgress(snap) {
    const summary = document.getElementById("batch-summary");
    const bar = document.getElementById("batch-bar");
    const eta = document.getElementById("batch-eta");
    if (!summary || !bar) return;
    const done = (snap.done || 0), total = (snap.total || 0);
    summary.textContent = "✓ " + snap.succeeded + " 成功 · ✗ " + snap.failed + " 失败 · ⋯ " + snap.pending + " 待投 · " + done + "/" + total;
    bar.style.width = (snap.percent || 0).toFixed(1) + "%";
    bar.style.background = snap.status === "finished"
      ? ((snap.failed || 0) > 0 ? "#d29922" : "#3fb950")
      : "#2f81f7";
    eta.textContent = snap.status === "finished"
      ? ("完成 · 成功率 " + Math.round((snap.success_rate || 0) * 100) + "% · 耗时 " + ((snap.elapsed_ms || 0) / 1000).toFixed(1) + "s")
      : (snap.eta_ms ? "ETA " + (snap.eta_ms / 1000).toFixed(0) + "s" : "");
    const box = document.getElementById("batch-targets");
    if (box && Array.isArray(snap.states)) {
      const color = { succeeded: "#3fb950", failed: "#f85149", running: "#2f81f7", pending: "#8b949e" };
      box.innerHTML = snap.states.map(function (st) {
        return '<div style="border:1px solid #30363d;border-radius:6px;padding:6px 8px;overflow:hidden">' +
          '<span style="color:' + (color[st.Status] || "#8b949e") + '">●</span> <b>' + st.AgentID.slice(0, 28) + "</b>" +
          '<span style="float:right;opacity:.75">' + st.Status + (st.Attempt > 1 ? " (第" + st.Attempt + "次)" : "") + "</span>" +
          (st.Error ? '<div style="opacity:.7;white-space:nowrap;text-overflow:ellipsis;overflow:hidden" title="' + st.Error.replace(/"/g, "&quot;") + '">' + st.Error + "</div>" : "") +
          "</div>";
      }).join("");
    }
  }

  // ===================================================================
  // Tab 导航 + 任务历史 + 审计日志(UI 打磨:任务/审计可视化)
  // ===================================================================

  const PAGE_SIZE = 20;

  // --- Tab 切换 ---
  window.__switchTab = function (pageId, btn) {
    document.querySelectorAll(".tab-page").forEach(function (p) { p.classList.remove("active"); });
    document.querySelectorAll(".tab").forEach(function (t) { t.classList.remove("active"); });
    var page = document.getElementById(pageId);
    if (page) page.classList.add("active");
    if (btn) btn.classList.add("active");
    // 惰性加载:首次切到历史/审计页时拉数据
    if (pageId === "page-history" && !state.histLoaded) window.__loadTasks(0);
    if (pageId === "page-audit" && !state.auditLoaded) window.__loadAudit(0);
  };

  // SSE task_result 到达时,若对应页可见则刷新(30s 节流防批量任务刷屏)
  var lastTableRefresh = 0;
  function maybeRefreshTables() {
    var now = Date.now();
    if (now - lastTableRefresh < 30000) return;
    var histActive = document.getElementById("page-history") && document.getElementById("page-history").classList.contains("active");
    var auditActive = document.getElementById("page-audit") && document.getElementById("page-audit").classList.contains("active");
    if (histActive) { lastTableRefresh = now; window.__loadTasks(state.histOffset || 0, true); }
    if (auditActive) { lastTableRefresh = now; window.__loadAudit(state.auditOffset || 0, true); }
  }

  // --- 状态徽章 ---
  function statusBadge(status) {
    var cls = { completed: "completed", failed: "failed", cancelled: "cancelled" }[status] || "running";
    var label = { pending: "待投递", dispatched: "已投递", acknowledged: "执行中", completed: "已完成", failed: "失败", cancelled: "已取消" }[status] || status;
    return '<span class="badge ' + cls + '">' + label + "</span>";
  }

  function fmtTime(ms) {
    if (!ms) return "—";
    return new Date(ms).toLocaleString();
  }

  // --- 任务历史 ---
  window.__loadTasks = async function (offset, silent) {
    try {
      var status = (document.getElementById("hist-status") || {}).value || "";
      var type = (document.getElementById("hist-type") || {}).value || "";
      var agent = (document.getElementById("hist-agent") || {}).value || "";
      var q = "/api/tasks?limit=" + PAGE_SIZE + "&offset=" + (offset || 0);
      if (status) q += "&status=" + encodeURIComponent(status);
      if (type) q += "&type=" + encodeURIComponent(type);
      if (agent) q += "&agent_id=" + encodeURIComponent(agent);
      var r = await apiFetch(q);
      if (!r.ok) {
        document.getElementById("hist-tbody").innerHTML =
          '<tr><td colspan="6" class="empty small">' + (r.status === 503 ? "持久化未启用" : "加载失败 HTTP " + r.status) + "</td></tr>";
        return;
      }
      var data = await r.json();
      state.histLoaded = true;
      state.histOffset = offset || 0;
      state.histTotal = data.total || 0;
      var seen = state.seenAgentIds;
      (data.tasks || []).forEach(function (t) {
        var id = t.request && t.request.agent_id;
        if (id && seen.indexOf(id) < 0) { seen.push(id); if (seen.length > 200) seen.shift(); }
      });
      renderTaskRows(data.tasks || []);
      renderPager("hist", state.histOffset, data.count || 0, data.total || 0);
    } catch (e) {
      if (!silent) alert("任务历史加载失败: " + e.message);
    }
  };

  function renderTaskRows(tasks) {
    var tbody = document.getElementById("hist-tbody");
    if (!tasks.length) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty small">当前筛选条件下没有任务记录</td></tr>';
      return;
    }
    tbody.innerHTML = "";
    tasks.forEach(function (t) {
      var tr = document.createElement("tr");
      tr.className = "rowclick";
      tr.onclick = function () { toggleTaskDetail(tr, t); };
      var payloadDesc = "";
      try {
        var p = typeof t.request.payload === "string" ? JSON.parse(t.request.payload || "{}") : (t.request.payload || {});
        payloadDesc = p.command || p.path || p.url || p.intent || (p.content ? "脚本 " + p.content.length + " 字符" : "");
      } catch (e) { payloadDesc = ""; }
      tr.innerHTML =
        "<td>" + esc(fmtTime(t.created_at)) + "</td>" +
        '<td class="cell-id">' + esc(t.request.task_id) + "</td>" +
        "<td>" + esc(t.request.type) + "</td>" +
        '<td class="cell-id">' + esc(t.request.agent_id || "") + "</td>" +
        "<td>" + statusBadge(t.status) + "</td>" +
        '<td class="cell-detail" title="' + esc(payloadDesc).replace(/"/g, "&quot;") + '">' + esc(payloadDesc) + "</td>";
      tbody.appendChild(tr);
    });
  }

  // 行详情:拉 /api/tasks/{id} 取结果,按类型渲染
  var openDetailTaskId = null;
  function toggleTaskDetail(tr, t) {
    var taskId = t.request.task_id;
    var existing = document.getElementById("hist-detail-row");
    if (existing) existing.remove();
    if (openDetailTaskId === taskId) { openDetailTaskId = null; return; }
    openDetailTaskId = taskId;
    var row = document.createElement("tr");
    row.className = "detail-row";
    row.id = "hist-detail-row";
    var cell = document.createElement("td");
    cell.colSpan = 6;
    cell.innerHTML = '<div class="kv">加载结果中…</div>';
    row.appendChild(cell);
    tr.after(row);
    apiFetch("/api/tasks/" + encodeURIComponent(taskId))
      .then(function (r) { return r.ok ? r.json() : { task: t, result: null, error: "HTTP " + r.status }; })
      .then(function (d) {
        if (openDetailTaskId !== taskId) return; // 已切到别的行
        cell.innerHTML = renderTaskDetail(t, d.result);
      })
      .catch(function (e) {
        cell.innerHTML = '<div class="kv">结果加载失败: ' + esc(e.message) + "</div>";
      });
  }

  // 按任务类型渲染结果详情(截图/文件内容/命令输出)
  function renderTaskDetail(t, result) {
    var head = '<div class="kv">任务 <b>' + esc(t.request.task_id) + "</b> · 类型 <b>" + esc(t.request.type) +
      "</b> · Agent <b>" + esc(t.request.agent_id || "") + "</b> · 状态 <b>" + esc(t.status) + "</b> · 创建 " + esc(fmtTime(t.created_at)) + "</div>";
    // 请求 payload 摘要
    var payloadPre = "";
    try {
      var p = typeof t.request.payload === "string" ? JSON.parse(t.request.payload || "{}") : (t.request.payload || {});
      var shown = Object.assign({}, p);
      // 长内容截断展示(完整内容在审计/结果里)
      ["command", "content", "content_b64", "intent"].forEach(function (k) {
        if (shown[k] && String(shown[k]).length > 400) shown[k] = String(shown[k]).slice(0, 400) + "…(" + String(shown[k]).length + " 字符)";
      });
      payloadPre = '<div class="kv">请求参数</div><div class="detail-pre">' + esc(JSON.stringify(shown, null, 2)) + "</div>";
    } catch (e) { /* payload 展示失败不致命 */ }
    if (!result) return head + payloadPre + '<div class="kv">暂无结果(任务未到终态或结果已被清理)</div>';
    var body = "";
    if (result.error) body += '<div class="kv" style="color:#f85149">错误: ' + esc(result.error) + "</div>";
    if (result.stdout) body += '<div class="kv">stdout</div><div class="detail-pre">' + esc(result.stdout) + "</div>";
    if (result.stderr) body += '<div class="kv" style="color:#f85149">stderr</div><div class="detail-pre">' + esc(result.stderr) + "</div>";
    if (result.data) {
      var dataObj = typeof result.data === "string" ? safeParse(result.data) : result.data;
      if (dataObj) {
        if (dataObj.image || dataObj.screenshot) {
          body += '<div class="kv">截图</div><img class="detail-img" src="' + esc(dataObj.image || dataObj.screenshot) + '">';
        }
        if (dataObj.content_b64) {
          var decoded;
          try { decoded = atob(dataObj.content_b64); } catch (e) { decoded = "(base64 解码失败)"; }
          body += '<div class="kv">文件内容 (' + (dataObj.bytes || decoded.length) + " bytes)</div><div class=\"detail-pre\">" + esc(decoded) + "</div>";
        }
        if (dataObj.steps) {
          body += '<div class="kv">交互步骤 (' + dataObj.steps.length + ")</div><div class=\"detail-pre\">" + esc(JSON.stringify(dataObj.steps, null, 2)) + "</div>";
        }
        // 其余结构化数据兜底展示(坐标/URL/版本号等)
        var rest = Object.assign({}, dataObj);
        delete rest.image; delete rest.screenshot; delete rest.content_b64; delete rest.steps;
        if (Object.keys(rest).length) {
          body += '<div class="kv">结构化数据</div><div class="detail-pre">' + esc(JSON.stringify(rest, null, 2)) + "</div>";
        }
      }
    }
    var dur = result.duration_ms ? " · 耗时 " + result.duration_ms + "ms" : "";
    var outcome = result.success
      ? '<span class="badge completed">✓ 成功' + dur + "</span>"
      : '<span class="badge failed">✗ 失败' + dur + "</span>";
    return head + '<div class="kv">结果 ' + outcome + " · 退出码 " + (result.exit_code !== undefined ? result.exit_code : "?") + "</div>" + body + payloadPre;
  }

  window.__histPrev = function () { if ((state.histOffset || 0) > 0) window.__loadTasks(Math.max(0, (state.histOffset || 0) - PAGE_SIZE)); };
  window.__histNext = function () {
    var off = (state.histOffset || 0) + PAGE_SIZE;
    if (off < (state.histTotal || 0)) window.__loadTasks(off);
  };

  // --- 审计日志 ---
  var AUDIT_EVENT_CLASS = {
    task_blocked: "crit", agent_revoked: "crit", task_max_attempts: "crit",
    task_failed: "warn", power_executed: "warn", shell_blocked: "warn", cron_blocked: "warn", dag_rejected: "warn",
    agent_online: "info",
  };

  window.__loadAudit = async function (offset, silent) {
    try {
      var ev = (document.getElementById("audit-event") || {}).value || "";
      var agent = (document.getElementById("audit-agent") || {}).value || "";
      var since = (document.getElementById("audit-since") || {}).value || "24h";
      var q = "/api/audit/query?limit=" + PAGE_SIZE + "&offset=" + (offset || 0) + "&since=" + encodeURIComponent(since);
      if (ev) q += "&event=" + encodeURIComponent(ev);
      if (agent) q += "&agent_id=" + encodeURIComponent(agent);
      var r = await apiFetch(q);
      if (!r.ok) {
        document.getElementById("audit-tbody").innerHTML =
          '<tr><td colspan="6" class="empty small">' + (r.status === 503 ? "审计未启用(Relay 需开启审计)" : "加载失败 HTTP " + r.status) + "</td></tr>";
        state.auditLoaded = true;
        renderPager("audit", offset || 0, 0, 0);
        return;
      }
      var data = await r.json();
      state.auditLoaded = true;
      state.auditOffset = offset || 0;
      state.auditTotal = data.total || 0;
      renderAuditRows(data.entries || []);
      renderPager("audit", state.auditOffset, data.count || 0, data.total || 0);
    } catch (e) {
      if (!silent) alert("审计加载失败: " + e.message);
    }
  };

  function renderAuditRows(entries) {
    var tbody = document.getElementById("audit-tbody");
    if (!entries.length) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty small">当前筛选条件下没有审计记录</td></tr>';
      return;
    }
    tbody.innerHTML = "";
    entries.forEach(function (e) {
      var tr = document.createElement("tr");
      tr.className = "rowclick";
      tr.onclick = function () { toggleAuditDetail(tr, e); };
      var cls = AUDIT_EVENT_CLASS[e.event] || "running";
      tr.innerHTML =
        "<td>" + esc(fmtTime(e.ts)) + "</td>" +
        '<td><span class="badge ' + cls + '">' + esc(e.event) + "</span></td>" +
        '<td class="cell-id">' + esc(e.agent_id || "—") + "</td>" +
        '<td class="cell-id">' + esc(e.task_id || "—") + "</td>" +
        "<td>" + esc(e.operator || "—") + "</td>" +
        '<td class="cell-detail" title="' + esc(e.detail || "").replace(/"/g, "&quot;") + '">' + esc(e.detail || "") + "</td>";
      tbody.appendChild(tr);
    });
  }

  var openAuditDetailTS = null;
  function toggleAuditDetail(tr, e) {
    var existing = document.getElementById("audit-detail-row");
    if (existing) existing.remove();
    if (openAuditDetailTS === e.ts + e.event) { openAuditDetailTS = null; return; }
    openAuditDetailTS = e.ts + e.event;
    var row = document.createElement("tr");
    row.className = "detail-row";
    row.id = "audit-detail-row";
    var cell = document.createElement("td");
    cell.colSpan = 6;
    cell.innerHTML =
      '<div class="kv">完整记录(hash chain 防篡改——prev_hash 应与上一条 this_hash 衔接)</div>' +
      '<div class="detail-pre">' + esc(JSON.stringify(e, null, 2)) + "</div>";
    row.appendChild(cell);
    tr.after(row);
  }

  window.__auditPrev = function () { if ((state.auditOffset || 0) > 0) window.__loadAudit(Math.max(0, (state.auditOffset || 0) - PAGE_SIZE)); };
  window.__auditNext = function () {
    var off = (state.auditOffset || 0) + PAGE_SIZE;
    if (off < (state.auditTotal || 0)) window.__loadAudit(off);
  };

  // SIEM 导出(新窗口下载 JSON Lines;format 可改 cef/syslog)
  window.__exportAudit = function () {
    var since = (document.getElementById("audit-since") || {}).value || "24h";
    var ev = (document.getElementById("audit-event") || {}).value || "";
    var agent = (document.getElementById("audit-agent") || {}).value || "";
    var url = api("/api/audit/export?format=json&since=" + encodeURIComponent(since));
    if (ev) url += "&event=" + encodeURIComponent(ev); // export 未实现 event 过滤时被忽略,行为无损
    if (agent) url += "&agent_id=" + encodeURIComponent(agent);
    // 凭证由同源 cookie 携带
    window.open(url, "_blank");
  };

  // --- 分页条渲染(hist/audit 共用) ---
  function renderPager(prefix, offset, count, total) {
    var info = document.getElementById(prefix + "-pageinfo");
    var prev = document.getElementById(prefix + "-prev");
    var next = document.getElementById(prefix + "-next");
    if (!info) return;
    if (total === 0) {
      info.textContent = "无记录";
    } else {
      var page = Math.floor(offset / PAGE_SIZE) + 1;
      var pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
      info.textContent = "第 " + page + "/" + pages + " 页 · 共 " + total + " 条";
    }
    if (prev) prev.disabled = offset <= 0;
    if (next) next.disabled = offset + count >= total;
  }
})();
