// go-rmm 控制台前端 —— 零依赖原生 JS
//
// 状态机：
//   agents: Map<agentID, Register>  当前已知 Agent
//   selected: string|null           当前选中 Agent
//   evtSource: EventSource          SSE 订阅
//   loginShowing: bool              登录遮罩显示中(暂停所有网络轮询)
//   mainUIStarted: bool             主 UI 已启动(幂等,登出后重置)
//
// 鉴权(2026-08-12):HttpOnly + Secure + SameSite 会话 Cookie。
//   - 所有 API 调用走 apiFetch()(自动 credentials:"include")
//   - EventSource 用 { withCredentials: true }(浏览器无法对 SSE 设 Authorization 头,只能靠 cookie)
//   - 401 → 弹登录遮罩;启动先 GET /api/console/session 判断登录态
(function () {
  "use strict";

  // 自动检测 base path（支持反向代理子路径如 /rmm/）
  // 页面 URL 为 https://IP/rmm/ → basePath = "/rmm"
  // 页面 URL 为 https://IP/ → basePath = ""
  var basePath = location.pathname.replace(/\/[^/]*$/, ""); // 剥掉文件名
  if (basePath === "/" || basePath === ".") basePath = "";
  function api(path) { return basePath + path; }

  const state = {
    agents: new Map(),
    selected: null,
    evtSource: null,
    loginShowing: false,
    mainUIStarted: false,
    agentsTimer: null,
    metricsTimer: null,
    bootstrapLoaded: false,
    // Agent 任务档案(2026-08-12)
    timelineAgent: null,   // 当前展示档案的 agent id
    timelineTimer: null,   // 15s 刷新句柄
    timelineTasks: [],     // 当前 agent 的任务缓存
    timelineCrons: [],     // cron 缓存
    taskAgentMap: {},      // task_id → agent_id(SSE 事件按 task_id 定位 agent)
    batchResults: {},      // 批量下发结果:task_id → {agentID, status, progress, result, ts}
    agentRun: { steps: [], summary: "", status: "", runId: "", model: "" },
  };

  // --- 控制台会话鉴权(2026-08-12)---
  //
  // apiFetch: 包装 fetch,自动 credentials:"include"(浏览器随请求发 HttpOnly cookie),
  // 并拦截 401 → 弹登录遮罩。cookie 是 HttpOnly,JS 读不到,完全由浏览器管理。
  async function apiFetch(url, opts) {
    opts = opts || {};
    opts.credentials = "include";
    const r = await fetch(url, opts);
    if (r.status === 401 && !state.loginShowing) {
      showLogin("会话已过期或未登录,请重新登录");
    }
    return r;
  }

  function showLogin(msg) {
    state.loginShowing = true;
    const ov = document.getElementById("login-overlay");
    if (ov) ov.style.display = "flex";
    const err = document.getElementById("login-error");
    if (err) err.textContent = msg || "";
    // 暂停 SSE/轮询,避免持续 401
    if (state.evtSource) { state.evtSource.close(); state.evtSource = null; }
    const pw = document.getElementById("login-password");
    if (pw) { pw.value = ""; setTimeout(function () { pw.focus(); }, 50); }
  }

  function hideLogin() {
    state.loginShowing = false;
    const ov = document.getElementById("login-overlay");
    if (ov) ov.style.display = "none";
  }

  function updateUserBadge(username) {
    const badge = document.getElementById("user-badge");
    const name = document.getElementById("user-name");
    if (badge && name) {
      name.textContent = username || "admin";
      badge.style.display = "inline";
    }
  }

  // 启动主 UI(登录成功或无需鉴权时调用)。幂等:__doLogout 后重置 mainUIStarted 允许重启。
  function startMainUI() {
    if (state.mainUIStarted) return;
    state.mainUIStarted = true;
    refreshAgents();
    subscribeSSE();
    refreshMetrics();
    renderBatchForm();
    renderAgentQuick();
    renderAgentHistory();
    state.agentsTimer = setInterval(refreshAgents, 10000); // 兜底轮询
    state.metricsTimer = setInterval(refreshMetrics, 5000); // metrics 轮询
    state.timelineTimer = setInterval(function () {
      if (state.timelineAgent && !state.loginShowing) loadAgentTimeline(state.timelineAgent);
    }, 15000); // 任务档案轮询
  }

  // 停主 UI(登出时调用):清 interval + 关 SSE。
  function stopMainUI() {
    state.mainUIStarted = false;
    if (state.agentsTimer) { clearInterval(state.agentsTimer); state.agentsTimer = null; }
    if (state.metricsTimer) { clearInterval(state.metricsTimer); state.metricsTimer = null; }
    if (state.timelineTimer) { clearInterval(state.timelineTimer); state.timelineTimer = null; }
    if (state.evtSource) { state.evtSource.close(); state.evtSource = null; }
    const badge = document.getElementById("user-badge");
    if (badge) badge.style.display = "none";
  }

  // 启动时先查会话状态,决定显示登录页还是直接进主 UI。
  async function checkSession() {
    try {
      const r = await apiFetch(api("/api/console/session"));
      const data = await r.json();
      if (data.logged_in) {
        updateUserBadge(data.username);
        hideLogin();
        startMainUI();
      } else if (data.console_auth) {
        // 控制台鉴权已启用但未登录
        showLogin();
      } else {
        // 控制台鉴权未启用:探测能否无鉴权进(dev 模式 Token 也空)
        hideLogin();
        try {
          const probe = await fetch(api("/api/agents"), { credentials: "include" });
          if (probe.ok) {
            startMainUI();
          } else {
            showLogin("控制台鉴权未启用(需配置 -console-password),无法登录");
          }
        } catch (e) {
          showLogin("无法连接 Relay: " + e.message);
        }
      }
    } catch (e) {
      showLogin("无法连接 Relay,请检查网络后重试");
    }
  }

  // 登录:裸 fetch(不走 apiFetch,避免 401 拦截与登录失败混淆)。credentials 带回新 cookie。
  window.__doLogin = async function () {
    const card = document.getElementById("login-card");
    const err = document.getElementById("login-error");
    const username = ((document.getElementById("login-username") || {}).value) || "";
    const password = ((document.getElementById("login-password") || {}).value) || "";
    if (!password) { if (err) err.textContent = "请输入密码"; return; }
    if (err) err.textContent = "";
    if (card) card.classList.add("loading");
    try {
      const loginURL = api("/api/console/login");
      const r = await fetch(loginURL, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({ username: username, password: password }),
      });
      const data = await r.json();
      if (card) card.classList.remove("loading");
      if (r.ok) {
        updateUserBadge(data.username);
        hideLogin();
        startMainUI();
      } else {
        if (err) err.textContent = data.error || ("登录失败 (" + r.status + ")");
      }
    } catch (e) {
      if (card) card.classList.remove("loading");
      if (err) err.textContent = "网络错误: " + e.message;
    }
  };

  window.__doLogout = async function () {
    try {
      const logoutURL = api("/api/console/logout");
      await fetch(logoutURL, { method: "POST", credentials: "include" });
    } catch (e) {}
    stopMainUI();
    showLogin("已登出");
  };

  // --- Agent 列表 ---
  async function refreshAgents() {
    if (state.loginShowing) return;
    try {
      const r = await apiFetch(api("/api/agents"));
      const data = await r.json();
      // 标记：列表里没有的视为离线
      const onlineIds = new Set();
      (data.agents || []).forEach((a) => {
        state.agents.set(a.agent_id, a);
        onlineIds.add(a.agent_id);
      });
      renderAgentList();
      renderBatchAgentList();
      // P2：0 Agent 时显示安装引导
      var overlay = document.getElementById("setup-overlay");
      if (state.agents.size === 0 && overlay && !overlay.dataset.dismissed) {
        overlay.style.display = "flex";
        // 自动检测 Relay IP（用当前页面的 hostname）
        var relayIP = location.hostname + (location.port ? ":" + location.port : "");
        // 填充所有 IP 占位符(install.ps1 命令中有 3 处)
        ["relay-ip", "relay-ip2", "relay-ip3"].forEach(function (id) {
          var el = document.getElementById(id);
          if (el) el.textContent = relayIP;
        });
        // 从 /api/bootstrap 拉取真实 Token(已登录,cookie 鉴权通过)
        if (!state.bootstrapLoaded) {
          state.bootstrapLoaded = true;
          apiFetch(api("/api/bootstrap?host=" + location.hostname + "&port=" + (location.port || "7780")))
            .then(function (r) { return r.ok ? r.json() : null; })
            .then(function (info) {
              if (info && info.token) {
                var tokenEl = document.getElementById("relay-token");
                if (tokenEl) tokenEl.textContent = info.token;
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
      div.innerHTML = `
        <div class="agent-id"><span class="status-dot"></span>${esc(a.agent_id)}</div>
        <div class="agent-meta">${esc(a.hostname || "")} · ${esc(a.os || "")} · v${esc(a.version || "?")}</div>
        <div class="caps">${caps}</div>
      `;
      list.appendChild(div);
    });
  }

  function selectAgent(id) {
    state.selected = id;
    renderAgentList();
    renderTaskForm();
    loadAgentTimeline(id);
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
      const r = await apiFetch(api("/api/tasks"), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      const data = await r.json();
      if (r.ok) {
        appendResult({ pending: true, task_id: taskId, type, ts: Date.now() });
        // 新任务归属当前选中 agent → 登记映射 + 刷新档案面板
        if (state.selected) {
          state.taskAgentMap[taskId] = state.selected;
          if (state.selected === state.timelineAgent) scheduleTimelineReload();
        }
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

  // --- SSE 订阅（D6 自动重连;2026-08-12 withCredentials 带 cookie）---
  // EventSource 浏览器侧无法设 Authorization 头,只能靠 cookie 鉴权 —— 这是会话 Cookie 方案的根因。
  // 所有事件监听(含 file_xfer / shell_output)在此注册,确保重连后新 EventSource 重新挂监听。
  function subscribeSSE() {
    if (state.loginShowing) return; // 登出/未登录时不连
    if (state.evtSource) state.evtSource.close();
    const es = new EventSource(api("/api/events"), { withCredentials: true });
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
      renderBatchAgentList();
    });
    es.addEventListener("agent_offline", (e) => {
      const data = JSON.parse(e.data).data;
      const id = data.agent_id;
      state.agents.delete(id);
      if (state.selected === id) state.selected = null;
      renderAgentList();
      renderBatchAgentList();
      renderTaskForm();
    });
    es.addEventListener("task_progress", (e) => {
      const data = JSON.parse(e.data).data;
      updatePending(data.task_id, { progress: data });
      if (state.taskAgentMap[data.task_id] === state.timelineAgent) scheduleTimelineReload();
      if (state.batchResults[data.task_id]) {
        state.batchResults[data.task_id].status = "dispatched";
        state.batchResults[data.task_id].progress = data;
        scheduleBatchReload();
      }
    });
    es.addEventListener("task_result", (e) => {
      const data = JSON.parse(e.data).data;
      updatePending(data.task_id, { result: data });
      if (state.taskAgentMap[data.task_id] === state.timelineAgent) scheduleTimelineReload();
      if (state.batchResults[data.task_id]) {
        var br = state.batchResults[data.task_id];
        br.status = data.success ? "completed" : "failed";
        br.duration_ms = data.duration_ms;
        if (data.error) br.error = data.error;
        br.ts = Date.now();
        scheduleBatchReload();
      }
    });
    // J3:大文件传输进度(SSE)
    es.addEventListener("file_xfer_progress", (e) => {
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
    es.addEventListener("file_xfer_complete", (e) => {
      const data = JSON.parse(e.data).data;
      const el = document.getElementById("xfer-progress");
      if (el && data.success) {
        el.innerHTML += '<div style="color:#3fb950;margin-top:4px">✓ 传输完成 sha256=' + (data.sha256_actual || "").substring(0, 16) + '...</div>';
      } else if (el) {
        el.innerHTML += '<div style="color:#f85149;margin-top:4px">✗ 传输失败: ' + esc(data.error || "") + '</div>';
      }
    });
    // H5:交互式终端输出(SSE)
    es.addEventListener("shell_output", (e) => {
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
    es.addEventListener("shell_close", (e) => {
      const content = document.getElementById("shell-content");
      if (content) { content.innerHTML += '<span style="color:#8b949e">\n[会话已关闭]</span>\n'; }
      state.shellSession = null;
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
    if (state.loginShowing) return;
    try {
      const r = await apiFetch(api("/api/tasks?limit=10"));
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
      await apiFetch(api("/api/tasks/cancel"), {
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
    if (state.loginShowing) return;
    try {
      const r = await apiFetch(api("/api/metrics"));
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

  // --- Agent 任务档案(2026-08-12:已做 / 在做 / 待做 + 定时计划)---
  let timelineReloadTimer = null;
  function scheduleTimelineReload() {
    if (timelineReloadTimer) clearTimeout(timelineReloadTimer);
    timelineReloadTimer = setTimeout(function () {
      timelineReloadTimer = null;
      if (state.timelineAgent && !state.loginShowing) loadAgentTimeline(state.timelineAgent);
    }, 500); // 合并短时多个 SSE 事件为一次重载
  }

  window.__refreshTimeline = function () {
    if (state.selected && !state.loginShowing) loadAgentTimeline(state.selected);
  };

  // loadAgentTimeline 拉取该 agent 的任务(分桶)+ cron,渲染到 #timeline-body。
  async function loadAgentTimeline(agentID) {
    if (state.loginShowing || !agentID) return;
    state.timelineAgent = agentID;
    var idEl = document.getElementById("timeline-agent-id");
    if (idEl) idEl.textContent = agentID;
    var body = document.getElementById("timeline-body");
    if (!body) return;
    body.innerHTML = '<div class="empty">加载中...</div>';
    try {
      var resp = await Promise.all([
        apiFetch(api("/api/tasks?agent_id=" + encodeURIComponent(agentID) + "&limit=100")),
        apiFetch(api("/api/crons?agent_id=" + encodeURIComponent(agentID))),
      ]);
      var tData = await resp[0].json();
      var cData = await resp[1].json();
      if (!resp[0].ok) {
        body.innerHTML = '<div class="empty">加载失败: ' + esc(tData.error || resp[0].status) + "</div>";
        return;
      }
      state.timelineTasks = tData.tasks || [];
      state.timelineCrons = cData.crons || [];
      // 维护 task_id → agent_id 映射(SSE 事件按 task_id 定位 agent)
      state.timelineTasks.forEach(function (t) {
        if (t.request) state.taskAgentMap[t.request.task_id] = agentID;
      });
      renderTimeline();
    } catch (e) {
      body.innerHTML = '<div class="empty">加载异常: ' + esc(e.message) + "</div>";
    }
  }

  function renderTimeline() {
    var body = document.getElementById("timeline-body");
    if (!body) return;
    if (!state.timelineAgent) {
      body.innerHTML = '<div class="empty">请先在左侧选择一个 Agent</div>';
      return;
    }
    var tasks = state.timelineTasks || [];
    var doing = [], pending = [], done = [];
    tasks.forEach(function (t) {
      var s = t.status;
      if (s === "dispatched" || s === "acknowledged") doing.push(t);
      else if (s === "pending") pending.push(t);
      else done.push(t); // completed/failed/cancelled
    });
    done.sort(function (a, b) { return (b.updated_at || 0) - (a.updated_at || 0); });
    var doneTop = done.slice(0, 20);
    var crons = state.timelineCrons || [];

    var html = '<div class="timeline-grid">' +
      timelineCol("🔄 在做 (" + doing.length + ")", doing.map(function (t) { return timelineItemHTML(t, "run"); }).join("")) +
      timelineCol("⏳ 待做 (" + pending.length + ")", pending.map(function (t) { return timelineItemHTML(t, "pend"); }).join("")) +
      timelineCol("✅ 已做 (最近 " + doneTop.length + ")", doneTop.map(function (t) {
        return timelineItemHTML(t, t.status === "completed" ? "ok" : "fail");
      }).join("")) +
      "</div>";
    if (crons.length) html += cronSectionHTML(crons);
    body.innerHTML = html;
    // 任务卡片点击展开详情
    body.querySelectorAll("[data-task-id]").forEach(function (el) {
      el.addEventListener("click", function () { toggleTaskDetail(el); });
    });
  }

  function timelineCol(title, itemsHTML) {
    return '<div class="timeline-col"><h4>' + title + "</h4>" +
      (itemsHTML || '<div class="empty" style="padding:12px">无</div>') + "</div>";
  }

  function timelineItemHTML(t, cls) {
    var id = t.request ? t.request.task_id : (t.task_id || "");
    var type = t.request ? t.request.type : "";
    var ts = fmtTime(t.updated_at || t.created_at);
    var label = t.status === "completed" ? "✓" : t.status === "failed" ? "✗"
      : t.status === "cancelled" ? "⊘" : t.status === "pending" ? "⏳" : "🔄";
    return '<div class="timeline-item" data-task-id="' + esc(id) + '">' +
      '<div><span class="' + cls + '">' + label + '</span> <span class="ti-id">' + esc(shortID(id)) +
      '</span> <span class="ti-meta">' + esc(type) + (ts ? " · " + ts : "") + "</span></div>" +
      '<div class="ti-meta">状态: ' + esc(t.status) + (t.attempt > 1 ? " · 尝试 " + t.attempt : "") + "</div>" +
      '<div class="timeline-detail" style="display:none"></div></div>';
  }

  function cronSectionHTML(crons) {
    var items = crons.map(function (c) {
      var next = c.next_run ? " · 下次: " + fmtTime(c.next_run) : "";
      var last = c.last_run ? "上次: " + fmtTime(c.last_run) : "上次: 无";
      var disabled = c.enabled ? "" : '<span class="cron-disabled">(已禁用)</span>';
      return '<div class="cron-item"><code>' + esc(c.cron_expr) + "</code> " +
        esc(c.task_type) + " · " + last + next + disabled + "</div>";
    }).join("");
    return '<div class="timeline-cron"><h4>⏰ 定时计划 (cron, ' + crons.length + ")</h4>" + items + "</div>";
  }

  function shortID(id) { return id ? id.slice(0, 16) : "?"; }
  function fmtTime(ms) {
    if (!ms) return "";
    var n = typeof ms === "number" ? ms : parseInt(ms, 10);
    if (!n) return "";
    var d = new Date(n);
    if (isNaN(d.getTime())) return "";
    return d.toLocaleString();
  }

  // toggleTaskDetail 点击任务卡片 → GET /api/tasks/{id} 取 result 展开详情。
  async function toggleTaskDetail(el) {
    var detail = el.querySelector(".timeline-detail");
    if (!detail) return;
    if (detail.style.display !== "none") { detail.style.display = "none"; return; }
    detail.style.display = "block";
    detail.textContent = "加载中...";
    var taskID = el.getAttribute("data-task-id");
    try {
      var r = await apiFetch(api("/api/tasks/" + encodeURIComponent(taskID)));
      if (!r.ok) { detail.textContent = "加载失败: " + r.status; return; }
      var data = await r.json();
      var result = data.result, task = data.task;
      var txt = "类型: " + (task && task.request ? task.request.type : "?") + "\n";
      txt += "状态: " + (task ? task.status : "?") + "\n";
      txt += "创建: " + fmtTime(task && task.created_at) + "\n";
      txt += "更新: " + fmtTime(task && task.updated_at) + "\n";
      if (result) {
        txt += "成功: " + result.success + (result.duration_ms ? " · " + result.duration_ms + "ms" : "") + "\n";
        if (result.stdout) txt += "\n--- stdout ---\n" + result.stdout;
        if (result.stderr) txt += "\n--- stderr ---\n" + result.stderr;
        if (result.error) txt += "\n--- error ---\n" + result.error;
      } else {
        txt += "\n(暂无结果,任务进行中或未投递)";
      }
      detail.textContent = txt;
    } catch (e) {
      detail.textContent = "异常: " + e.message;
    }
  }

  // --- 批量任务下发(2026-08-12:多选 agent + 一次性下发 + 执行结果表)---
  let batchReloadTimer = null;
  function scheduleBatchReload() {
    if (batchReloadTimer) clearTimeout(batchReloadTimer);
    batchReloadTimer = setTimeout(function () {
      batchReloadTimer = null;
      if (Object.keys(state.batchResults).length > 0) renderBatchResults();
    }, 300);
  }

  function renderBatchForm() {
    var sel = document.getElementById("b-task-type");
    if (!sel) return;
    var all = ["powershell", "script", "file", "screenshot", "browse", "guiclick"];
    sel.innerHTML = all.map(function (t) { return '<option value="' + t + '">' + t + "</option>"; }).join("");
    window.__renderBatchFields();
    renderBatchAgentList();
  }

  // renderBatchAgentList 渲染在线 agent checkbox,保留已勾选状态。
  function renderBatchAgentList() {
    var host = document.getElementById("batch-agent-list");
    updateAgentRunAgentSelect(); // 同步更新 AI Agent 面板的 agent 下拉
    if (!host) return;
    var checked = {};
    host.querySelectorAll('input[type="checkbox"]').forEach(function (cb) { checked[cb.value] = cb.checked; });
    var agents = Array.from(state.agents.values());
    if (agents.length === 0) {
      host.innerHTML = '<div class="empty" style="padding:8px">暂无在线 Agent</div>';
      return;
    }
    host.innerHTML = agents.map(function (a) {
      var c = checked[a.agent_id] ? " checked" : "";
      return '<div class="batch-agent"><label><input type="checkbox" value="' + esc(a.agent_id) + '"' + c + "> " +
        esc(a.agent_id) + ' <span style="color:#8b949e">' + esc(a.hostname || "") + "</span></label></div>";
    }).join("");
  }

  window.__renderBatchFields = function () {
    var type = document.getElementById("b-task-type").value;
    document.getElementById("b-task-fields").innerHTML = batchFieldsHTML(type);
  };

  // batchFieldsHTML 同 fieldsHTML 但用 b- 前缀 id,避免与单下发冲突。
  function batchFieldsHTML(type) {
    switch (type) {
      case "powershell":
        return '<label>PowerShell 命令(所有选中 Agent 执行相同命令)</label><textarea id="b-f-command" placeholder="Get-Process | Select-Object Name,Id -First 5">Get-Process | Select-Object Name,Id -First 5</textarea>';
      case "script":
        return '<div class="row"><div><label>语言</label><select id="b-f-lang"><option value="ps1">ps1</option><option value="bat">bat</option></select></div></div><label>脚本内容</label><textarea id="b-f-content">Write-Output "batch"</textarea>';
      case "file":
        return '<div class="row"><div><label>操作</label><select id="b-f-op"><option value="download">download</option><option value="upload">upload</option></select></div></div><label>目标路径(绝对路径)</label><input id="b-f-path" placeholder="C:\\temp\\x.txt"><label>内容 base64(仅 upload)</label><textarea id="b-f-contentb64" placeholder="upload 时填"></textarea>';
      case "screenshot":
        return '<label>目标(留空=全屏)</label><input id="b-f-target" placeholder="进程名/标题">';
      case "browse":
        return '<label>URL</label><input id="b-f-url" placeholder="https://example.com"><label>动作 JSON</label><textarea id="b-f-actions">[{"op":"screenshot"}]</textarea>';
      case "guiclick":
        return '<label>意图(自然语言)</label><textarea id="b-f-intent" placeholder="点击登录按钮"></textarea>';
      default:
        return "";
    }
  }

  window.__batchSelectAll = function () {
    document.querySelectorAll('#batch-agent-list input[type="checkbox"]').forEach(function (cb) { cb.checked = true; });
  };
  window.__batchClearAll = function () {
    document.querySelectorAll('#batch-agent-list input[type="checkbox"]').forEach(function (cb) { cb.checked = false; });
  };

  function batchBuildPayload(type) {
    var val = function (id) { var el = document.getElementById(id); return el ? el.value : ""; };
    switch (type) {
      case "powershell": return { command: val("b-f-command") };
      case "script": return { lang: val("b-f-lang"), content: val("b-f-content") };
      case "file": return { op: val("b-f-op"), path: val("b-f-path"), content_b64: val("b-f-contentb64") };
      case "screenshot": return { target: val("b-f-target"), full: false, activate: true };
      case "browse":
        try { return { url: val("b-f-url"), actions: JSON.parse(val("b-f-actions") || "[]") }; }
        catch (e) { alert("actions JSON 错: " + e.message); return null; }
      case "guiclick": return { intent: val("b-f-intent"), target: val("b-f-target"), max_steps: 5 };
      default: return {};
    }
  }

  window.__submitBatch = async function () {
    if (state.loginShowing) return;
    var type = document.getElementById("b-task-type").value;
    var timeout = parseInt(document.getElementById("b-task-timeout").value, 10) || 30;
    var payload = batchBuildPayload(type);
    if (payload === null) return;
    var agentIDs = [];
    document.querySelectorAll('#batch-agent-list input[type="checkbox"]:checked').forEach(function (cb) { agentIDs.push(cb.value); });
    if (agentIDs.length === 0) { alert("请至少勾选一个 Agent"); return; }
    if (type === "powershell" && !payload.command) { alert("请填 PowerShell 命令"); return; }

    var result = document.getElementById("batch-result");
    result.innerHTML = '<div class="empty" style="padding:12px">下发中...(' + agentIDs.length + " 个 Agent)</div>";
    try {
      var r = await apiFetch(api("/api/tasks/batch"), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ agent_ids: agentIDs, type: type, payload: payload, timeout: timeout }),
      });
      var data = await r.json();
      if (!r.ok) {
        result.innerHTML = '<div style="color:#f85149;padding:8px">下发失败: ' + esc(data.error || r.status) + "</div>";
        return;
      }
      // 初始化 batchResults
      state.batchResults = {};
      var dispatchedSet = {};
      (data.dispatched || []).forEach(function (aid) { dispatchedSet[aid] = true; });
      Object.keys(data.task_ids || {}).forEach(function (aid) {
        state.batchResults[data.task_ids[aid]] = { agentID: aid, status: "dispatched", ts: Date.now() };
      });
      // 勾选但未投递成功 → 标投递失败
      agentIDs.forEach(function (aid) {
        if (!dispatchedSet[aid]) {
          state.batchResults["fail-" + aid + "-" + Date.now()] = {
            agentID: aid, status: "failed", error: "投递失败(Agent 可能离线)", ts: Date.now(),
          };
        }
      });
      renderBatchResults();
    } catch (e) {
      result.innerHTML = '<div style="color:#f85149;padding:8px">异常: ' + esc(e.message) + "</div>";
    }
  };

  function renderBatchResults() {
    var result = document.getElementById("batch-result");
    if (!result) return;
    var rows = Object.values(state.batchResults);
    if (rows.length === 0) { result.innerHTML = ""; return; }
    var ok = 0, run = 0, fail = 0;
    rows.forEach(function (r) {
      if (r.status === "completed") ok++;
      else if (r.status === "failed") fail++;
      else run++;
    });
    rows.sort(function (a, b) { return (a.agentID || "").localeCompare(b.agentID || ""); });
    var html = '<div style="font-size:12px;color:#8b949e;margin-bottom:6px">执行结果(' + rows.length + ' 个 Agent,点击行展开详情):</div>';
    rows.forEach(function (r) {
      var icon = r.status === "completed" ? '<span class="ok">✓</span>'
        : r.status === "failed" ? '<span class="fail">✗</span>'
        : '<span class="run">🔄</span>';
      var statusText = r.status === "completed" ? "成功" + (r.duration_ms ? " " + r.duration_ms + "ms" : "")
        : r.status === "failed" ? "失败" + (r.error ? " · " + esc(r.error) : "")
        : (r.progress ? r.progress.percent + "% " + esc(r.progress.step || "") : "执行中...");
      html += '<div class="batch-row" data-batch-agent="' + esc(r.agentID) + '">' +
        '<div class="b-agent">' + icon + " " + esc(r.agentID) + '</div>' +
        '<div class="b-status ' + (r.status === "completed" ? "ok" : r.status === "failed" ? "fail" : "run") + '">' + statusText + "</div></div>";
    });
    html += '<div class="batch-summary">总体: <span class="ok">' + ok + "✓</span> · <span class=\"run\">" + run + '🔄</span> · <span class="fail">' + fail + "✗</span></div>";
    result.innerHTML = html;
    result.querySelectorAll("[data-batch-agent]").forEach(function (el) {
      el.addEventListener("click", function () { toggleBatchDetail(el); });
    });
  }

  // toggleBatchDetail 点执行表行 → GET /api/tasks/{id} 展开结果详情。
  async function toggleBatchDetail(el) {
    var agentID = el.getAttribute("data-batch-agent");
    var existing = el.querySelector(".batch-detail");
    if (existing) { existing.remove(); return; }
    var tid = null;
    Object.keys(state.batchResults).forEach(function (k) { if (state.batchResults[k].agentID === agentID) tid = k; });
    if (!tid || tid.indexOf("fail-") === 0) {
      var br = state.batchResults[tid];
      var d = document.createElement("div");
      d.className = "batch-detail";
      d.textContent = br ? br.error || "投递失败" : "无详情";
      el.appendChild(d);
      return;
    }
    var detail = document.createElement("div");
    detail.className = "batch-detail";
    detail.textContent = "加载中...";
    el.appendChild(detail);
    try {
      var r = await apiFetch(api("/api/tasks/" + encodeURIComponent(tid)));
      if (!r.ok) { detail.textContent = "加载失败: " + r.status; return; }
      var data = await r.json();
      var task = data.task, res = data.result;
      var txt = "task: " + tid + "\n状态: " + (task ? task.status : "?") + "\n";
      if (res) {
        txt += "成功: " + res.success + (res.duration_ms ? " · " + res.duration_ms + "ms" : "") + "\n";
        if (res.stdout) txt += "\n--- stdout ---\n" + res.stdout;
        if (res.stderr) txt += "\n--- stderr ---\n" + res.stderr;
        if (res.error) txt += "\n--- error ---\n" + res.error;
      } else { txt += "\n(暂无结果,执行中)"; }
      detail.textContent = txt;
    } catch (e) { detail.textContent = "异常: " + e.message; }
  }

  // --- 🤖 AI Agent 任务(2026-08-12:自然语言 → LLM 编排,fetch+ReadableStream 解析 SSE)---
  // 快捷指令模板:降低自然语言使用门槛(常见运维任务一键填入)。
  var agentQuickTemplates = [
    "列出所有在线 agent 及其能力",
    "查看所有 agent 的磁盘空间使用情况",
    "找出所有 agent 上 CPU 占用最高的 5 个进程",
    "查看系统的操作系统版本和内存信息",
    "检查是否有 Chrome 浏览器进程在运行",
    "读取目标 agent 的配置(allowed_roots/workspace)",
    "查看所有 agent 最近的任务执行历史",
  ];

  function renderAgentQuick() {
    var host = document.getElementById("agentrun-quick");
    if (!host) return;
    host.innerHTML = agentQuickTemplates.map(function (t) {
      return '<button class="ar-quick-btn" data-quick="' + esc(t) + '">' + esc(t) + "</button>";
    }).join("");
    host.querySelectorAll("[data-quick]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var ta = document.getElementById("agentrun-task");
        if (ta) { ta.value = btn.getAttribute("data-quick"); ta.focus(); }
      });
    });
  }

  // 会话历史(localStorage 持久化最近任务文本,点击重填)。
  function loadAgentHistory() {
    try { return JSON.parse(localStorage.getItem("gormm_agent_history") || "[]"); }
    catch (e) { return []; }
  }
  function saveAgentHistory(task) {
    var hist = loadAgentHistory().filter(function (t) { return t !== task; });
    hist.unshift(task);
    hist = hist.slice(0, 8);
    try { localStorage.setItem("gormm_agent_history", JSON.stringify(hist)); } catch (e) {}
  }
  function renderAgentHistory() {
    var host = document.getElementById("agentrun-history");
    if (!host) return;
    var hist = loadAgentHistory();
    if (hist.length === 0) { host.innerHTML = ""; return; }
    host.innerHTML = '<div class="ar-history"><div class="ar-hist-label">最近任务(点击重填)</div>' +
      hist.map(function (t) { return '<span class="ar-hist-item" data-hist="' + esc(t) + '" title="' + esc(t) + '">' + esc(t) + "</span>"; }).join("") +
      "</div>";
    host.querySelectorAll("[data-hist]").forEach(function (el) {
      el.addEventListener("click", function () {
        var ta = document.getElementById("agentrun-task");
        if (ta) { ta.value = el.getAttribute("data-hist"); ta.focus(); }
      });
    });
  }

  function updateAgentRunAgentSelect() {
    var sel = document.getElementById("agentrun-agent");
    if (!sel) return;
    var cur = sel.value;
    var agents = Array.from(state.agents.values());
    var html = '<option value="">自动选择</option>';
    agents.forEach(function (a) {
      html += '<option value="' + esc(a.agent_id) + '">' + esc(a.agent_id) + " (" + esc(a.hostname || "") + ")</option>";
    });
    sel.innerHTML = html;
    if (cur && state.agents.has(cur)) sel.value = cur;
  }

  // __submitAgentRun: POST /api/agent/run(非 EventSource,因 SSE 是 POST)→ ReadableStream 按 \n\n 分帧。
  window.__submitAgentRun = async function () {
    if (state.loginShowing) return;
    var taskEl = document.getElementById("agentrun-task");
    var task = (taskEl && taskEl.value || "").trim();
    var agentID = (document.getElementById("agentrun-agent") || {}).value || "";
    if (!task) { alert("请描述任务"); return; }
    saveAgentHistory(task);   // 记入历史
    renderAgentHistory();
    var out = document.getElementById("agentrun-output");
    var btn = document.getElementById("agentrun-btn");
    if (btn) btn.disabled = true;
    state.agentRun = { steps: [], summary: "", status: "running", runId: "", model: state.agentRun.model };

    try {
      var resp = await fetch(api("/api/agent/run"), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({ task: task, agent_id: agentID }),
      });
      var ct = resp.headers.get("Content-Type") || "";
      if (ct.indexOf("text/event-stream") < 0) {
        var errData = {};
        try { errData = await resp.json(); } catch (e) {}
        var msg = resp.status === 503 ? "Agent 编排未启用(服务器未配置 LLM,需 -llm-base-url)"
          : resp.status === 401 ? "未登录或会话过期"
          : (errData.error || ("HTTP " + resp.status));
        out.innerHTML = '<div class="agentrun-status" style="color:#f85149">✗ ' + esc(msg) + "</div>";
        if (btn) btn.disabled = false;
        return;
      }
      ensureAgentRunContainers(out);
      setArStatus("⏳ 思考中(LLM 规划)...");
      var reader = resp.body.getReader();
      var decoder = new TextDecoder();
      var buffer = "";
      while (true) {
        var chunk = await reader.read();
        if (chunk.done) break;
        buffer += decoder.decode(chunk.value, { stream: true });
        var idx;
        while ((idx = buffer.indexOf("\n\n")) >= 0) {
          handleAgentRunEvent(buffer.slice(0, idx));
          buffer = buffer.slice(idx + 2);
        }
      }
      if (buffer.trim()) handleAgentRunEvent(buffer);
    } catch (e) {
      out.innerHTML += '<div class="agentrun-status" style="color:#f85149">异常: ' + esc(e.message) + "</div>";
    }
    if (btn) btn.disabled = false;
  };

  function ensureAgentRunContainers(out) {
    if (!out.querySelector("#ar-status")) {
      out.innerHTML = '<div class="agentrun-status" id="ar-status"></div><div id="ar-steps"></div><div id="ar-summary"></div>';
    }
  }
  function setArStatus(text, cls) {
    var el = document.getElementById("ar-status");
    if (el) { el.textContent = text; el.className = "agentrun-status" + (cls ? " " + cls : ""); }
  }

  // handleAgentRunEvent 解析一帧("event: xxx\ndata: {...}"),unwrap 信封 .data,分发。
  function handleAgentRunEvent(frame) {
    var eventName = "", dataLine = "";
    frame.split("\n").forEach(function (ln) {
      if (ln.indexOf("event:") === 0) eventName = ln.slice(6).trim();
      else if (ln.indexOf("data:") === 0) dataLine = ln.slice(5).trim();
    });
    if (!eventName || !dataLine) return;
    var env;
    try { env = JSON.parse(dataLine); } catch (e) { return; }
    onAgentRunEvent(eventName, env.data || {});
  }

  function onAgentRunEvent(name, p) {
    if (name === "agent_queued") {
      state.agentRun.status = "queued";
      setArStatus("⏳ 排队中(并发已满,等待其他任务完成)");
    } else if (name === "agent_start") {
      state.agentRun.runId = p.run_id || "";
      state.agentRun.model = p.model || "";
      var m = document.getElementById("agentrun-model");
      if (m) m.textContent = "(模型: " + (p.model || "?") + ")";
      setArStatus("🔄 执行中(run " + state.agentRun.runId + ")...");
    } else if (name === "agent_plan") {
      state.agentRun.steps = (p.steps || []).map(function (s) {
        return { index: s.index, goal: s.goal, tool: s.tool, args: s.args, status: s.status || "pending", result: "" };
      });
      renderArSteps();
      setArStatus(p.replanned ? "🔄 重新规划(部分步骤失败,调整计划)" : "📋 计划已生成,执行中");
    } else if (name === "step_start") {
      var s1 = findArStep(p.index); if (s1) s1.status = "running";
      renderArSteps();
    } else if (name === "tool_call") {
      var s2 = findArStep(p.index); if (s2) { s2.tool = p.tool; s2.args = p.args; }
      renderArSteps();
    } else if (name === "tool_result") {
      var s3 = findArStep(p.index);
      if (s3) { s3.result = (p.ok ? "✓ " : "✗ ") + (p.summary || ""); s3.ok = !!p.ok; }
      renderArSteps();
    } else if (name === "step_done") {
      var s4 = findArStep(p.index); if (s4) s4.status = "done";
      renderArSteps();
    } else if (name === "step_failed") {
      var s5 = findArStep(p.index); if (s5) { s5.status = "failed"; s5.result = "✗ " + (p.error || ""); }
      renderArSteps();
    } else if (name === "agent_delta") {
      state.agentRun.summary += (p.delta || "");
      renderArSummary();
    } else if (name === "agent_final") {
      if (p.answer) state.agentRun.summary = p.answer;
      state.agentRun.status = "done";
      renderArSummary();
      setArStatus("✓ 完成");
    } else if (name === "agent_error") {
      state.agentRun.status = "error";
      setArStatus("✗ 错误: " + (p.error || ""), "fail");
    }
  }

  function findArStep(index) {
    for (var i = 0; i < state.agentRun.steps.length; i++) {
      if (state.agentRun.steps[i].index === index) return state.agentRun.steps[i];
    }
    return null;
  }

  function renderArSteps() {
    var el = document.getElementById("ar-steps");
    if (!el) return;
    var html = "";
    state.agentRun.steps.forEach(function (s) {
      var icon = s.status === "done" ? '<span class="ok">✓</span>'
        : s.status === "failed" ? '<span class="fail">✗</span>'
        : s.status === "running" ? '<span class="run">🔄</span>'
        : '<span class="pend">○</span>';
      html += '<div class="agentrun-step"><div class="ar-head"><span class="ar-idx">' + (s.index + 1) + "</span>" + icon +
        ' <span class="ar-goal">' + esc(s.goal || "") + "</span>" +
        (s.tool ? ' <span class="ar-tool">' + esc(s.tool) + "</span>" : "") + "</div>";
      if (s.args) html += '<div class="ar-args">args: ' + esc(JSON.stringify(s.args)) + "</div>";
      if (s.result) {
        var border = s.ok === false ? "border-left:2px solid #f85149;" : "border-left:2px solid #3fb950;";
        html += '<div class="ar-result" style="' + border + '">' + esc(s.result) + "</div>";
      }
      html += "</div>";
    });
    el.innerHTML = html;
  }

  function renderArSummary() {
    var el = document.getElementById("ar-summary");
    if (!el) return;
    el.innerHTML = state.agentRun.summary
      ? '<div class="ar-label">汇总(LLM 流式)</div>' + esc(state.agentRun.summary)
      : "";
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
      const r = await apiFetch(api("/api/shell/open"), {
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
      await apiFetch(api("/api/shell/input"), {
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
      await apiFetch(api("/api/shell/close"), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ session_id: sid, agent_id: state.selected }),
      });
    } catch (e) {}
    var content = document.getElementById("shell-content");
    if (content) { content.innerHTML += '\n<span style="color:#8b949e">[会话已关闭]</span>'; }
  };

  // J3：上传文件（B2：用 XMLHttpRequest 显示上传进度;2026-08-12 withCredentials 带 cookie）
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
    xhr.withCredentials = true; // 带会话 cookie
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
  // cookie 为 SameSite=Lax + Path=/,浏览器对同源 <a> 下载(GET 顶级导航)自动携带。
  window.__downloadFile = function () {
    if (!state.selected) { alert("请先选择 Agent"); return; }
    var path = document.getElementById("xfer-download-path").value;
    if (!path) { alert("请填写路径"); return; }
    var transferID = "xfer-" + Date.now();
    document.getElementById("xfer-progress").innerHTML = '<span style="color:#d29922">⏳ 下载中（浏览器原生流式下载）...</span>';
    // B1 修复：用隐藏 <a> 让浏览器原生流式落盘，不用 blob() 全载内存
    var url = api("/api/transfer/download?agent_id=" + encodeURIComponent(state.selected) +
      "&path=" + encodeURIComponent(path) + "&transfer_id=" + transferID);
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

  // --- 启动(2026-08-12:先查会话,再决定显示登录页或主 UI)---
  checkSession();
})();
