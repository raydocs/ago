import {
  threadBucket,
  groupThreadsForRail,
  stableTitleFromThread,
  compactModelName,
} from "./shell-model.mjs";

const APP_VERSION = "0.2.5";
const state = {
  threads: [],
  currentID: null,
  current: null,
  view: "thread",
  searchTimer: null,
  pollTimer: null,
  refreshing: false,
  pollFailures: 0,
  renderToken: 0,
  outline: {
    marks: [],
    dragging: false,
    hoverIndex: -1,
    raf: 0,
  },
};

const $ = (selector) => document.querySelector(selector);
const on = (selectorOrEl, event, handler) => {
  const node = typeof selectorOrEl === "string" ? $(selectorOrEl) : selectorOrEl;
  if (!node) return;
  node.addEventListener(event, handler);
};
const threadList = $("#thread-list");
const threadDetail = $("#thread-detail");
const emptyState = $("#empty-state");
const drawerScrim = $("#drawer-scrim");
const mobileQuery = window.matchMedia("(max-width: 900px)");
const housekeepingTools = new Set(["TaskCreate", "TaskGet", "TaskUpdate", "TaskList", "TaskOutput"]);
const MAX_ACTIVITY_PREVIEW = 40;

document.addEventListener("DOMContentLoaded", () => {
  boot().catch((error) => showBootError(error));
});

async function boot() {
  try {
    bindEvents();
    restoreRailPreference();
    const userName = $("#user-name");
    const userEmail = $("#user-email");
    if (userName) userName.textContent = "local";
    if (userEmail) userEmail.textContent = "Claude X workspace";
    schedulePoll();
    await refreshAll(true);
    hideBootLoading();
  } catch (error) {
    setSync("offline");
    hideBootLoading();
    showBootError(error);
    throw error;
  }
}

function hideBootLoading() {
  const node = $("#boot-loading");
  if (node) node.hidden = true;
}

function showBootError(error) {
  const node = $("#boot-error");
  if (!node) {
    console.error(error);
    return;
  }
  const message = error?.message || String(error || "unknown error");
  node.hidden = false;
  node.textContent = `页面加载失败（v${APP_VERSION}）：${message}\n请强制刷新（Cmd+Shift+R）。若在 IDE 预览中，也可直接打开 https://claudex-threads.ppop.workers.dev`;
  console.error(error);
}

function bindEvents() {
  on("#refresh-button", "click", () => refreshAll(true));
  on("#mobile-refresh", "click", () => refreshAll(true));
  on("#archive-button", "click", archiveCurrent);
  on("#copy-thread-md", "click", copyThreadMarkdown);
  on("#thread-tab", "click", () => setView("thread"));
  on("#usage-tab", "click", () => setView("usage"));
  on("#nav-activity", "click", () => {
    if (state.currentID) setView("usage");
  });
  on("#nav-projects", "click", () => {
    const panel = $("#search-panel");
    const input = $("#thread-search");
    if (panel) panel.hidden = false;
    if (input) {
      input.placeholder = "Filter by project";
      input.focus();
    }
  });
  on("#sessions-button", "click", openDrawer);
  on("#empty-sessions-button", "click", openDrawer);
  on("#metadata-button", "click", openMetadata);
  on("#metadata-close", "click", closeMetadata);
  on("#rail-peek", "click", expandRail);
  on("#search-toggle", "click", toggleSearchPanel);
  on("#organize-toggle", "click", toggleOrganizeMenu);
  document.querySelectorAll(".organize-item").forEach((item) => {
    item.addEventListener("click", () => selectOrganizeMode(item.dataset.sort));
  });
  document.addEventListener("click", (event) => {
    const menu = $("#organize-menu");
    const toggle = $("#organize-toggle");
    if (!menu || menu.hidden) return;
    if (menu.contains(event.target) || toggle?.contains(event.target)) return;
    closeOrganizeMenu();
  });
  on(drawerScrim, "click", closeOverlays);
  on("#home-link", "click", (event) => {
    event.preventDefault();
    if (mobileQuery.matches) openDrawer();
    else expandRail();
  });
  on("#thread-search", "input", (event) => {
    clearTimeout(state.searchTimer);
    state.searchTimer = setTimeout(() => loadThreads(event.target.value), 180);
  });
  window.addEventListener("hashchange", openFromHash);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) stopPoll();
    else {
      schedulePoll();
      refreshAll(false).catch(() => setSync("offline"));
    }
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      closeOverlays();
      const panel = $("#search-panel");
      if (panel && !panel.hidden) {
        panel.hidden = true;
        $("#search-toggle")?.setAttribute("aria-expanded", "false");
      }
    }
  });
  bindOutlineScrubber();
  window.addEventListener("resize", () => {
    if (state.outline.raf) cancelAnimationFrame(state.outline.raf);
    state.outline.raf = requestAnimationFrame(() => {
      rebuildOutlineMarks();
      updateOutlineThumb();
    });
  });
}

function bindOutlineScrubber() {
  const scroller = $("#conversation-scroll");
  const track = $("#outline-track");
  const scrubber = $("#outline-scrubber");
  if (!scroller || !track || !scrubber) return;

  scroller.addEventListener("scroll", () => {
    if (state.outline.raf) cancelAnimationFrame(state.outline.raf);
    state.outline.raf = requestAnimationFrame(updateOutlineThumb);
  }, { passive: true });

  const scrubFromClientY = (clientY, { jump = false } = {}) => {
    const rect = track.getBoundingClientRect();
    if (rect.height <= 0) return;
    const ratio = Math.min(1, Math.max(0, (clientY - rect.top) / rect.height));
    const max = Math.max(0, scroller.scrollHeight - scroller.clientHeight);
    const top = ratio * max;
    if (jump) scroller.scrollTo({ top, behavior: "smooth" });
    else scroller.scrollTop = top;
    updateOutlineThumb();
  };

  track.addEventListener("pointerdown", (event) => {
    if (event.button !== 0) return;
    state.outline.dragging = true;
    scrubber.classList.add("is-dragging");
    track.setPointerCapture?.(event.pointerId);
    scrubFromClientY(event.clientY);
    event.preventDefault();
  });
  track.addEventListener("pointermove", (event) => {
    if (!state.outline.dragging) return;
    scrubFromClientY(event.clientY);
  });
  const endDrag = (event) => {
    if (!state.outline.dragging) return;
    state.outline.dragging = false;
    scrubber.classList.remove("is-dragging");
    try { track.releasePointerCapture?.(event.pointerId); } catch {}
  };
  track.addEventListener("pointerup", endDrag);
  track.addEventListener("pointercancel", endDrag);
  track.addEventListener("lostpointercapture", endDrag);

  scrubber.addEventListener("mouseleave", () => {
    if (!state.outline.dragging) hideOutlineTooltip();
  });
}

function toggleSearchPanel() {
  const panel = $("#search-panel");
  const toggle = $("#search-toggle");
  if (!panel) return;
  closeOrganizeMenu();
  const open = panel.hidden;
  panel.hidden = !open;
  toggle?.setAttribute("aria-expanded", String(open));
  if (open) setTimeout(() => $("#thread-search")?.focus(), 40);
}

function toggleOrganizeMenu() {
  const menu = $("#organize-menu");
  const toggle = $("#organize-toggle");
  if (!menu) return;
  const open = menu.hidden;
  if (open) {
    const panel = $("#search-panel");
    if (panel) panel.hidden = true;
    $("#search-toggle")?.setAttribute("aria-expanded", "false");
  }
  menu.hidden = !open;
  toggle?.setAttribute("aria-expanded", String(open));
}

function closeOrganizeMenu() {
  const menu = $("#organize-menu");
  const toggle = $("#organize-toggle");
  if (menu) menu.hidden = true;
  toggle?.setAttribute("aria-expanded", "false");
}

function selectOrganizeMode(mode) {
  document.querySelectorAll(".organize-item").forEach((item) => {
    const active = item.dataset.sort === mode;
    item.classList.toggle("is-active", active);
    item.setAttribute("aria-checked", String(active));
  });
  closeOrganizeMenu();
  // Recency is the only sort backed by the list API; others are chrome-only.
  if (mode === "recency") loadThreads($("#thread-search")?.value || "").catch(() => {});
}

function openDrawer() {
  closeMetadata();
  expandRail();
  document.body.classList.add("drawer-open");
  const button = $("#sessions-button");
  if (button) button.setAttribute("aria-expanded", "true");
  syncOverlayScrim();
  setTimeout(() => $("#thread-search")?.focus(), 120);
}

function closeDrawer() {
  document.body.classList.remove("drawer-open");
  const button = $("#sessions-button");
  if (button) button.setAttribute("aria-expanded", "false");
  syncOverlayScrim();
}

function collapseRail() {
  if (mobileQuery.matches) {
    closeDrawer();
    return;
  }
  document.body.classList.add("rail-collapsed");
  const peek = $("#rail-peek");
  if (peek) peek.hidden = false;
  try { localStorage.setItem("claudex-rail", "collapsed"); } catch {}
}

function expandRail() {
  document.body.classList.remove("rail-collapsed");
  const peek = $("#rail-peek");
  if (peek) peek.hidden = true;
  try { localStorage.setItem("claudex-rail", "open"); } catch {}
}

function restoreRailPreference() {
  // Amp app keeps the sidebar open by default.
  if (mobileQuery.matches) return;
  try {
    if (localStorage.getItem("claudex-rail") === "collapsed") collapseRail();
    else expandRail();
  } catch {
    expandRail();
  }
}

function openMetadata() {
  closeDrawer();
  document.body.classList.add("metadata-open");
  const button = $("#metadata-button");
  const panel = $("#thread-metadata");
  if (button) button.setAttribute("aria-expanded", "true");
  if (panel) panel.setAttribute("aria-hidden", "false");
  syncOverlayScrim();
}

function closeMetadata() {
  document.body.classList.remove("metadata-open");
  const button = $("#metadata-button");
  const panel = $("#thread-metadata");
  if (button) button.setAttribute("aria-expanded", "false");
  if (panel) panel.setAttribute("aria-hidden", "true");
  syncOverlayScrim();
}

function closeOverlays() {
  closeOrganizeMenu();
  closeDrawer();
  closeMetadata();
}

function syncOverlayScrim() {
  drawerScrim.hidden = !document.body.classList.contains("drawer-open")
    && !document.body.classList.contains("metadata-open");
}

function schedulePoll() {
  stopPoll();
  if (document.hidden) return;
  const delay = Math.min(30_000, 12_000 * (1 + state.pollFailures));
  state.pollTimer = setInterval(() => {
    refreshAll(false).catch(() => {});
  }, delay);
}

function stopPoll() {
  clearInterval(state.pollTimer);
  state.pollTimer = null;
}

async function refreshAll(force = false) {
  if (state.refreshing) return;
  state.refreshing = true;
  setSync("syncing");
  try {
    await Promise.all([loadStats(), loadThreads($("#thread-search").value)]);
    if (state.currentID) await loadThread(state.currentID, force);
    state.pollFailures = 0;
    setSync("live");
    schedulePoll();
  } catch (error) {
    state.pollFailures += 1;
    setSync("offline");
    schedulePoll();
    throw error;
  } finally {
    state.refreshing = false;
  }
}

async function loadStats() {
  // Stats are no longer shown as a heavy rail chip; keep call for future Activity.
  try { await api("/api/stats"); } catch { /* ignore */ }
}

async function loadThreads(query = "") {
  const data = await api(`/api/threads?q=${encodeURIComponent(query)}`);
  state.threads = data.threads || [];
  renderThreads();
  if (!state.currentID) openFromHash();
}

function stableTitle(thread) {
  return stableTitleFromThread(thread);
}

function compactModel(model) {
  return compactModelName(model);
}

function renderThreads() {
  threadList.replaceChildren();
  if (!state.threads.length) {
    const empty = document.createElement("p");
    empty.className = "list-empty";
    empty.textContent = "No threads yet.";
    threadList.append(empty);
    return;
  }

  const groups = groupThreadsForRail(state.threads);
  const template = $("#thread-card-template");
  for (const group of groups) {
    if (group.label) {
      const heading = document.createElement("div");
      heading.className = "list-group-label";
      heading.textContent = group.label;
      threadList.append(heading);
    }
    for (const thread of group.threads) {
      const fragment = template.content.cloneNode(true);
      const card = fragment.querySelector(".thread-card");
      card.dataset.sessionId = thread.session_id;
      card.dataset.state = thread.state;
      card.dataset.bucket = group.key;
      card.classList.toggle("selected", thread.session_id === state.currentID);
      card.setAttribute("aria-pressed", String(thread.session_id === state.currentID));
      card.title = [
        stableTitle(thread),
        compactModel(thread.model),
        thread.effort,
        relativeTime(thread.updated_at),
      ].filter(Boolean).join(" · ");
      fragment.querySelector(".thread-title").textContent = stableTitle(thread);
      card.addEventListener("click", () => {
        selectThread(thread.session_id, "thread");
        if (mobileQuery.matches) closeDrawer();
      });
      threadList.append(fragment);
    }
  }
}

function openFromHash() {
  const match = location.hash.match(/^#\/thread\/([^/]+)(?:\/(usage))?$/);
  if (match) {
    let id;
    try { id = decodeURIComponent(match[1]); } catch { id = match[1]; }
    selectThread(id, match[2] === "usage" ? "usage" : "thread", false);
  } else if (state.threads.length) {
    selectThread(state.threads[0].session_id, "thread", false);
  }
}

function selectThread(id, view = "thread", updateHash = true) {
  state.currentID = id;
  state.view = view;
  if (updateHash) updateLocationHash();
  renderThreads();
  renderViewState();
  // Amp app keeps the sidebar open while reading.
  if (mobileQuery.matches) closeDrawer();
  else expandRail();
  loadThread(id, true).catch(() => setSync("offline"));
}

function setView(view) {
  if (!state.currentID || !["thread", "usage"].includes(view)) return;
  state.view = view;
  updateLocationHash();
  renderViewState();
  // Amp-style chrome: Usage lives in the floating action card, not a tab bar.
  const usageTab = $("#usage-tab");
  const threadTab = $("#thread-tab");
  if (usageTab) usageTab.hidden = view === "usage";
  if (threadTab) threadTab.hidden = view !== "usage";
  if (view === "thread") {
    requestAnimationFrame(() => {
      rebuildOutlineMarks();
      updateOutlineThumb();
    });
  } else {
    hideOutlineScrubber();
  }
}

function updateLocationHash() {
  if (!state.currentID) return;
  const suffix = state.view === "usage" ? "/usage" : "";
  const next = `#/thread/${encodeURIComponent(state.currentID)}${suffix}`;
  if (location.hash !== next) history.replaceState(null, "", next);
}

function renderViewState() {
  const usage = state.view === "usage";
  const threadView = $("#thread-view");
  const usageView = $("#usage-view");
  if (threadView) threadView.hidden = usage;
  if (usageView) usageView.hidden = !usage;
  const usageTab = $("#usage-tab");
  const threadTab = $("#thread-tab");
  if (usageTab) {
    usageTab.hidden = usage;
    usageTab.setAttribute("aria-selected", String(usage));
  }
  if (threadTab) {
    threadTab.hidden = !usage;
    threadTab.setAttribute("aria-selected", String(!usage));
  }
}

async function loadThread(id, showLoading = true) {
  if (showLoading) {
    setSync("syncing");
    showThreadLoading();
  }
  const token = ++state.renderToken;
  const graph = await api(`/api/threads/${encodeURIComponent(id)}?include_descendants=1`);
  if (state.currentID !== id || token !== state.renderToken) return;
  state.current = graph;
  // Yield so the browser can paint the loading shell before heavy DOM work.
  await new Promise((resolve) => requestAnimationFrame(() => resolve()));
  if (state.currentID !== id || token !== state.renderToken) return;
  renderDetail(graph);
  setSync("live");
}

function showThreadLoading() {
  emptyState.hidden = true;
  threadDetail.hidden = false;
  const list = $("#event-list");
  if (!list) return;
  list.replaceChildren();
  const loading = document.createElement("p");
  loading.className = "thread-loading";
  loading.textContent = "正在加载 Thread…";
  list.append(loading);
}

function renderDetail(graph) {
  const thread = recordValue(graph.thread);
  emptyState.hidden = true;
  threadDetail.hidden = false;

  const topbar = $("#main-topbar");
  if (topbar) topbar.hidden = false;
  const title = $("#detail-title");
  if (title) title.textContent = stableTitle(thread);

  // Amp web: no secondary meta strip competing with the sticky topbar.
  const metaLine = $("#detail-project");
  if (metaLine) {
    metaLine.hidden = true;
    metaLine.textContent = "";
  }

  const topUser = $("#topbar-user");
  const topEffort = $("#topbar-effort");
  const topSep = document.querySelector(".topbar-sep");
  const modelLabel = compactModel(thread.model);
  const effortLabel = String(thread.effort || "").trim();
  if (topUser) topUser.textContent = modelLabel || "local";
  if (topEffort) {
    topEffort.textContent = effortLabel;
    topEffort.hidden = !effortLabel;
  }
  if (topSep) topSep.hidden = !effortLabel;

  const composer = $("#composer-chrome");
  if (composer) composer.hidden = false;

  // Footer account chip stays stable (local workspace), not per-thread model.

  const base = `/api/threads/${encodeURIComponent(thread.session_id)}`;
  const jsonLink = $("#export-json-link");
  const mdLink = $("#export-markdown-link");
  const usageLink = $("#usage-export-link");
  if (jsonLink) jsonLink.href = `${base}/export.json?include_descendants=1`;
  if (mdLink) mdLink.href = `${base}/export.md?include_descendants=1`;
  if (usageLink) usageLink.href = `${base}/usage/export?include_descendants=1`;

  renderBreadcrumb(thread);
  renderMetadata(graph);
  renderParticipants(graph.participants || []);
  const strip = $("#participant-strip");
  if (strip) {
    strip.hidden = true;
    strip.replaceChildren();
  }
  renderTurns(graph);
  renderArtifacts(graph.artifacts || []);
  renderUsage(graph.usage || {});
  const policy = $("#redaction-policy");
  if (policy) policy.textContent = graph.redaction?.policy_version || "unknown";
  renderViewState();
}

function metaItem(text, technical = false) {
  const node = document.createElement("span");
  node.className = `meta-item${technical ? " technical" : ""}`;
  node.textContent = text;
  return node;
}

function statusItem(stateName) {
  const node = document.createElement("span");
  node.className = "meta-item status-inline";
  node.dataset.state = stateName || "active";
  node.textContent = stateLabel(stateName);
  return node;
}

function renderBreadcrumb(thread) {
  const nav = $("#thread-breadcrumb");
  if (!nav) return;
  nav.replaceChildren();
  if (!thread.parent_session_id) {
    nav.hidden = true;
    return;
  }
  nav.hidden = false;
  const parent = document.createElement("button");
  parent.type = "button";
  parent.textContent = `Parent ${shortID(thread.parent_session_id)}`;
  parent.addEventListener("click", () => selectThread(thread.parent_session_id));
  const current = document.createElement("span");
  current.textContent = shortID(thread.session_id);
  nav.append(parent, document.createTextNode(" / "), current);
}

function renderMetadata(graph) {
  const thread = recordValue(graph.thread);
  const totals = recordValue(graph.usage?.totals);
  const entries = [
    ["Session", shortID(thread.session_id), thread.session_id],
    ["Root", shortID(thread.root_session_id), thread.root_session_id],
    ["State", stateLabel(thread.state)],
    ["Started", formatDate(thread.started_at)],
    ["Updated", formatDate(thread.updated_at)],
    ["Events", number((graph.events || []).length)],
    ["Active", formatOptionalDuration(totals.active_duration_ms)],
    ["Worker compute", formatOptionalDuration(totals.worker_compute_duration_ms)],
  ];
  const list = $("#metadata-list");
  list.replaceChildren();
  for (const [label, value, title] of entries) {
    const term = document.createElement("dt");
    term.textContent = label;
    const detail = document.createElement("dd");
    detail.textContent = value || "Unknown";
    if (title) detail.title = title;
    list.append(term, detail);
  }
}

function renderParticipants(participants) {
  const list = $("#participant-list");
  list.replaceChildren();
  if (!participants.length) {
    const empty = document.createElement("p");
    empty.className = "metadata-empty";
    empty.textContent = "No participants recorded.";
    list.append(empty);
    return;
  }
  for (const participant of participants) {
    const item = document.createElement(participant.session_id ? "button" : "div");
    if (participant.session_id) {
      item.type = "button";
      item.addEventListener("click", () => selectThread(participant.session_id));
    }
    item.className = "participant-item";
    const heading = document.createElement("strong");
    heading.textContent = roleLabel(participant.role);
    const model = document.createElement("span");
    model.textContent = compactModel(participant.model) || "Model not reported";
    const detail = document.createElement("small");
    detail.textContent = [participant.effort, participant.session_id ? shortID(participant.session_id) : ""]
      .filter(Boolean).join(" · ") || "Metadata unknown";
    item.append(heading, model, detail);
    list.append(item);
  }
}

function renderParticipantStrip(participants) {
  const strip = $("#participant-strip");
  strip.replaceChildren();
  if (!participants.length) {
    strip.hidden = true;
    return;
  }
  strip.hidden = false;
  for (const participant of participants) {
    const chip = document.createElement("div");
    chip.className = "participant-chip";
    const role = document.createElement("strong");
    role.textContent = roleLabel(participant.role);
    const model = document.createElement("span");
    model.textContent = [compactModel(participant.model), participant.effort].filter(Boolean).join(" · ");
    chip.append(role, model);
    if (participant.session_id && participant.session_id !== state.currentID) {
      const open = document.createElement("button");
      open.type = "button";
      open.textContent = "Open";
      open.addEventListener("click", () => selectThread(participant.session_id));
      chip.append(open);
    }
    strip.append(chip);
  }
}

/* ── Turn model ── */

function deriveTurns(events) {
  const list = Array.isArray(events) ? events : [];
  const resultsByCall = new Map();
  const callIDs = new Set(list.filter((event) => event.type === "tool_call").map((event) => event.event_id || event.tool_use_id));
  const represented = new Set();

  for (const event of list) {
    if (event.type === "worker") {
      if (event.raw?.call_event_id) represented.add(event.raw.call_event_id);
      if (event.raw?.result_event_id) represented.add(event.raw.result_event_id);
    }
    if (event.type === "tool_result") {
      const parent = event.parent_event_id || event.tool_use_id;
      if (parent && !resultsByCall.has(parent)) resultsByCall.set(parent, event);
    }
  }

  const turns = [];
  let current = null;

  const flush = () => {
    if (!current) return;
    finalizeTurn(current);
    turns.push(current);
    current = null;
  };

  const ensureTurn = (event) => {
    if (current) return current;
    current = createTurn(null, event);
    return current;
  };

  for (const event of list) {
    if (represented.has(event.event_id)) continue;
    if (event.type === "tool_result" && callIDs.has(event.parent_event_id || event.tool_use_id)) continue;

    if (event.type === "message" && event.role === "user") {
      flush();
      current = createTurn(event, event);
      continue;
    }

    const turn = ensureTurn(event);
    if (event.type === "message" && event.role === "assistant") {
      turn.assistantMessages.push(event);
      continue;
    }
    if (event.type === "tool_call") {
      turn.activity.push({ kind: "tool", event, result: resultsByCall.get(event.event_id || event.tool_use_id) });
      continue;
    }
    if (event.type === "worker") {
      turn.activity.push({ kind: "execution", event });
      continue;
    }
    if (event.type === "message") {
      turn.activity.push({ kind: "message", event });
      continue;
    }
    turn.activity.push({ kind: "system", event });
  }
  flush();
  return turns;
}

function createTurn(userEvent, seedEvent) {
  return {
    id: userEvent?.event_id || seedEvent?.event_id || `turn-${Math.random().toString(36).slice(2, 8)}`,
    user: userEvent || null,
    assistantMessages: [],
    activity: [],
    startedAt: userEvent?.started_at || seedEvent?.started_at || null,
    endedAt: null,
    durationMs: null,
  };
}

function finalizeTurn(turn) {
  const timestamps = [];
  if (turn.user?.started_at) timestamps.push(Date.parse(turn.user.started_at));
  for (const message of turn.assistantMessages) {
    if (message.started_at) timestamps.push(Date.parse(message.started_at));
    if (message.ended_at) timestamps.push(Date.parse(message.ended_at));
  }
  for (const item of turn.activity) {
    const event = item.event;
    if (event?.started_at) timestamps.push(Date.parse(event.started_at));
    if (event?.ended_at) timestamps.push(Date.parse(event.ended_at));
    if (item.result?.started_at) timestamps.push(Date.parse(item.result.started_at));
    if (item.result?.ended_at) timestamps.push(Date.parse(item.result.ended_at));
    if (typeof event?.duration_ms === "number") timestamps.push(Date.parse(event.started_at || 0) + event.duration_ms);
  }
  const valid = timestamps.filter(Number.isFinite);
  if (valid.length) {
    const start = Math.min(...valid);
    const end = Math.max(...valid);
    turn.startedAt = turn.startedAt || new Date(start).toISOString();
    turn.endedAt = new Date(end).toISOString();
    turn.durationMs = Math.max(0, end - start);
  } else if (turn.user?.started_at && turn.assistantMessages.at(-1)?.started_at) {
    const start = Date.parse(turn.user.started_at);
    const end = Date.parse(turn.assistantMessages.at(-1).started_at);
    if (Number.isFinite(start) && Number.isFinite(end) && end >= start) {
      turn.durationMs = end - start;
      turn.endedAt = turn.assistantMessages.at(-1).started_at;
    }
  }

  // Process notes: non-final assistant messages that appear before tools, or short bridge text.
  turn.processNotes = [];
  turn.finalAnswers = [];
  if (!turn.assistantMessages.length) return;
  if (!turn.activity.length) {
    turn.finalAnswers = turn.assistantMessages.slice();
    return;
  }
  if (turn.assistantMessages.length === 1) {
    turn.finalAnswers = turn.assistantMessages.slice();
    return;
  }
  // Keep the last non-empty assistant message as the answer; earlier ones as process notes.
  turn.finalAnswers = [turn.assistantMessages.at(-1)];
  turn.processNotes = turn.assistantMessages.slice(0, -1);
}

function eventsForHumanView(graph) {
  const events = Array.isArray(graph.events) ? graph.events : [];
  const rootID = graph.thread?.session_id;
  const filtered = !rootID
    ? events.slice()
    : events.filter((event) => {
      // Main timeline: this thread's own messages/tools + worker cards.
      // Child chat text lives in the child thread.
      if (!event.session_id || event.session_id === rootID) return true;
      if (event.type === "worker") return true;
      return false;
    });

  return filtered.sort((left, right) => {
    const ta = Date.parse(left.started_at);
    const tb = Date.parse(right.started_at);
    const a = Number.isFinite(ta) ? ta : 0;
    const b = Number.isFinite(tb) ? tb : 0;
    if (a !== b) return a - b;
    // Same timestamp: user → tools → worker → assistant → rest.
    const rank = (event) => {
      if (event.type === "message" && event.role === "user") return 0;
      if (event.type === "tool_call") return 1;
      if (event.type === "tool_result") return 2;
      if (event.type === "worker") return 3;
      if (event.type === "message" && event.role === "assistant") return 4;
      return 5;
    };
    const diff = rank(left) - rank(right);
    if (diff) return diff;
    return String(left.event_id || "").localeCompare(String(right.event_id || ""));
  });
}

function renderTurns(graph) {
  const list = $("#event-list");
  list.replaceChildren();
  const events = eventsForHumanView(graph);
  if (!events.length) {
    list.append(inlineState("No canonical events recorded for this Thread."));
    hideOutlineScrubber();
    return;
  }

  const turns = deriveTurns(events);
  for (const turn of turns) list.append(renderTurn(turn, graph));
  // Outline positions need layout; measure on next frame.
  requestAnimationFrame(() => {
    rebuildOutlineMarks();
    updateOutlineThumb();
  });
}

/* ── Amp outline scrubber ── */

function hideOutlineScrubber() {
  const scrubber = $("#outline-scrubber");
  if (scrubber) scrubber.hidden = true;
  const marks = $("#outline-marks");
  if (marks) marks.replaceChildren();
  state.outline.marks = [];
  hideOutlineTooltip();
}

function hideOutlineTooltip() {
  const tip = $("#outline-tooltip");
  if (tip) {
    tip.hidden = true;
    tip.classList.remove("is-visible");
  }
  state.outline.hoverIndex = -1;
  document.querySelectorAll(".outline-mark.is-hover").forEach((n) => n.classList.remove("is-hover"));
  $("#outline-scrubber")?.classList.remove("is-hovering");
}

function outlineLabelForNode(node) {
  if (!node) return "Section";
  if (node.classList.contains("user-bubble")) {
    return clipOutlineText(node.querySelector(".user-text")?.textContent || "User message");
  }
  if (node.classList.contains("worked-for")) {
    return clipOutlineText(node.querySelector(".worked-label")?.textContent || "Show Work");
  }
  if (node.matches("h1, h2, h3, h4")) {
    return clipOutlineText(node.textContent || "Heading");
  }
  if (node.classList.contains("assistant-answer") || node.classList.contains("assistant-note")) {
    const heading = node.querySelector("h1, h2, h3, h4");
    if (heading) return clipOutlineText(heading.textContent || "Answer");
    return clipOutlineText(node.querySelector(".message-content")?.textContent || "Assistant");
  }
  if (node.classList.contains("tool-event") || node.classList.contains("execution-card")) {
    return clipOutlineText(node.querySelector(".tool-summary-copy")?.textContent || "Tool");
  }
  if (node.classList.contains("work-cluster")) {
    return clipOutlineText(node.querySelector("summary")?.textContent || "Work");
  }
  return clipOutlineText(node.textContent || "Section");
}

function clipOutlineText(value, max = 120) {
  const text = String(value || "").replace(/\s+/g, " ").trim();
  if (!text) return "Section";
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

function outlinePreviewForNode(node) {
  if (!node) return "";
  if (node.classList.contains("user-bubble")) {
    return clipOutlineText(node.querySelector(".user-text")?.textContent || "", 220);
  }
  if (node.matches("h1, h2, h3, h4")) {
    const parent = node.closest(".message-content");
    const next = node.nextElementSibling;
    const body = next?.textContent || parent?.textContent || "";
    return clipOutlineText(body, 220);
  }
  if (node.classList.contains("assistant-answer") || node.classList.contains("assistant-note")) {
    return clipOutlineText(node.querySelector(".message-content")?.textContent || "", 220);
  }
  if (node.classList.contains("tool-event") || node.classList.contains("execution-card")) {
    return clipOutlineText(node.querySelector(".tool-body, .tool-summary-copy")?.textContent || "", 220);
  }
  return clipOutlineText(node.textContent || "", 220);
}

function collectOutlineNodes() {
  const list = $("#event-list");
  if (!list) return [];
  const nodes = [];
  const seen = new Set();
  const push = (node, kind) => {
    if (!node || seen.has(node)) return;
    seen.add(node);
    if (!node.id) {
      node.id = `outline-${kind}-${nodes.length}-${Math.random().toString(36).slice(2, 7)}`;
    }
    nodes.push({ node, kind, label: outlineLabelForNode(node), preview: outlinePreviewForNode(node) });
  };

  for (const el of list.querySelectorAll(".user-bubble")) push(el, "user");
  for (const el of list.querySelectorAll("details.worked-for")) push(el, "work");
  for (const el of list.querySelectorAll(".assistant-answer .message-content h1, .assistant-answer .message-content h2, .assistant-answer .message-content h3")) {
    push(el, "heading");
  }
  for (const el of list.querySelectorAll(".assistant-answer")) {
    // Only add whole answer if it has no headings already collected as children.
    if (!el.querySelector("h1, h2, h3")) push(el, "answer");
  }
  for (const el of list.querySelectorAll(".assistant-note")) push(el, "note");
  for (const el of list.querySelectorAll(".tool-event, .execution-card, .work-cluster")) push(el, "tool");

  // Stable document order by vertical position after layout.
  return nodes;
}

function rebuildOutlineMarks() {
  const scrubber = $("#outline-scrubber");
  const marksRoot = $("#outline-marks");
  const scroller = $("#conversation-scroll");
  if (!scrubber || !marksRoot || !scroller) return;

  if (state.view !== "thread" || !state.currentID || scroller.scrollHeight <= scroller.clientHeight + 24) {
    hideOutlineScrubber();
    return;
  }

  const items = collectOutlineNodes();
  if (items.length < 2) {
    hideOutlineScrubber();
    return;
  }

  const scrollerRect = scroller.getBoundingClientRect();
  const contentH = Math.max(1, scroller.scrollHeight - 1);
  const majors = [];
  for (const item of items) {
    const rect = item.node.getBoundingClientRect();
    const absoluteTop = rect.top - scrollerRect.top + scroller.scrollTop;
    const ratio = Math.min(1, Math.max(0, absoluteTop / contentH));
    majors.push({ ...item, ratio, top: absoluteTop, major: true });
  }
  majors.sort((a, b) => a.ratio - b.ratio);
  const filtered = [];
  for (const mark of majors) {
    const prev = filtered[filtered.length - 1];
    if (prev && Math.abs(prev.ratio - mark.ratio) < 0.008) continue;
    filtered.push(mark);
  }
  state.outline.marks = filtered;

  // Amp density: fill track with micro ticks, then overlay interactive majors.
  const microCount = Math.min(120, Math.max(24, Math.round(scroller.scrollHeight / 900)));
  marksRoot.replaceChildren();
  for (let i = 0; i < microCount; i += 1) {
    const ratio = microCount === 1 ? 0 : i / (microCount - 1);
    // Skip micros that sit on a major (avoid double bars).
    if (filtered.some((m) => Math.abs(m.ratio - ratio) < 0.012)) continue;
    const micro = document.createElement("span");
    micro.className = "outline-mark is-micro";
    micro.style.top = `${ratio * 100}%`;
    micro.setAttribute("aria-hidden", "true");
    marksRoot.append(micro);
  }

  filtered.forEach((mark, index) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "outline-mark is-major";
    btn.dataset.kind = mark.kind;
    btn.dataset.index = String(index);
    btn.style.top = `${mark.ratio * 100}%`;
    btn.setAttribute("aria-label", mark.label);
    btn.addEventListener("mouseenter", () => showOutlineTooltip(index, btn));
    btn.addEventListener("mouseleave", () => {
      if (!state.outline.dragging) hideOutlineTooltip();
    });
    btn.addEventListener("focus", () => showOutlineTooltip(index, btn));
    btn.addEventListener("blur", hideOutlineTooltip);
    btn.addEventListener("click", (event) => {
      event.preventDefault();
      event.stopPropagation();
      jumpToOutlineMark(index);
    });
    marksRoot.append(btn);
  });

  scrubber.hidden = false;
  updateOutlineThumb();
}

function showOutlineTooltip(index, markEl) {
  const tip = $("#outline-tooltip");
  const scrubber = $("#outline-scrubber");
  const mark = state.outline.marks[index];
  if (!tip || !scrubber || !mark) return;
  state.outline.hoverIndex = index;
  document.querySelectorAll(".outline-mark.is-hover").forEach((n) => n.classList.remove("is-hover"));
  markEl?.classList.add("is-hover");

  tip.replaceChildren();
  const title = document.createElement("strong");
  title.textContent = mark.label;
  tip.append(title);
  if (mark.preview && mark.preview !== mark.label) {
    const body = document.createElement("p");
    body.textContent = mark.preview;
    tip.append(body);
  }
  const track = $("#outline-track");
  const trackRect = track?.getBoundingClientRect();
  const scrubRect = scrubber.getBoundingClientRect();
  if (trackRect) {
    const y = trackRect.top + mark.ratio * trackRect.height - scrubRect.top;
    tip.style.top = `${y}px`;
  }
  tip.hidden = false;
  // Force reflow so transition plays.
  void tip.offsetWidth;
  tip.classList.add("is-visible");
  scrubber.classList.add("is-hovering");
}

function jumpToOutlineMark(index) {
  const scroller = $("#conversation-scroll");
  const mark = state.outline.marks[index];
  if (!scroller || !mark?.node) return;
  const scrollerRect = scroller.getBoundingClientRect();
  const nodeRect = mark.node.getBoundingClientRect();
  const top = nodeRect.top - scrollerRect.top + scroller.scrollTop - 24;
  scroller.scrollTo({ top: Math.max(0, top), behavior: "smooth" });
  mark.node.classList.remove("outline-target-flash");
  void mark.node.offsetWidth;
  mark.node.classList.add("outline-target-flash");
  setTimeout(() => mark.node.classList.remove("outline-target-flash"), 800);
  updateOutlineThumb();
}

function updateOutlineThumb() {
  const scroller = $("#conversation-scroll");
  const thumb = $("#outline-thumb");
  const track = $("#outline-track");
  const scrubber = $("#outline-scrubber");
  if (!scroller || !thumb || !track || !scrubber || scrubber.hidden) return;

  const max = Math.max(0, scroller.scrollHeight - scroller.clientHeight);
  const trackH = track.clientHeight || 1;
  // Playhead follows reading position (upper third), as a short dash — not a scrollbar pill.
  const readY = scroller.scrollTop + scroller.clientHeight * 0.22;
  const contentH = Math.max(1, scroller.scrollHeight - 1);
  const playRatio = Math.min(1, Math.max(0, readY / contentH));
  thumb.style.height = "2.5px";
  thumb.style.top = `${playRatio * trackH}px`;

  // Light up nearest major tick; when close enough, hide dash so only tick is “current”.
  let best = -1;
  let bestDist = Infinity;
  state.outline.marks.forEach((mark, index) => {
    const dist = Math.abs(mark.top - readY);
    if (dist < bestDist) {
      bestDist = dist;
      best = index;
    }
  });
  const snapPx = Math.max(48, scroller.clientHeight * 0.08);
  const snapped = best >= 0 && bestDist <= snapPx;
  scrubber.classList.toggle("has-active-major", snapped);
  document.querySelectorAll(".outline-mark.is-major").forEach((node) => {
    const index = Number(node.dataset.index);
    node.classList.toggle("is-active", snapped && index === best);
  });
  if (!state.outline.dragging && state.outline.hoverIndex < 0) {
    scrubber.classList.remove("is-hovering");
  }
  void max; // scroll extent used for drag path elsewhere
}

function renderTurn(turn, graph) {
  const card = document.createElement("section");
  card.className = "turn-card";
  card.dataset.turnId = turn.id;

  if (turn.user) card.append(renderUserBubble(turn.user));

  // Amp: short bridge lines can sit above Show Work; longer mid-process text lives inside Work.
  const outsideNotes = [];
  const insideNotes = [];
  for (const note of turn.processNotes || []) {
    const text = String(note.content || note.summary || "");
    if (text.length <= 160 && !turn.activity.length) outsideNotes.push(note);
    else insideNotes.push(note);
  }
  for (const note of outsideNotes) card.append(renderAssistantNote(note));

  const activityItems = [];
  let sawTask = false;
  for (const note of insideNotes) {
    activityItems.push({ kind: "message", event: note });
  }
  for (const item of turn.activity) {
    if (item.kind === "tool" && shouldCollapseHousekeeping(item.event)) {
      if (sawTask) continue;
      sawTask = true;
    }
    activityItems.push(item);
  }
  // Keep work stream chronological: notes + tools interleaved by started_at.
  activityItems.sort((a, b) => {
    const ta = Date.parse(a.event?.started_at) || 0;
    const tb = Date.parse(b.event?.started_at) || 0;
    return ta - tb;
  });

  if (activityItems.length || turn.durationMs > 0) {
    card.append(renderWorkedFor(turn, activityItems, graph));
  }

  for (const answer of turn.finalAnswers || []) {
    card.append(renderAssistantAnswer(answer));
  }

  if (!turn.user && !(turn.finalAnswers || []).length && !activityItems.length) {
    card.append(inlineState("Empty turn"));
  }
  return card;
}

function shouldCollapseHousekeeping(event) {
  return housekeepingTools.has(shortTool(event.tool_name));
}

function renderActivityItem(item, graph) {
  if (item.kind === "tool") return renderTool(item.event, item.result);
  if (item.kind === "execution") return renderExecution(item.event, graph);
  if (item.kind === "message") return renderAssistantNote(item.event);
  return renderSystemEvent(item.event);
}

function renderAssistantNote(event) {
  const article = document.createElement("article");
  article.className = "assistant-note";
  article.id = eventAnchor(event.event_id);
  article.dataset.eventId = event.event_id || "";
  const content = document.createElement("div");
  content.className = "message-content";
  content.append(renderMarkdown(event.content || event.summary || ""));
  article.append(content);
  return article;
}

function renderUserBubble(event) {
  const article = document.createElement("article");
  article.className = "user-bubble";
  article.id = eventAnchor(event.event_id);
  article.dataset.eventId = event.event_id || "";
  const body = document.createElement("div");
  body.className = "user-text";
  body.textContent = event.content || event.summary || "";
  article.append(body);
  return article;
}

function renderWorkedFor(turn, activityItems, graph) {
  if (!activityItems.length && !(turn.durationMs > 0)) return document.createDocumentFragment();

  const details = document.createElement("details");
  details.className = "worked-for";
  const summary = document.createElement("summary");
  const label = document.createElement("span");
  label.className = "worked-label";
  const closedText = turn.durationMs > 0
    ? `Worked for ${formatDuration(turn.durationMs)}`
    : "Show Work";
  label.textContent = closedText;
  summary.append(label);

  const body = document.createElement("div");
  body.className = "worked-body";
  let mounted = false;
  const mount = () => {
    if (mounted) return;
    mounted = true;
    body.replaceChildren();
    if (!activityItems.length) {
      body.append(inlineState("No tool activity recorded for this turn."));
      return;
    }
    // Amp stream: grouped Ran/Explored/Edited rows, with notes interleaved.
    const clusters = clusterActivity(activityItems);
    let rendered = 0;
    for (const cluster of clusters) {
      if (rendered >= MAX_ACTIVITY_PREVIEW && cluster.kind !== "note") continue;
      body.append(renderWorkCluster(cluster, graph));
      rendered += cluster.items?.length || 1;
    }
  };
  details.addEventListener("toggle", () => {
    label.textContent = details.open ? "Hide Work" : closedText;
    if (details.open) mount();
  });
  details.append(summary, body);
  return details;
}

function toolCategory(item) {
  if (item.kind === "message") return "note";
  if (item.kind === "execution") {
    return item.event?.role === "agent" ? "agent" : "worker";
  }
  if (item.kind === "system") return "other";
  const tool = shortTool(item.event?.tool_name);
  const lower = tool.toLowerCase();
  if (tool === "Bash") return "bash";
  if (tool === "Read") return "read";
  if (tool === "Grep" || tool === "Glob" || lower.includes("explore") || lower.includes("search")) return "search";
  if (tool === "Edit" || tool === "Write") return "edit";
  if (housekeepingTools.has(tool) || lower.startsWith("task")) return "task";
  if (lower.includes("worker") || lower.includes("start_worker") || lower.includes("resume_worker")) return "worker";
  return "other";
}

function clusterActivity(activityItems) {
  const clusters = [];
  for (const item of activityItems) {
    const category = toolCategory(item);
    if (category === "note") {
      clusters.push({ kind: "note", items: [item] });
      continue;
    }
    // Skip pure task-management noise entirely from the Amp work stream.
    if (category === "task") continue;
    // Explore groups read+search together like Amp.
    const groupKey = category === "read" || category === "search" ? "explore" : category;
    const last = clusters.at(-1);
    if (last && last.kind === groupKey && groupKey !== "worker" && groupKey !== "agent") {
      last.items.push(item);
      continue;
    }
    clusters.push({ kind: groupKey, items: [item] });
  }
  return clusters;
}

function renderWorkCluster(cluster, graph) {
  if (cluster.kind === "note") {
    const note = document.createElement("div");
    note.className = "work-note";
    const event = cluster.items[0].event;
    // Amp process narration: plain prose in the work stream.
    note.append(renderAssistantNote(event));
    return note;
  }

  // Single-item groups still show as one Amp row (and expand for detail).
  if (cluster.kind === "worker") {
    const fragment = document.createDocumentFragment();
    for (const item of cluster.items) fragment.append(renderActivityItem(item, graph));
    return fragment;
  }

  if (cluster.items.length === 1 && cluster.kind !== "bash" && cluster.kind !== "explore" && cluster.kind !== "edit") {
    return renderActivityItem(cluster.items[0], graph);
  }

  // Amp always groups multi-step runs, including multi-command and multi-file.
  // For a single bash command, still use the "$ cmd" leaf row.
  if (cluster.items.length === 1) {
    return renderActivityItem(cluster.items[0], graph);
  }

  const details = document.createElement("details");
  details.className = "work-cluster";
  const summary = document.createElement("summary");
  const label = document.createElement("span");
  label.className = "work-cluster-label";
  label.textContent = clusterLabel(cluster);
  summary.append(label);
  const body = document.createElement("div");
  body.className = "work-cluster-body";
  let mounted = false;
  details.addEventListener("toggle", () => {
    if (!details.open || mounted) return;
    mounted = true;
    for (const item of cluster.items) body.append(renderActivityItem(item, graph));
  });
  details.append(summary, body);
  return details;
}

function clusterLabel(cluster) {
  const items = cluster.items || [];
  if (cluster.kind === "bash") {
    return items.length === 1 ? "Ran 1 command" : `Ran ${items.length} commands`;
  }
  if (cluster.kind === "explore") {
    let files = 0;
    let searches = 0;
    for (const item of items) {
      const tool = shortTool(item.event?.tool_name);
      if (tool === "Read") files += 1;
      else searches += 1;
    }
    const bits = [];
    if (files) bits.push(files === 1 ? "1 file" : `${files} files`);
    if (searches) bits.push(searches === 1 ? "1 search" : `${searches} searches`);
    return `Explored ${bits.join(", ") || `${items.length} items`}`;
  }
  if (cluster.kind === "edit") {
    if (items.length === 1) {
      const row = ampToolRow(items[0].event, items[0].result);
      return `Edited ${row.detail}`.trim();
    }
    return `Edited ${items.length} files`;
  }
  if (cluster.kind === "task") return items.length === 1 ? "Task update" : `Task updates ×${items.length}`;
  return items.length === 1 ? "1 step" : `${items.length} steps`;
}

function renderAssistantAnswer(event) {
  const article = document.createElement("article");
  article.className = "assistant-answer";
  article.id = eventAnchor(event.event_id);
  article.dataset.eventId = event.event_id || "";

  const toolbar = document.createElement("div");
  toolbar.className = "message-toolbar";
  const copy = document.createElement("button");
  copy.type = "button";
  copy.className = "copy-message";
  copy.textContent = "Copy";
  copy.addEventListener("click", async () => {
    await copyText(event.content || event.summary || "");
    copy.textContent = "Copied";
    setTimeout(() => { copy.textContent = "Copy"; }, 1200);
  });
  toolbar.append(copy);

  const content = document.createElement("div");
  content.className = "message-content";
  content.append(renderMarkdown(event.content || event.summary || ""));
  article.append(toolbar, content);
  return article;
}

function renderProcessNote(event) {
  return renderAssistantNote(event);
}

function renderTool(event, result) {
  const details = document.createElement("details");
  details.className = `tool-event activity-item${result?.status === "failed" || event.status === "failed" ? " failed" : ""}`;
  details.id = eventAnchor(event.event_id);
  details.dataset.eventId = event.event_id || "";
  const tool = shortTool(event.tool_name);
  if (housekeepingTools.has(tool)) details.dataset.toolGroup = "task";

  const summary = document.createElement("summary");
  summary.className = "tool-summary";
  const heading = document.createElement("span");
  heading.className = "tool-summary-copy";
  const row = ampToolRow(event, result);
  const name = document.createElement("strong");
  name.textContent = row.prefix;
  const description = document.createElement("span");
  description.textContent = row.detail;
  description.title = row.detail;
  heading.append(name);
  if (row.detail) heading.append(description);
  summary.append(heading);

  const body = document.createElement("div");
  body.className = "tool-body";
  body.append(...renderToolBody(event, result));
  details.append(summary, body);
  return details;
}

function ampToolRow(event, result) {
  const input = recordValue(recordValue(event.raw).input);
  const tool = shortTool(event.tool_name);
  const lower = tool.toLowerCase();
  if (tool === "Bash") {
    return { prefix: "$", detail: firstLine(input.command || event.summary || "command") };
  }
  if (tool === "Read") {
    return { prefix: "Read", detail: shortPath(input.file_path || input.path || event.summary || "file") };
  }
  if (tool === "Write") {
    return { prefix: "Wrote", detail: shortPath(input.file_path || input.path || event.summary || "file") };
  }
  if (tool === "Edit") {
    const path = shortPath(input.file_path || input.path || event.summary || "file");
    const oldText = String(firstDefined(input.old_string, input.oldString, "") || "");
    const newText = String(firstDefined(input.new_string, input.newString, "") || "");
    const plus = newText ? newText.split(/\r?\n/).length : 0;
    const minus = oldText ? oldText.split(/\r?\n/).length : 0;
    const delta = [plus ? `+${plus}` : "", minus ? `-${minus}` : ""].filter(Boolean).join(" ");
    return { prefix: "Edited", detail: [path, delta].filter(Boolean).join("  ") };
  }
  if (tool === "Grep") {
    return { prefix: "Searched", detail: input.pattern || event.summary || "pattern" };
  }
  if (tool === "Glob") {
    return { prefix: "Glob", detail: input.pattern || input.glob || event.summary || "pattern" };
  }
  if (lower.includes("explore")) {
    return { prefix: "Explored", detail: firstLine(input.objective || input.question || event.summary || "repository") };
  }
  if (housekeepingTools.has(tool) || lower.startsWith("task")) {
    return { prefix: "Task", detail: "progress" };
  }
  return { prefix: tool, detail: firstLine(event.summary || result?.summary || "") };
}

function shortPath(path) {
  const value = String(path || "");
  if (value.length <= 64) return value;
  const parts = value.split("/");
  if (parts.length < 3) return `${value.slice(0, 28)}…${value.slice(-28)}`;
  return `…/${parts.slice(-2).join("/")}`;
}

function humanToolSummary(event, result) {
  return ampToolRow(event, result).detail;
}

function renderToolBody(event, result) {
  const nodes = [];
  const input = recordValue(recordValue(event.raw).input);
  const tool = shortTool(event.tool_name);
  const output = result?.content || recordValue(result?.raw).result;

  if (tool === "Bash") {
    nodes.push(fieldBlock("Command", input.command || event.summary || ""));
    if (input.cwd || input.working_directory) nodes.push(fieldBlock("Working directory", input.cwd || input.working_directory));
    const exit = firstDefined(input.exit_code, recordValue(result?.raw).exit_code, result?.raw?.exitCode);
    if (exit !== undefined) nodes.push(fieldBlock("Exit code", String(exit)));
    nodes.push(fieldBlock("Output", stringifyOutput(output) || "No visible output."));
    return nodes;
  }

  if (tool === "Read") {
    nodes.push(fieldBlock("Path", input.file_path || input.path || "Unknown path"));
    if (input.offset != null) nodes.push(fieldBlock("Offset", String(input.offset)));
    if (input.limit != null) nodes.push(fieldBlock("Limit", String(input.limit)));
    nodes.push(fieldBlock("Result", compactOutput(output, 4000)));
    return nodes;
  }

  if (tool === "Grep" || tool === "Glob") {
    if (input.pattern || input.glob) nodes.push(fieldBlock("Pattern", input.pattern || input.glob));
    if (input.path || input.glob) nodes.push(fieldBlock("Path", input.path || input.glob_path || "."));
    nodes.push(fieldBlock("Result", compactOutput(output, 4000)));
    return nodes;
  }

  if (tool === "Edit" || tool === "Write") {
    const path = input.file_path || input.path || "Unknown path";
    nodes.push(fieldBlock("Path", path));
    const diff = buildDiffPreview(input, tool);
    if (diff) nodes.push(diff);
    else nodes.push(fieldBlock("Result", compactOutput(output, 4000) || "No visible diff."));
    return nodes;
  }

  if (housekeepingTools.has(tool)) {
    nodes.push(fieldBlock("Update", event.summary || stringifyOutput(input) || "Task management"));
    if (output) nodes.push(fieldBlock("Result", compactOutput(output, 1500)));
    return nodes;
  }

  nodes.push(fieldBlock("Input", stringifyOutput(input) || event.summary || "—"));
  nodes.push(fieldBlock("Result", compactOutput(output, 4000) || "No visible result recorded."));

  const raw = document.createElement("details");
  raw.className = "code-panel";
  const rawSummary = document.createElement("summary");
  rawSummary.textContent = "Show raw JSON";
  const rawPre = document.createElement("pre");
  rawPre.textContent = JSON.stringify({ input: event.raw, result: result?.raw ?? result?.content ?? null }, null, 2);
  raw.append(rawSummary, rawPre);
  nodes.push(raw);
  return nodes;
}

function buildDiffPreview(input, tool) {
  const section = document.createElement("section");
  section.className = "tool-field diff-block";
  const heading = document.createElement("h4");
  heading.textContent = tool === "Write" ? "Write" : "Diff";
  section.append(heading);

  if (tool === "Write" && typeof input.content === "string") {
    const lines = input.content.split(/\r?\n/);
    const meta = document.createElement("div");
    meta.className = "diff-meta";
    meta.innerHTML = `<span class="plus">+${lines.length}</span>`;
    const pre = document.createElement("pre");
    pre.textContent = lines.slice(0, 80).map((line) => `+ ${line}`).join("\n")
      + (lines.length > 80 ? `\n… ${lines.length - 80} more lines` : "");
    section.append(meta, pre);
    return section;
  }

  const oldText = firstDefined(input.old_string, input.oldString, input.old_content, "");
  const newText = firstDefined(input.new_string, input.newString, input.new_content, "");
  if (typeof oldText === "string" && typeof newText === "string" && (oldText || newText)) {
    const oldLines = String(oldText).split(/\r?\n/);
    const newLines = String(newText).split(/\r?\n/);
    const meta = document.createElement("div");
    meta.className = "diff-meta";
    meta.innerHTML = `<span class="plus">+${newLines.length}</span><span class="minus">-${oldLines.length}</span>`;
    const pre = document.createElement("pre");
    const removed = oldLines.slice(0, 40).map((line) => `- ${line}`);
    const added = newLines.slice(0, 40).map((line) => `+ ${line}`);
    let text = [...removed, ...added].join("\n");
    if (oldLines.length > 40 || newLines.length > 40) text += "\n… truncated";
    pre.textContent = text || "(empty diff)";
    section.append(meta, pre);
    return section;
  }
  return null;
}

function fieldBlock(label, value) {
  const section = document.createElement("section");
  section.className = "tool-field";
  const heading = document.createElement("h4");
  heading.textContent = label;
  const pre = document.createElement("pre");
  pre.textContent = typeof value === "string" ? value : stringifyOutput(value);
  section.append(heading, pre);
  return section;
}

function renderExecution(event, graph) {
  const details = document.createElement("details");
  const failed = ["failed", "error", "blocked", "cancelled"].includes(String(event.status || "").toLowerCase());
  details.className = `execution-card tool-event${failed ? " failed" : ""}`;
  details.id = eventAnchor(event.event_id);
  details.dataset.eventId = event.event_id || "";

  const kind = event.role === "agent" ? "Agent" : "Worker";
  const objective = firstLine(event.raw?.objective || event.summary || `${kind} invocation`);
  const summary = document.createElement("summary");
  // Amp-like system row: not mono for agents, muted label + detail
  summary.className = "tool-summary execution-summary";
  const heading = document.createElement("span");
  heading.className = "tool-summary-copy execution-copy";
  const name = document.createElement("strong");
  name.textContent = kind;
  const description = document.createElement("span");
  const model = compactModel(event.raw?.resolved_model || event.model || event.raw?.requested_model);
  description.textContent = [objective, model, event.effort || event.raw?.resolved_effort].filter(Boolean).join(" · ");
  description.title = description.textContent;
  heading.append(name, description);
  summary.append(heading);
  details.append(summary);

  const body = document.createElement("div");
  body.className = "tool-body";
  if (event.content && event.content !== "[INTERNAL METADATA REDACTED]") {
    const content = document.createElement("div");
    content.className = "execution-result";
    content.append(renderMarkdown(event.content));
    body.append(content);
  }
  const childID = childForWorker(graph.relationships || [], event.event_id);
  if (childID) {
    const child = document.createElement("button");
    child.type = "button";
    child.className = "inline-link";
    child.textContent = `Open child ${shortID(childID)}`;
    child.addEventListener("click", () => selectThread(childID));
    body.append(child);
  }
  if (body.childNodes.length) details.append(body);
  return details;
}

function renderSystemEvent(event) {
  const article = document.createElement("article");
  const failed = event.type === "error" || ["failed", "error"].includes(String(event.status || "").toLowerCase());
  article.className = `activity-item${failed ? " failed" : ""}`;
  article.id = eventAnchor(event.event_id);
  const header = document.createElement("header");
  const label = document.createElement("strong");
  label.textContent = event.type === "compact" ? "Compact" : event.type || "Event";
  const time = document.createElement("time");
  time.textContent = formatTime(event.started_at);
  header.append(label, time);
  const content = document.createElement("p");
  content.className = "execution-result";
  content.textContent = event.content || event.summary || "Recorded event";
  article.append(header, content);
  return article;
}

function eventMeta(parts) {
  const meta = document.createElement("p");
  meta.className = "tool-summary-meta";
  meta.textContent = parts.filter(Boolean).join(" · ");
  return meta;
}

function appendSessionLink(container, event) {
  const root = state.current?.thread?.session_id;
  if (!event.session_id || event.session_id === root) return;
  const link = document.createElement("button");
  link.type = "button";
  link.className = "session-reference";
  link.textContent = `Session ${shortID(event.session_id)}`;
  link.addEventListener("click", () => selectThread(event.session_id));
  container.append(link);
}

/* ── Markdown ── */

function renderMarkdown(source) {
  const fragment = document.createDocumentFragment();
  const text = String(source || "").replace(/\r\n/g, "\n");
  if (!text.trim()) {
    fragment.append(document.createTextNode(""));
    return fragment;
  }

  const lines = text.split("\n");
  let index = 0;
  let paragraph = [];

  const flushParagraph = () => {
    if (!paragraph.length) return;
    const p = document.createElement("p");
    p.append(renderInline(paragraph.join("\n")));
    fragment.append(p);
    paragraph = [];
  };

  while (index < lines.length) {
    const line = lines[index];

    if (/^\s*```/.test(line)) {
      flushParagraph();
      const language = line.trim().slice(3).trim();
      index += 1;
      const codeLines = [];
      while (index < lines.length && !/^\s*```/.test(lines[index])) {
        codeLines.push(lines[index]);
        index += 1;
      }
      if (index < lines.length) index += 1;
      fragment.append(renderCodeBlock(codeLines.join("\n"), language));
      continue;
    }

    if (/^\s*\|.+\|\s*$/.test(line) && index + 1 < lines.length && /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)+\|?\s*$/.test(lines[index + 1])) {
      flushParagraph();
      const tableLines = [];
      while (index < lines.length && /^\s*\|/.test(lines[index])) {
        tableLines.push(lines[index]);
        index += 1;
      }
      fragment.append(renderTable(tableLines));
      continue;
    }

    if (/^\s*([-*_])\1{2,}\s*$/.test(line)) {
      flushParagraph();
      fragment.append(document.createElement("hr"));
      index += 1;
      continue;
    }

    const heading = line.match(/^\s*(#{1,4})\s+(.+)$/);
    if (heading) {
      flushParagraph();
      const level = heading[1].length;
      const node = document.createElement(`h${level}`);
      const headingText = heading[2];
      node.id = `mdh-${level}-${headingText.slice(0, 40).replace(/[^a-zA-Z0-9\u4e00-\u9fff]+/g, "-").replace(/^-|-$/g, "").toLowerCase() || "h"}-${index}`;
      node.append(renderInline(headingText));
      fragment.append(node);
      index += 1;
      continue;
    }

    const quote = line.match(/^\s*>\s?(.*)$/);
    if (quote) {
      flushParagraph();
      const block = document.createElement("blockquote");
      const quotes = [];
      while (index < lines.length) {
        const match = lines[index].match(/^\s*>\s?(.*)$/);
        if (!match) break;
        quotes.push(match[1]);
        index += 1;
      }
      block.append(renderInline(quotes.join("\n")));
      fragment.append(block);
      continue;
    }

    const unordered = line.match(/^\s*[-*+]\s+(.+)$/);
    if (unordered) {
      flushParagraph();
      const list = document.createElement("ul");
      while (index < lines.length) {
        const match = lines[index].match(/^\s*[-*+]\s+(.+)$/);
        if (!match) break;
        const li = document.createElement("li");
        li.append(renderInline(match[1]));
        list.append(li);
        index += 1;
      }
      fragment.append(list);
      continue;
    }

    const ordered = line.match(/^\s*\d+\.\s+(.+)$/);
    if (ordered) {
      flushParagraph();
      const list = document.createElement("ol");
      while (index < lines.length) {
        const match = lines[index].match(/^\s*\d+\.\s+(.+)$/);
        if (!match) break;
        const li = document.createElement("li");
        li.append(renderInline(match[1]));
        list.append(li);
        index += 1;
      }
      fragment.append(list);
      continue;
    }

    if (!line.trim()) {
      flushParagraph();
      index += 1;
      continue;
    }

    paragraph.push(line);
    index += 1;
  }
  flushParagraph();
  return fragment;
}

function renderCodeBlock(code, language) {
  const wrap = document.createElement("div");
  wrap.className = "code-block";
  const copy = document.createElement("button");
  copy.type = "button";
  copy.className = "copy-code";
  copy.textContent = "Copy";
  copy.addEventListener("click", async () => {
    await copyText(code);
    copy.textContent = "Copied";
    setTimeout(() => { copy.textContent = "Copy"; }, 1200);
  });
  const pre = document.createElement("pre");
  if (language) pre.dataset.language = language;
  const node = document.createElement("code");
  node.textContent = code;
  pre.append(node);
  wrap.append(copy, pre);
  return wrap;
}

function renderTable(lines) {
  const table = document.createElement("table");
  const rows = lines
    .filter((line, index) => index !== 1)
    .map((line) => line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((cell) => cell.trim()));
  if (!rows.length) return table;
  const thead = document.createElement("thead");
  const headRow = document.createElement("tr");
  for (const cell of rows[0]) {
    const th = document.createElement("th");
    th.append(renderInline(cell));
    headRow.append(th);
  }
  thead.append(headRow);
  const tbody = document.createElement("tbody");
  for (const row of rows.slice(1)) {
    const tr = document.createElement("tr");
    for (const cell of row) {
      const td = document.createElement("td");
      td.append(renderInline(cell));
      tr.append(td);
    }
    tbody.append(tr);
  }
  table.append(thead, tbody);
  return table;
}

function renderInline(text) {
  const fragment = document.createDocumentFragment();
  const pattern = /(`[^`]+`|\*\*[^*]+\*\*|__[^_]+__|\*[^*]+\*|_[^_]+_|\[[^\]]+\]\([^)]+\))/g;
  let last = 0;
  let match;
  const source = String(text || "");
  while ((match = pattern.exec(source))) {
    if (match.index > last) fragment.append(document.createTextNode(source.slice(last, match.index)));
    const token = match[0];
    if (token.startsWith("`")) {
      const code = document.createElement("code");
      code.textContent = token.slice(1, -1);
      fragment.append(code);
    } else if (token.startsWith("**") || token.startsWith("__")) {
      const strong = document.createElement("strong");
      strong.textContent = token.slice(2, -2);
      fragment.append(strong);
    } else if (token.startsWith("*") || token.startsWith("_")) {
      const em = document.createElement("em");
      em.textContent = token.slice(1, -1);
      fragment.append(em);
    } else if (token.startsWith("[")) {
      const linkMatch = token.match(/^\[([^\]]+)\]\(([^)]+)\)$/);
      if (linkMatch && isSafeURL(linkMatch[2])) {
        const anchor = document.createElement("a");
        anchor.href = linkMatch[2];
        anchor.target = "_blank";
        anchor.rel = "noreferrer noopener";
        anchor.textContent = linkMatch[1];
        fragment.append(anchor);
      } else {
        fragment.append(document.createTextNode(token));
      }
    }
    last = match.index + token.length;
  }
  if (last < source.length) fragment.append(document.createTextNode(source.slice(last)));
  return fragment;
}

function isSafeURL(value) {
  try {
    const url = new URL(value, location.origin);
    return url.protocol === "http:" || url.protocol === "https:" || url.protocol === "mailto:";
  } catch {
    return false;
  }
}

/* ── Artifacts / Usage ── */

function renderArtifacts(artifacts) {
  const section = $("#artifacts-section");
  const list = $("#artifact-list");
  list.replaceChildren();
  section.hidden = artifacts.length === 0;
  for (const artifact of artifacts) {
    const item = document.createElement("div");
    item.className = "artifact-item";
    const action = document.createElement("span");
    action.textContent = artifact.action || "artifact";
    const path = document.createElement("code");
    path.textContent = artifact.path || artifact.name || "Recorded artifact";
    const status = document.createElement("small");
    status.textContent = stateLabel(artifact.status);
    item.append(action, path, status);
    if (artifact.event_id) {
      item.tabIndex = 0;
      item.setAttribute("role", "button");
      item.addEventListener("click", () => focusEvent(artifact.event_id));
      item.addEventListener("keydown", (event) => {
        if (event.key === "Enter" || event.key === " ") focusEvent(artifact.event_id);
      });
    }
    list.append(item);
  }
}

function renderUsage(usage) {
  const container = $("#usage-content");
  container.replaceChildren();
  const totals = recordValue(usage.totals);
  $("#usage-updated").textContent = totals.updated_at
    ? `Updated ${formatDate(totals.updated_at)} · root and descendants`
    : "No numeric usage record is available; unknown values remain unknown.";

  const summary = document.createElement("dl");
  summary.className = "usage-summary";
  for (const [label, value] of [
    ["Total tokens", usageNumber(totals.total_tokens, totals.updated_at)],
    ["Requests", usageNumber(totals.requests, totals.updated_at)],
    ["Active duration", formatOptionalDuration(totals.active_duration_ms)],
    ["Worker compute", formatOptionalDuration(totals.worker_compute_duration_ms)],
    ["Cost", formatCost(totals)],
  ]) {
    const item = document.createElement("div");
    const term = document.createElement("dt");
    term.textContent = label;
    const detail = document.createElement("dd");
    detail.textContent = value;
    item.append(term, detail);
    summary.append(item);
  }
  container.append(summary);

  for (const [title, rows, dimension] of [
    ["Models", usage.models || [], "model"],
    ["Roles", usage.roles || [], "role"],
    ["Sessions", usage.sessions || [], "session_id"],
  ]) {
    container.append(usageTable(title, rows, dimension));
  }
}

function usageTable(title, rows, dimension) {
  const section = document.createElement("section");
  section.className = "usage-section";
  const heading = document.createElement("h3");
  heading.textContent = title;
  section.append(heading);
  if (!rows.length) {
    section.append(inlineState(`No ${title.toLowerCase()} usage recorded.`));
    return section;
  }
  const wrapper = document.createElement("div");
  wrapper.className = "usage-table-wrap";
  const table = document.createElement("table");
  table.className = "usage-table";
  const thead = document.createElement("thead");
  const headRow = document.createElement("tr");
  for (const label of [title.slice(0, -1), "Req", "Input", "Cache 5m", "Cache 1h", "Cache read", "Output", "Total", "Cost"]) {
    const cell = document.createElement("th");
    cell.scope = "col";
    cell.textContent = label;
    headRow.append(cell);
  }
  thead.append(headRow);
  const tbody = document.createElement("tbody");
  for (const row of rows) {
    const tr = document.createElement("tr");
    const key = row[dimension] || "Unknown";
    const first = document.createElement("td");
    const link = document.createElement("button");
    link.type = "button";
    link.className = "usage-link";
    link.textContent = dimension === "role" ? roleLabel(key) : dimension === "session_id" ? shortID(key) : key;
    link.title = key;
    link.addEventListener("click", () => focusUsageDimension(dimension, key));
    first.append(link);
    tr.append(first);
    for (const value of [
      row.requests, row.input_tokens, row.cache_write_5m_tokens, row.cache_write_1h_tokens,
      row.cache_read_tokens, row.output_tokens, row.total_tokens,
    ]) {
      const cell = document.createElement("td");
      cell.textContent = compactNumber(value);
      cell.title = number(value);
      tr.append(cell);
    }
    const cost = document.createElement("td");
    cost.textContent = formatCost(row);
    cost.className = row.cost_status === "reported" ? "" : "pending-cost";
    tr.append(cost);
    tbody.append(tr);
  }
  table.append(thead, tbody);
  wrapper.append(table);
  section.append(wrapper);
  return section;
}

function focusUsageDimension(dimension, value) {
  const events = state.current?.events || [];
  const target = events.find((event) => {
    if (dimension === "model") return event.model === value || event.raw?.resolved_model === value;
    if (dimension === "role") return event.role === value;
    return event.session_id === value;
  });
  if (!target) return;
  setView("thread");
  requestAnimationFrame(() => focusEvent(target.event_id));
}

function focusEvent(eventID) {
  const node = document.getElementById(eventAnchor(eventID));
  if (!node) return;
  const worked = node.closest("details.worked-for");
  if (worked) worked.open = true;
  if (node instanceof HTMLDetailsElement) node.open = true;
  node.scrollIntoView({ behavior: "smooth", block: "center" });
  node.tabIndex = -1;
  node.focus({ preventScroll: true });
  node.classList.add("event-highlight");
  setTimeout(() => node.classList.remove("event-highlight"), 1600);
}

function childForWorker(relationships, eventID) {
  return relationships.find((item) => item.type === "worker_session" && item.from === eventID)?.to || "";
}

function usageForSession(usage, sessionID) {
  return (usage?.sessions || []).find((item) => item.session_id === sessionID) || null;
}

async function archiveCurrent() {
  if (!state.currentID || !confirm("归档这个 Root Thread 及其子 Thread？")) return;
  await api(`/api/threads/${encodeURIComponent(state.currentID)}/archive`, { method: "POST" });
  state.currentID = null;
  state.current = null;
  history.replaceState(null, "", location.pathname + location.search);
  threadDetail.hidden = true;
  emptyState.hidden = false;
  await refreshAll(true);
}

async function copyThreadMarkdown() {
  if (!state.currentID) return;
  const response = await fetch(`/api/threads/${encodeURIComponent(state.currentID)}/export.md?include_descendants=1`);
  if (!response.ok) throw new Error("export failed");
  const text = await response.text();
  await copyText(text);
  const button = $("#copy-thread-md");
  const previous = button.textContent;
  button.textContent = "Copied";
  setTimeout(() => { button.textContent = previous; }, 1200);
}

function inlineState(text) {
  const node = document.createElement("p");
  node.className = "inline-state";
  node.textContent = text;
  return node;
}

function roleLabel(role) {
  return ({
    supervisor: "Supervisor",
    assistant: "Assistant",
    agent: "Agent",
    worker: "Worker",
    user: "User",
    external_search: "External search",
    url_digest: "URL digest",
    repo_explore: "Repository explore",
  })[role] || role || "Participant";
}

function stateLabel(value) {
  return ({
    active: "Running",
    running: "Running",
    completed: "Completed",
    closed: "Completed",
    failed: "Failed",
    error: "Failed",
    blocked: "Blocked",
    cancelled: "Cancelled",
    archived: "Archived",
    unknown: "Unknown",
  })[value] || value || "Unknown";
}

function shortTool(name) {
  return String(name || "Tool")
    .replace(/^functions\./, "")
    .replace(/^mcp__[^_]+__/, "")
    .replace(/^mcp__/, "")
    .replace(/^tool_/, "");
}

function eventAnchor(id) {
  return `event-${String(id || "unknown").replace(/[^a-zA-Z0-9_-]/g, "-")}`;
}

function recordValue(value) {
  return value && typeof value === "object" && !Array.isArray(value) ? value : {};
}

function shortID(id) {
  const value = String(id || "");
  return value.length > 12 ? `${value.slice(0, 8)}…${value.slice(-4)}` : value;
}

function number(value) {
  return Number(value || 0).toLocaleString("en-US");
}

function compactNumber(value) {
  const amount = Number(value || 0);
  if (Math.abs(amount) < 1000) return number(amount);
  return new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 2 }).format(amount);
}

function usageNumber(value, observedAt) {
  if (!observedAt && (value === null || value === undefined || Number(value) === 0)) return "Unknown";
  return number(value);
}

function formatCost(value) {
  if ((value.cost_status === "reported" || value.cost_status === "partially_reported") && typeof value.reported_cost_usd === "number") {
    const suffix = value.cost_status === "partially_reported" ? " partial" : "";
    return `$${value.reported_cost_usd.toFixed(4)}${suffix}`;
  }
  return value.cost_status === "unpriced" ? "Unpriced" : "Unknown";
}

function formatDate(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "Unknown time";
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
  }).format(date);
}

function formatTime(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(date);
}

function relativeTime(value) {
  const seconds = Math.round((Date.parse(value) - Date.now()) / 1000);
  if (!Number.isFinite(seconds)) return "—";
  const formatter = new Intl.RelativeTimeFormat("zh-CN", { numeric: "auto" });
  if (Math.abs(seconds) < 60) return formatter.format(seconds, "second");
  const minutes = Math.round(seconds / 60);
  if (Math.abs(minutes) < 60) return formatter.format(minutes, "minute");
  const hours = Math.round(minutes / 60);
  if (Math.abs(hours) < 24) return formatter.format(hours, "hour");
  return formatter.format(Math.round(hours / 24), "day");
}

function formatOptionalDuration(value) {
  return typeof value === "number" && Number.isFinite(value) ? formatDuration(value) : "Unknown";
}

function formatDuration(milliseconds) {
  const ms = Math.max(0, Number(milliseconds || 0));
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  if (minutes < 60) return remainder ? `${minutes}m ${remainder}s` : `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const minuteRemainder = minutes % 60;
  return minuteRemainder ? `${hours}h ${minuteRemainder}m` : `${hours}h`;
}

function setSync(mode) {
  const node = $("#sync-state-main");
  if (!node) return;
  node.classList.toggle("syncing", mode === "syncing");
  node.classList.toggle("stale", mode === "offline");
  const title = mode === "live" ? "Live" : mode === "offline" ? "Offline" : "Syncing";
  node.title = title;
  node.textContent = title;
}

async function api(path, options = {}) {
  const response = await fetch(path, { credentials: "same-origin", ...options });
  if (!response.ok) {
    const error = new Error(`Request failed: ${response.status}`);
    error.status = response.status;
    throw error;
  }
  return response.json();
}

async function copyText(text) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const area = document.createElement("textarea");
  area.value = text;
  document.body.append(area);
  area.select();
  document.execCommand("copy");
  area.remove();
}

function stringifyOutput(value) {
  if (value == null) return "";
  if (typeof value === "string") return value;
  try { return JSON.stringify(value, null, 2); } catch { return String(value); }
}

function compactOutput(value, max = 4000) {
  const text = stringifyOutput(value);
  if (text.length <= max) return text;
  return `${text.slice(0, max)}\n… truncated`;
}

function firstLine(value) {
  return String(value || "").split(/\r?\n/).map((line) => line.trim()).find(Boolean) || "";
}

function firstDefined(...values) {
  for (const value of values) {
    if (value !== undefined && value !== null && value !== "") return value;
  }
  return undefined;
}

function escapeHTML(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}
