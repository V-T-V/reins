// go-rmm 控制台前端 —— 零依赖原生 JS
//
// 状态机：
//   agents: Map<agentID, Register>  当前已知 Agent
//   selected: string|null           当前选中 Agent
//   evtSource: EventSource          SSE 订阅
(function () {
  "use strict";

  const state = { agents: new Map(), selected: null, evtSource: null };

  // --- Agent 列表 ---
  async function refreshAgents() {
    try {
      const r = await fetch("/api/agents");
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
        document.getElementById("relay-ip").textContent = relayIP;
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
      const r = await fetch("/api/tasks", {
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
    const es = new EventSource("/api/events");
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
      const r = await fetch("/api/tasks?limit=10");
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
      await fetch("/api/tasks/cancel", {
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
      const r = await fetch("/api/metrics");
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
      const r = await fetch("/api/shell/open", {
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
      await fetch("/api/shell/input", {
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
      await fetch("/api/shell/close", {
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

  // J3：上传文件
  window.__uploadFile = async function () {
    if (!state.selected) { alert("请先选择 Agent"); return; }
    var path = document.getElementById("xfer-upload-path").value;
    var fileInput = document.getElementById("xfer-file-input");
    if (!path || !fileInput.files[0]) { alert("请填写路径并选择文件"); return; }
    var file = fileInput.files[0];
    var transferID = "xfer-" + Date.now();
    document.getElementById("xfer-progress").innerHTML = '<span style="color:#d29922">⏳ 上传中...</span>';
    try {
      var resp = await fetch("/api/transfer/upload?agent_id=" + encodeURIComponent(state.selected) +
        "&path=" + encodeURIComponent(path) + "&transfer_id=" + transferID, {
        method: "POST",
        body: file,
      });
      var result = await resp.json();
      if (!resp.ok) {
        document.getElementById("xfer-progress").innerHTML = '<span style="color:#f85149">上传失败: ' + esc(result.error || "") + '</span>';
      }
    } catch (e) {
      document.getElementById("xfer-progress").innerHTML = '<span style="color:#f85149">错误: ' + esc(e.message) + '</span>';
    }
  };

  // J3：下载文件
  window.__downloadFile = async function () {
    if (!state.selected) { alert("请先选择 Agent"); return; }
    var path = document.getElementById("xfer-download-path").value;
    if (!path) { alert("请填写路径"); return; }
    var transferID = "xfer-" + Date.now();
    document.getElementById("xfer-progress").innerHTML = '<span style="color:#d29922">⏳ 下载中...</span>';
    try {
      var resp = await fetch("/api/transfer/download?agent_id=" + encodeURIComponent(state.selected) +
        "&path=" + encodeURIComponent(path) + "&transfer_id=" + transferID);
      if (!resp.ok) {
        var err = await resp.json();
        document.getElementById("xfer-progress").innerHTML = '<span style="color:#f85149">下载失败: ' + esc(err.error || "") + '</span>';
        return;
      }
      // 触发浏览器下载
      var blob = await resp.blob();
      var a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = path.split("\\").pop().split("/").pop();
      a.click();
    } catch (e) {
      document.getElementById("xfer-progress").innerHTML = '<span style="color:#f85149">错误: ' + esc(e.message) + '</span>';
    }
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
})();
