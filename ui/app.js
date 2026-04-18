const state = {
  authenticated: false,
  accountStoreEnabled: false,
  health: null,
  models: [],
  accounts: [],
  activeAccountId: "",
  debugAbortController: null,
  toastTimer: null,
};

const el = {};

document.addEventListener("DOMContentLoaded", () => {
  bindElements();
  bindEvents();
  boot();
});

function bindElements() {
  [
    "refreshButton",
    "logoutButton",
    "metricStatus",
    "metricStarted",
    "metricUptime",
    "metricModels",
    "metricAccountsMode",
    "metricPools",
    "runtimeState",
    "modelSearch",
    "modelList",
    "accountStoreBadge",
    "accountForm",
    "accountId",
    "accountLabel",
    "accountToken",
    "accountPriority",
    "accountWeight",
    "accountEnabled",
    "accountSubmitButton",
    "accountResetButton",
    "accountList",
    "debugForm",
    "debugModel",
    "debugSystem",
    "debugPrompt",
    "debugStream",
    "debugSubmitButton",
    "debugAbortButton",
    "debugOutput",
    "copyOutputButton",
    "toast",
    "loginOverlay",
    "loginForm",
    "loginPassword",
  ].forEach((id) => {
    el[id] = document.getElementById(id);
  });
}

function bindEvents() {
  el.loginForm.addEventListener("submit", onLoginSubmit);
  el.refreshButton.addEventListener("click", () => guarded(loadAll));
  el.logoutButton.addEventListener("click", onLogout);
  el.accountForm.addEventListener("submit", onAccountSubmit);
  el.accountResetButton.addEventListener("click", resetAccountForm);
  el.accountList.addEventListener("click", onAccountAction);
  el.modelSearch.addEventListener("input", renderModels);
  el.debugForm.addEventListener("submit", onDebugSubmit);
  el.debugAbortButton.addEventListener("click", abortDebugRequest);
  el.copyOutputButton.addEventListener("click", copyDebugOutput);
}

async function boot() {
  try {
    const session = await fetchJSON("/web/session");
    state.accountStoreEnabled = Boolean(session.account_store_enabled);
    state.authenticated = Boolean(session.authenticated);
    updateSessionUI();
    if (state.authenticated) {
      await loadAll();
    }
  } catch (error) {
    toast(error.message || "初始化失败", true);
  }
}

function updateSessionUI() {
  el.loginOverlay.classList.toggle("hidden", state.authenticated);
  el.accountStoreBadge.textContent = state.accountStoreEnabled ? "SQLite 已启用" : "Legacy 模式";
  el.metricAccountsMode.textContent = state.accountStoreEnabled ? "账号池模式" : "Legacy Token 模式";
}

async function onLoginSubmit(event) {
  event.preventDefault();
  const password = el.loginPassword.value.trim();
  if (!password) {
    toast("请输入访问密码", true);
    return;
  }

  try {
    await fetchJSON("/web/login", {
      method: "POST",
      body: JSON.stringify({ password }),
    });
    el.loginPassword.value = "";
    state.authenticated = true;
    updateSessionUI();
    await loadAll();
    toast("登录成功");
  } catch (error) {
    toast(error.message || "登录失败", true);
  }
}

async function onLogout() {
  try {
    await fetchJSON("/web/logout", { method: "POST" });
  } catch (_) {
  }
  state.authenticated = false;
  updateSessionUI();
}

async function loadAll() {
  const requests = [
    fetchJSON("/web/api/healthz"),
    fetchJSON("/web/api/models"),
    fetchJSON("/web/api/accounts").catch((error) => {
      if (!state.accountStoreEnabled) {
        return { data: [] };
      }
      throw error;
    }),
  ];

  const [health, modelsPayload, accountsPayload] = await Promise.all(requests);
  state.health = health;
  state.models = modelsPayload.data || [];
  state.accounts = accountsPayload.data || [];

  renderHealth();
  renderModels();
  renderAccounts();
  fillModelSelect();
}

function renderHealth() {
  const health = state.health;
  if (!health) {
    return;
  }

  el.metricStatus.textContent = health.ok ? "在线" : "异常";
  el.metricStarted.textContent = formatDateTime(health.started_at);
  el.metricUptime.textContent = String(health.uptime_sec ?? "-");
  el.metricModels.textContent = String(state.models.length);
  el.metricPools.textContent = String((health.token_state || []).length);

  const snapshots = Array.isArray(health.token_state) ? health.token_state : [];
  if (snapshots.length === 0) {
    el.runtimeState.innerHTML = `<div class="empty-state">当前没有可参与轮换的账号池。通常意味着还没添加账号，或所有账号都处于 invalid 状态。</div>`;
    return;
  }

  el.runtimeState.innerHTML = snapshots
    .map((snapshot) => {
      const runs = (snapshot.runs || [])
        .map(
          (run) => `
            <div class="run-line">
              <strong>${escapeHTML(run.agent_id)}</strong>
              <span>run=${escapeHTML(run.run_id)}</span>
              <span>并发 ${run.inflight}</span>
              <span>请求 ${run.request_count}</span>
            </div>`
        )
        .join("");
      const coolDown = snapshot.cooldown_until ? `<span class="status-pill status-error">冷却到 ${escapeHTML(formatDateTime(snapshot.cooldown_until))}</span>` : "";
      const lastError = snapshot.last_error ? `<div class="account-meta">最近错误：${escapeHTML(snapshot.last_error)}</div>` : "";
      return `
        <article class="runtime-card">
          <header>
            <div>
              <strong>${escapeHTML(snapshot.name)}</strong>
              <div class="account-meta">优先级 ${snapshot.priority} · 权重 ${snapshot.weight} · draining ${snapshot.draining_runs}</div>
            </div>
            <div class="inline-meta">
              <span class="badge">${escapeHTML(snapshot.id)}</span>
              ${coolDown}
            </div>
          </header>
          ${lastError}
          <div class="runtime-runs">${runs || `<div class="account-meta">尚未预热出 run，或当前池已被移出调度。</div>`}</div>
        </article>`;
    })
    .join("");
}

function renderModels() {
  const query = el.modelSearch.value.trim().toLowerCase();
  const models = state.models.filter((item) => item.id.toLowerCase().includes(query));
  if (models.length === 0) {
    el.modelList.innerHTML = `<div class="empty-state">没有匹配的模型。</div>`;
    return;
  }
  el.modelList.innerHTML = models
    .map(
      (model) => `
        <div class="model-item">
          <strong>${escapeHTML(model.id)}</strong>
        </div>`
    )
    .join("");
}

function renderAccounts() {
  if (!state.accountStoreEnabled) {
    el.accountList.innerHTML = `<div class="empty-state">当前运行在 Legacy 模式。配置 DATA_DB_PATH 与 DATA_ENCRYPTION_KEY 后即可启用 SQLite 账号池。</div>`;
    return;
  }

  if (state.accounts.length === 0) {
    el.accountList.innerHTML = `<div class="empty-state">还没有账号。先在上方表单新增一个 token。</div>`;
    return;
  }

  el.accountList.innerHTML = state.accounts
    .map((account) => {
      const selected = state.activeAccountId === account.id ? "style='border-color: rgba(99, 102, 241, 0.45); box-shadow: 0 0 0 1px rgba(99, 102, 241, 0.25)'" : "";
      return `
        <article class="account-card" ${selected}>
          <header>
            <div>
              <strong>${escapeHTML(account.label)}</strong>
              <div class="account-meta">${escapeHTML(account.token_preview || "")}</div>
            </div>
            <span class="status-pill status-${escapeHTML(account.last_status)}">${escapeHTML(account.last_status)}</span>
          </header>
          <div class="account-meta">
            <div>优先级 ${account.priority} · 权重 ${account.weight} · ${account.enabled ? "已启用" : "已停用"}</div>
            <div>最近检查：${account.last_checked_at ? escapeHTML(formatDateTime(account.last_checked_at)) : "未校验"}</div>
            <div>${account.last_error ? `错误：${escapeHTML(account.last_error)}` : "暂无错误"}</div>
          </div>
          <div class="account-actions">
            <button class="ghost-button small" data-action="edit" data-id="${escapeHTML(account.id)}">编辑</button>
            <button class="ghost-button small" data-action="validate" data-id="${escapeHTML(account.id)}">校验</button>
            <button class="ghost-button small" data-action="toggle" data-id="${escapeHTML(account.id)}">${account.enabled ? "停用" : "启用"}</button>
            <button class="ghost-button small danger" data-action="delete" data-id="${escapeHTML(account.id)}">删除</button>
          </div>
        </article>`;
    })
    .join("");
}

function fillModelSelect() {
  const current = el.debugModel.value;
  el.debugModel.innerHTML = state.models
    .map((model) => `<option value="${escapeHTML(model.id)}">${escapeHTML(model.id)}</option>`)
    .join("");
  if (current && state.models.some((model) => model.id === current)) {
    el.debugModel.value = current;
  }
}

async function onAccountSubmit(event) {
  event.preventDefault();
  if (!state.accountStoreEnabled) {
    toast("当前未启用账号池存储", true);
    return;
  }

  const id = el.accountId.value.trim();
  const payload = {
    label: el.accountLabel.value.trim(),
    enabled: el.accountEnabled.checked,
    priority: Number(el.accountPriority.value || 0),
    weight: Number(el.accountWeight.value || 1),
  };
  const token = el.accountToken.value.trim();
  if (id) {
    if (token) {
      payload.token = token;
    }
  } else {
    payload.token = token;
  }

  if (!payload.label) {
    toast("请输入账号标签", true);
    return;
  }
  if (!id && !payload.token) {
    toast("新增账号时必须填写 token", true);
    return;
  }

  try {
    const path = id ? `/web/api/accounts/${encodeURIComponent(id)}` : "/web/api/accounts";
    const method = id ? "PATCH" : "POST";
    await fetchJSON(path, {
      method,
      body: JSON.stringify(payload),
    });
    resetAccountForm();
    await loadAll();
    toast(id ? "账号已更新" : "账号已创建");
  } catch (error) {
    toast(error.message || "保存账号失败", true);
  }
}

function resetAccountForm() {
  state.activeAccountId = "";
  el.accountId.value = "";
  el.accountLabel.value = "";
  el.accountToken.value = "";
  el.accountPriority.value = "100";
  el.accountWeight.value = "1";
  el.accountEnabled.checked = true;
  el.accountSubmitButton.textContent = "新增账号";
  renderAccounts();
}

async function onAccountAction(event) {
  const target = event.target.closest("button[data-action]");
  if (!target) {
    return;
  }

  const id = target.dataset.id;
  const action = target.dataset.action;
  const account = state.accounts.find((item) => item.id === id);
  if (!account) {
    return;
  }

  if (action === "edit") {
    state.activeAccountId = id;
    el.accountId.value = id;
    el.accountLabel.value = account.label;
    el.accountToken.value = "";
    el.accountPriority.value = String(account.priority);
    el.accountWeight.value = String(account.weight);
    el.accountEnabled.checked = account.enabled;
    el.accountSubmitButton.textContent = "保存修改";
    renderAccounts();
    el.accountLabel.focus();
    return;
  }

  if (action === "validate") {
    await guarded(async () => {
      await fetchJSON(`/web/api/accounts/${encodeURIComponent(id)}/validate`, { method: "POST" });
      await loadAll();
      toast("校验已完成");
    });
    return;
  }

  if (action === "toggle") {
    await guarded(async () => {
      await fetchJSON(`/web/api/accounts/${encodeURIComponent(id)}`, {
        method: "PATCH",
        body: JSON.stringify({ enabled: !account.enabled }),
      });
      await loadAll();
      toast(account.enabled ? "账号已停用" : "账号已启用");
    });
    return;
  }

  if (action === "delete") {
    const confirmed = window.confirm(`确认删除账号“${account.label}”？`);
    if (!confirmed) {
      return;
    }
    await guarded(async () => {
      await fetchJSON(`/web/api/accounts/${encodeURIComponent(id)}`, { method: "DELETE" });
      if (state.activeAccountId === id) {
        resetAccountForm();
      }
      await loadAll();
      toast("账号已删除");
    });
  }
}

async function onDebugSubmit(event) {
  event.preventDefault();
  const model = el.debugModel.value;
  const prompt = el.debugPrompt.value.trim();
  const system = el.debugSystem.value.trim();
  const stream = el.debugStream.checked;

  if (!model) {
    toast("请先选择模型", true);
    return;
  }
  if (!prompt) {
    toast("请输入调试消息", true);
    return;
  }

  abortDebugRequest();
  const controller = new AbortController();
  state.debugAbortController = controller;
  el.debugOutput.textContent = stream ? "建立流式连接中...\n" : "请求中...\n";

  const messages = [];
  if (system) {
    messages.push({ role: "system", content: system });
  }
  messages.push({ role: "user", content: prompt });

  try {
    const response = await fetch("/web/api/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model, stream, messages }),
      signal: controller.signal,
      credentials: "same-origin",
    });

    if (!response.ok) {
      const error = await parseError(response);
      throw new Error(error);
    }

    if (stream) {
      await consumeStreamResponse(response);
      toast("流式请求已完成");
    } else {
      const payload = await response.json();
      const extracted = extractMessageText(payload);
      el.debugOutput.textContent = extracted ? `${extracted}\n\n--- 原始 JSON ---\n${JSON.stringify(payload, null, 2)}` : JSON.stringify(payload, null, 2);
      toast("非流式请求已完成");
    }
  } catch (error) {
    if (error.name === "AbortError") {
      toast("请求已中止");
      return;
    }
    el.debugOutput.textContent = error.message || "请求失败";
    toast(error.message || "请求失败", true);
  } finally {
    state.debugAbortController = null;
  }
}

function abortDebugRequest() {
  if (state.debugAbortController) {
    state.debugAbortController.abort();
    state.debugAbortController = null;
  }
}

async function consumeStreamResponse(response) {
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let text = "";

  while (true) {
    const { value, done } = await reader.read();
    if (done) {
      break;
    }
    buffer += decoder.decode(value, { stream: true });
    const segments = buffer.split("\n");
    buffer = segments.pop() || "";
    for (const segment of segments) {
      const line = segment.trim();
      if (!line.startsWith("data:")) {
        continue;
      }
      const data = line.slice(5).trim();
      if (!data || data === "[DONE]") {
        continue;
      }
      try {
        const payload = JSON.parse(data);
        const delta = payload?.choices?.[0]?.delta?.content;
        if (typeof delta === "string") {
          text += delta;
          el.debugOutput.textContent = text || JSON.stringify(payload, null, 2);
        }
      } catch (_) {
        el.debugOutput.textContent = `${text}\n${data}`;
      }
    }
  }

  if (!text && buffer.trim()) {
    el.debugOutput.textContent = buffer.trim();
  }
}

async function copyDebugOutput() {
  try {
    await navigator.clipboard.writeText(el.debugOutput.textContent || "");
    toast("输出已复制");
  } catch (error) {
    toast(error.message || "复制失败", true);
  }
}

async function fetchJSON(path, options = {}) {
  const response = await fetch(path, {
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      ...(options.headers || {}),
    },
    ...options,
  });

  if (response.status === 401) {
    state.authenticated = false;
    updateSessionUI();
    throw new Error("会话已过期，请重新登录");
  }

  if (!response.ok) {
    throw new Error(await parseError(response));
  }

  const contentType = response.headers.get("content-type") || "";
  if (contentType.includes("application/json")) {
    return response.json();
  }
  return {};
}

async function parseError(response) {
  try {
    const payload = await response.json();
    return payload?.error?.message || payload?.message || `请求失败 (${response.status})`;
  } catch (_) {
    return `请求失败 (${response.status})`;
  }
}

function extractMessageText(payload) {
  const content = payload?.choices?.[0]?.message?.content;
  if (typeof content === "string") {
    return content;
  }
  if (Array.isArray(content)) {
    return content
      .map((item) => (typeof item?.text === "string" ? item.text : ""))
      .filter(Boolean)
      .join("\n");
  }
  return "";
}

function formatDateTime(value) {
  if (!value) {
    return "-";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "-";
  }
  return date.toLocaleString("zh-CN", { hour12: false });
}

function escapeHTML(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function toast(message, isError = false) {
  el.toast.textContent = message;
  el.toast.classList.toggle("toast-error", isError);
  el.toast.classList.toggle("toast-success", !isError);
  el.toast.classList.add("visible");
  clearTimeout(state.toastTimer);
  state.toastTimer = setTimeout(() => {
    el.toast.classList.remove("visible");
    el.toast.classList.remove("toast-error", "toast-success");
  }, 2600);
}

async function guarded(fn) {
  try {
    await fn();
  } catch (error) {
    toast(error.message || "操作失败", true);
  }
}
