import { reduceStream, boundedTimeline, APPLY, RESYNC } from "/stream-model.mjs";

// Ago board interface.
//
// The interface owns no task state. Every column, card, and badge is rendered
// from a snapshot the server produced, and every control submits a protocol
// command and then re-reads the result. Nothing is ever shown as done, claimed,
// or retried because the browser expects it to be — only because the durable
// graph says so.

const COLUMNS = ["Backlog", "Ready", "Claimed", "Running", "Review", "Blocked", "Done"];

const state = {
  boardId: null,
  snapshot: null,
  detail: null,
  openTaskId: null,
  source: null,
  lastSequence: 0,
  timeline: [],
  resyncs: 0,
};

// Exposed for end-to-end assertions about stream health. It is read-only
// diagnostics: nothing here can change what the board shows.
window.agoDiagnostics = () => ({
  cursor: state.lastSequence,
  resyncs: state.resyncs,
  timeline: state.timeline.length,
  boardId: state.boardId,
});

const el = (id) => document.getElementById(id);
const testid = (name) => document.querySelector(`[data-testid="${name}"]`);

// text() is used for every value that came from the server. Assigning to
// textContent means executor output can never become markup.
function text(value) {
  return value === undefined || value === null ? "" : String(value);
}

function commandId(prefix) {
  return `${prefix}-${Date.now()}-${Math.random().toString(16).slice(2, 10)}`;
}

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: options.body ? { "Content-Type": "application/json" } : undefined,
  });
  const body = response.status === 204 ? null : await response.json().catch(() => null);
  if (!response.ok) {
    const message = body && body.error ? body.error.message : `请求失败（${response.status}）`;
    throw new Error(message);
  }
  return body;
}

// ---------- goal creation ----------

async function createGoal() {
  const errorNode = el("composer-error");
  errorNode.hidden = true;
  const objective = el("objective").value.trim();
  const root = el("repo-root").value.trim();
  try {
    const created = await api("/api/v1/goals", {
      method: "POST",
      body: JSON.stringify({
        command_id: commandId("goal"),
        objective,
        repository: { root },
        execution_mode: el("execution-mode").value,
      }),
    });
    openBoard(created.board);
  } catch (error) {
    errorNode.textContent = text(error.message);
    errorNode.hidden = false;
  }
}

function openBoard(snapshot) {
  state.boardId = snapshot.board_id;
  try {
    window.history.replaceState({}, "", `/boards/${snapshot.board_id}`);
    window.localStorage.setItem("ago.board", snapshot.board_id);
  } catch (_) {
    // Storage is a convenience for reload recovery, never a source of truth.
  }
  applySnapshot(snapshot);
  el("goal-panel").hidden = false;
  el("board").hidden = false;
  el("composer").hidden = true;
  subscribe();
}

// ---------- rendering ----------

function applySnapshot(snapshot) {
  state.snapshot = snapshot;
  state.lastSequence = snapshot.latest_event_sequence;
  renderGoal(snapshot);
  renderBoard(snapshot);
  if (state.openTaskId) {
    loadTaskDetail(state.openTaskId);
  }
}

function renderGoal(snapshot) {
  el("goal-objective").textContent = text(snapshot.goal.objective);
  el("goal-meta").textContent =
    `仓库 ${text(snapshot.goal.repository.root)} · 执行方式 ${text(snapshot.goal.execution_mode)} · 图版本 ${text(snapshot.graph_version)}`;
  const progress = snapshot.progress;
  const done = progress.passed;
  const percent = progress.total ? Math.round((done / progress.total) * 100) : 0;
  el("progress-fill").style.width = `${percent}%`;
  el("progress-text").textContent =
    `${done}/${progress.total} 已完成 · ${progress.failed} 失败 · ${progress.remaining} 进行中 · ${text(progress.status)}`;
  el("paused-badge").hidden = !snapshot.paused;
  el("pause").disabled = snapshot.paused;
  el("resume").disabled = !snapshot.paused;

  const list = el("plan-list");
  list.replaceChildren();
  for (const column of snapshot.columns) {
    for (const task of column.tasks) {
      const item = document.createElement("li");
      item.dataset.testid = `plan-item-${task.id}`;
      const title = document.createElement("span");
      title.textContent = text(task.title);
      item.append(title);
      if (task.depends_on && task.depends_on.length) {
        const deps = document.createElement("span");
        deps.className = "muted";
        deps.textContent = ` ← 依赖 ${task.depends_on.join("、")}`;
        item.append(deps);
      }
      list.append(item);
    }
  }
}

function renderBoard(snapshot) {
  const board = el("board");
  board.replaceChildren();
  for (const name of COLUMNS) {
    const column = snapshot.columns.find((c) => c.name === name) || { name, tasks: [] };
    const section = document.createElement("section");
    section.className = "column";
    section.dataset.testid = `column-${name}`;

    const heading = document.createElement("h3");
    heading.textContent = `${name} (${column.tasks.length})`;
    section.append(heading);

    for (const task of column.tasks) {
      section.append(renderCard(task));
    }
    board.append(section);
  }
}

function renderCard(task) {
  const card = document.createElement("article");
  card.className = `card card-${task.state}`;
  card.dataset.testid = `card-${task.id}`;
  card.dataset.state = task.state;
  card.tabIndex = 0;

  const title = document.createElement("h4");
  title.textContent = text(task.title);
  card.append(title);

  const meta = document.createElement("dl");
  meta.className = "card-meta";
  addMeta(meta, "状态", task.state);
  if (task.depends_on && task.depends_on.length) {
    addMeta(meta, "依赖", String(task.depends_on.length));
  }
  card.append(meta);

  // A retry countdown is derived from the durable deadline the server sent,
  // never from a timer the page started on its own.
  if (task.state === "retry-wait" && task.next_eligible_at) {
    const countdown = document.createElement("p");
    countdown.className = "countdown";
    countdown.dataset.testid = `countdown-${task.id}`;
    countdown.dataset.deadline = task.next_eligible_at;
    card.append(countdown);
  }
  if (task.blocked_reason) {
    const blocker = document.createElement("p");
    blocker.className = "blocker";
    blocker.dataset.testid = `blocker-${task.id}`;
    blocker.textContent = text(task.blocked_reason);
    card.append(blocker);
  }

  card.addEventListener("click", () => openTask(task.id));
  card.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      openTask(task.id);
    }
  });
  return card;
}

function addMeta(list, label, value) {
  const dt = document.createElement("dt");
  dt.textContent = label;
  const dd = document.createElement("dd");
  dd.textContent = text(value);
  list.append(dt, dd);
}

function tickCountdowns() {
  const now = Date.now();
  for (const node of document.querySelectorAll(".countdown")) {
    const remaining = Math.max(0, Math.round((Date.parse(node.dataset.deadline) - now) / 1000));
    node.textContent = remaining > 0 ? `${remaining} 秒后重试` : "即将重试";
  }
}

// ---------- task drawer ----------

async function openTask(taskId) {
  state.openTaskId = taskId;
  el("drawer").hidden = false;
  el("scrim").hidden = false;
  await loadTaskDetail(taskId);
}

function closeDrawer() {
  state.openTaskId = null;
  el("drawer").hidden = true;
  el("scrim").hidden = true;
}

async function loadTaskDetail(taskId) {
  try {
    const detail = await api(`/api/v1/boards/${state.boardId}/tasks/${taskId}`);
    state.detail = detail;
    renderDrawer(detail);
  } catch (error) {
    el("drawer-body").textContent = text(error.message);
  }
}

function renderDrawer(detail) {
  el("drawer-title").textContent = text(detail.title);
  const body = el("drawer-body");
  body.replaceChildren();

  body.append(section("任务契约", (host) => {
    host.append(paragraph(detail.terminal_contract.outcome));
    host.append(labelledList("验收标准", detail.terminal_contract.acceptance_criteria, "acceptance"));
    host.append(labelledList("能力标签", detail.capability_tags, "capabilities"));
    host.append(labelledList("路径范围", detail.path_scopes, "scopes"));
    host.append(keyValue("访问模式", detail.access_mode, "access-mode"));
    host.append(keyValue("尝试次数", `${detail.attempt_count}/${detail.max_attempts}`, "attempt-summary"));
    host.append(labelledList("依赖", detail.depends_on, "depends-on"));
    host.append(labelledList("被依赖", detail.required_by, "required-by"));
  }));

  if (detail.state === "failed" || detail.state === "retry-wait") {
    body.append(section("需要处理", (host) => {
      if (detail.blocked_reason) {
        host.append(paragraph(detail.blocked_reason, "blocked-reason"));
      }
      const form = document.createElement("form");
      form.dataset.testid = "input-form";
      const input = document.createElement("input");
      input.type = "text";
      input.dataset.testid = "input-note";
      input.placeholder = "补充信息或批准说明";
      const submit = document.createElement("button");
      submit.type = "submit";
      submit.dataset.testid = "retry-submit";
      submit.textContent = "提交并重试";
      form.append(input, submit);
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        submit.disabled = true;
        try {
          const snapshot = await api(`/api/v1/boards/${state.boardId}/tasks/${detail.task_id}/retry`, {
            method: "POST",
            body: JSON.stringify({ command_id: commandId("retry"), reason: input.value.trim() }),
          });
          applySnapshot(snapshot);
        } catch (error) {
          host.append(paragraph(error.message, "retry-error"));
        } finally {
          submit.disabled = false;
        }
      });
      host.append(form);
    }));
  }

  body.append(section("尝试记录", (host) => {
    if (!detail.attempts.length) {
      host.append(paragraph("尚无尝试。"));
      return;
    }
    for (const attempt of detail.attempts) {
      const entry = document.createElement("div");
      entry.className = "entry";
      entry.dataset.testid = `attempt-entry-${attempt.id}`;
      entry.append(keyValue("第几次", attempt.number));
      entry.append(keyValue("状态", attempt.state));
      entry.append(keyValue("执行者", attempt.worker_id));
      entry.append(keyValue("代次", attempt.generation));
      if (attempt.failure_class) {
        entry.append(keyValue("失败类别", attempt.failure_class));
      }
      if (attempt.failure_reason) {
        entry.append(keyValue("失败原因", attempt.failure_reason));
      }
      host.append(entry);
    }
  }));

  body.append(section("证据与验收", (host) => {
    if (!detail.evidence.length) {
      host.append(paragraph("尚无证据。"));
      return;
    }
    for (const evidence of detail.evidence) {
      const entry = document.createElement("div");
      entry.className = "entry";
      entry.dataset.testid = `evidence-${evidence.id}`;
      entry.append(keyValue("摘要", evidence.summary));
      entry.append(keyValue("状态", evidence.state));
      if (evidence.verdict) {
        entry.append(keyValue("裁定", evidence.verdict, "verdict"));
      }
      if (evidence.verdict_reason) {
        entry.append(keyValue("裁定理由", evidence.verdict_reason));
      }
      const result = evidence.result || {};
      for (const test of result.tests || []) {
        const row = document.createElement("p");
        row.className = test.passed ? "test-pass" : "test-fail";
        row.dataset.testid = `test-${test.name}`;
        row.textContent = `${test.required ? "必需 " : ""}${text(test.name)} · ${test.passed ? "通过" : "未通过"} · 退出码 ${text(test.exit_code)}`;
        entry.append(row);
      }
      for (const file of result.changed_files || []) {
        entry.append(keyValue("变更文件", `${text(file.path)} ${text(file.before_hash).slice(0, 8)}→${text(file.after_hash).slice(0, 8)}`));
      }
      for (const command of result.commands || []) {
        entry.append(keyValue("命令", `${text(command.display)} · 退出码 ${text(command.exit_code)} · ${text(command.duration_ms)}ms`));
      }
      for (const artifact of result.artifacts || []) {
        const link = document.createElement("a");
        link.href = `/api/v1/boards/${state.boardId}/artifacts/${artifact.id}`;
        link.textContent = `${text(artifact.display_name)}（${text(artifact.bytes)} 字节）`;
        link.dataset.testid = `artifact-${artifact.id}`;
        link.setAttribute("download", "");
        const wrapper = document.createElement("p");
        wrapper.append(link);
        entry.append(wrapper);
      }
      for (const warning of result.warnings || []) {
        entry.append(paragraph(warning, "warning"));
      }
      host.append(entry);
    }
  }));

  body.append(section("活动时间线", (host) => {
    const entries = state.timeline.filter((item) => item.taskId === detail.task_id).slice(-25);
    if (!entries.length) {
      host.append(paragraph("暂无活动。"));
      return;
    }
    const list = document.createElement("ol");
    list.dataset.testid = "timeline";
    for (const item of entries) {
      const li = document.createElement("li");
      li.textContent = `#${item.sequence} ${item.type}${item.reason ? " · " + item.reason : ""}`;
      list.append(li);
    }
    host.append(list);
  }));
}

function section(title, build) {
  const node = document.createElement("section");
  node.className = "drawer-section";
  const heading = document.createElement("h3");
  heading.textContent = title;
  node.append(heading);
  build(node);
  return node;
}

function paragraph(value, id) {
  const node = document.createElement("p");
  if (id) node.dataset.testid = id;
  node.textContent = text(value);
  return node;
}

function keyValue(label, value, id) {
  const node = document.createElement("p");
  node.className = "kv";
  if (id) node.dataset.testid = id;
  const strong = document.createElement("strong");
  strong.textContent = `${label}：`;
  const span = document.createElement("span");
  span.textContent = text(value);
  node.append(strong, span);
  return node;
}

function labelledList(label, values, id) {
  const node = document.createElement("div");
  if (id) node.dataset.testid = id;
  const heading = document.createElement("p");
  heading.className = "kv";
  const strong = document.createElement("strong");
  strong.textContent = `${label}：`;
  heading.append(strong);
  node.append(heading);
  const list = document.createElement("ul");
  for (const value of values || []) {
    const item = document.createElement("li");
    item.textContent = text(value);
    list.append(item);
  }
  node.append(list);
  return node;
}

// ---------- live updates ----------

function subscribe() {
  if (state.source) {
    state.source.close();
  }
  // EventSource reconnects on its own and replays Last-Event-ID, so the cursor
  // is the server's durable sequence rather than anything tracked here.
  const source = new EventSource(`/api/v1/boards/${state.boardId}/events?after=${state.lastSequence}`);
  state.source = source;

  source.addEventListener("open", () => setStreamState("已连接", "pill-live"));
  source.addEventListener("error", () => setStreamState("重连中", "pill-idle"));
  source.addEventListener("message", (event) => {
    let payload;
    try {
      payload = JSON.parse(event.data);
    } catch (_) {
      return;
    }
    // The cursor rules live in a pure module so they can be tested directly.
    const next = reduceStream({ cursor: state.lastSequence }, payload);
    state.lastSequence = next.cursor;
    if (next.action === RESYNC) {
      // This client missed something. Re-read the authoritative snapshot
      // instead of guessing at the gap.
      state.resyncs += 1;
      refreshSnapshot();
      return;
    }
    if (next.action !== APPLY) {
      return;
    }
    state.timeline = boundedTimeline(state.timeline, {
      sequence: payload.sequence,
      type: payload.type,
      reason: payload.reason,
      taskId: payload.task ? payload.task.id : null,
    });
    refreshSnapshot();
  });
}

let refreshPending = false;
async function refreshSnapshot() {
  if (refreshPending) return;
  refreshPending = true;
  try {
    const snapshot = await api(`/api/v1/boards/${state.boardId}`);
    applySnapshot(snapshot);
  } catch (_) {
    // A failed refresh is transient; the next event or poll retries.
  } finally {
    refreshPending = false;
  }
}

// Releases the event stream. The page calls this when it is being unloaded so
// the connection ends cleanly instead of being severed mid-response, which
// also lets any recording proxy or debugger finalise the request.
function stopStream() {
  if (state.source) {
    state.source.close();
    state.source = null;
  }
  setStreamState("未连接", "pill-idle");
}
window.agoStopStream = stopStream;

function setStreamState(label, className) {
  const node = el("stream-state");
  node.textContent = label;
  node.className = `pill ${className}`;
}

async function loadProviders() {
  try {
    const body = await api("/api/v1/providers");
    const names = (body.providers || []).map((p) => `${p.id}${p.auth_configured ? "（已配置认证）" : ""}`);
    el("provider-health").textContent = names.length ? `提供方：${names.join(" · ")}` : "无提供方";
  } catch (_) {
    el("provider-health").textContent = "提供方状态不可用";
  }
}

async function control(path, prefix) {
  try {
    const snapshot = await api(`/api/v1/boards/${state.boardId}/${path}`, {
      method: "POST",
      body: JSON.stringify({ command_id: commandId(prefix), reason: "用户操作" }),
    });
    applySnapshot(snapshot);
  } catch (error) {
    el("goal-meta").textContent = text(error.message);
  }
}

// ---------- startup ----------

function start() {
  el("create-goal").addEventListener("click", createGoal);
  el("drawer-close").addEventListener("click", closeDrawer);
  el("scrim").addEventListener("click", closeDrawer);
  el("pause").addEventListener("click", () => control("pause", "pause"));
  el("resume").addEventListener("click", () => control("resume", "resume"));
  setInterval(tickCountdowns, 500);
  window.addEventListener("pagehide", stopStream);
  loadProviders();

  // A reload or a server restart resumes the board from its identifier in the
  // URL; all state comes back from the server.
  const fromPath = window.location.pathname.match(/^\/boards\/(.+)$/);
  let boardId = fromPath ? decodeURIComponent(fromPath[1]) : null;
  if (!boardId) {
    try {
      boardId = window.localStorage.getItem("ago.board");
    } catch (_) {
      boardId = null;
    }
  }
  if (boardId) {
    state.boardId = boardId;
    api(`/api/v1/boards/${boardId}`)
      .then((snapshot) => openBoard(snapshot))
      .catch(() => {
        state.boardId = null;
      });
  }
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", start);
} else {
  start();
}
