import {
  threadBucket,
  groupThreadsForRail,
  stableTitleFromThread,
  compactModelName,
  humanThreadMetadata,
} from "./shell-model.mjs";
import {
  filterEventsByMode,
  summarizeExecutions,
  formatCompactLabel,
  detectHandoffSticky,
  collectObservedModels,
  underAttributedModels,
  cacheHitRate,
  honestCostLabel,
  isFailedStatus,
  buildHumanTurns,
  stableTurnAnchor,
  turnDeepLinkHash,
} from "./timeline-model.mjs";

const APP_VERSION = "0.4.0";
const TURN_RENDER_CHUNK = 8;
const LONG_CONTENT_CHARS = 2400;
const LONG_CONTENT_LINES = 36;
const WORK_CLUSTER_PREVIEW = 12;
const state = {
  threads: [],
  currentID: null,
  current: null,
  view: "thread",
  timelineMode: "decision", // decision | full | errors
  searchTimer: null,
  pollTimer: null,
  refreshing: false,
  pollFailures: 0,
  renderToken: 0,
  pendingTurnAnchor: "",
  pendingEventID: "",
  outline: {
    marks: [],
    anchors: [],
    contentH: 1,
    trackH: 1,
    dragging: false,
    hoverIndex: -1,
    raf: 0,
    tipHideTimer: 0,
    tipThrottle: 0,
    lastY: 0,
    lastT: 0,
    velocity: 0,
    momentumRaf: 0,
    hoverClientY: null,
    lastTipText: "",
    lastPlayY: -1,
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
const workRevealers = new WeakMap();

document.addEventListener("DOMContentLoaded", () => {
  boot().catch((error) => showBootError(error));
});

async function boot() {
  try {
    bindEvents();
    restoreRailPreference();
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
  document.querySelectorAll(".timeline-mode-btn").forEach((btn) => {
    btn.addEventListener("click", () => setTimelineMode(btn.dataset.mode));
  });
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

  const stopMomentum = () => {
    if (state.outline.momentumRaf) {
      cancelAnimationFrame(state.outline.momentumRaf);
      state.outline.momentumRaf = 0;
    }
  };

  const ratioFromClientY = (clientY) => {
    // Use cached track box when possible to avoid layout thrash during drag.
    const rect = track.getBoundingClientRect();
    state.outline.trackH = rect.height || state.outline.trackH || 1;
    if (rect.height <= 0) return 0;
    return Math.min(1, Math.max(0, (clientY - rect.top) / rect.height));
  };

  const schedulePaint = (fn) => {
    if (state.outline.raf) return;
    state.outline.raf = requestAnimationFrame(() => {
      state.outline.raf = 0;
      fn();
    });
  };

  scroller.addEventListener("scroll", () => {
    schedulePaint(() => {
      if (state.outline.hoverClientY != null && !state.outline.dragging) {
        const ratio = ratioFromClientY(state.outline.hoverClientY);
        // Position every frame; text throttled inside.
        showOutlinePreviewAtRatio(ratio, state.outline.hoverClientY, { forceText: false });
        paintOutlinePlayhead({ hoverRatio: ratio });
      } else {
        paintOutlinePlayhead();
      }
    });
  }, { passive: true });

  const scrubFromClientY = (clientY, { preview = true, sampleVelocity = false } = {}) => {
    const ratio = ratioFromClientY(clientY);
    const max = Math.max(0, scroller.scrollHeight - scroller.clientHeight);
    const nextTop = ratio * max;
    const now = performance.now();
    if (sampleVelocity) {
      const dt = Math.max(1, now - (state.outline.lastT || now));
      const dy = nextTop - scroller.scrollTop;
      state.outline.velocity = state.outline.velocity * 0.6 + (dy / dt) * 0.4;
      state.outline.lastT = now;
      state.outline.lastY = clientY;
    }
    // Direct assignment — no smooth scroll fighting pointer.
    scroller.scrollTop = nextTop;
    state.outline.hoverClientY = clientY;
    paintOutlinePlayhead({ hoverRatio: ratio });
    if (preview) showOutlinePreviewAtRatio(ratio, clientY, { forceText: sampleVelocity ? false : true });
  };

  const startMomentum = () => {
    stopMomentum();
    let v = state.outline.velocity;
    if (Math.abs(v) < 0.06) {
      state.outline.velocity = 0;
      return;
    }
    v = Math.max(-7.5, Math.min(7.5, v));
    let prev = performance.now();
    const step = (now) => {
      const dt = Math.min(32, now - prev);
      prev = now;
      v *= Math.pow(0.96, dt / 16.67);
      if (Math.abs(v) < 0.028) {
        state.outline.velocity = 0;
        state.outline.momentumRaf = 0;
        return;
      }
      const max = Math.max(0, scroller.scrollHeight - scroller.clientHeight);
      scroller.scrollTop = Math.max(0, Math.min(max, scroller.scrollTop + v * dt));
      // scroll listener paints playhead; only refresh tip text throttled
      if (state.outline.hoverClientY != null) {
        const ratio = ratioFromClientY(state.outline.hoverClientY);
        showOutlinePreviewAtRatio(ratio, state.outline.hoverClientY, { forceText: false });
      }
      state.outline.momentumRaf = requestAnimationFrame(step);
    };
    state.outline.momentumRaf = requestAnimationFrame(step);
  };

  track.addEventListener("pointerdown", (event) => {
    if (event.button !== 0) return;
    stopMomentum();
    state.outline.dragging = true;
    state.outline.velocity = 0;
    state.outline.lastT = performance.now();
    state.outline.lastY = event.clientY;
    scrubber.classList.add("is-dragging", "is-hovering");
    track.setPointerCapture?.(event.pointerId);
    scrubFromClientY(event.clientY, { preview: true, sampleVelocity: false });
    event.preventDefault();
  });
  track.addEventListener("pointermove", (event) => {
    state.outline.hoverClientY = event.clientY;
    if (state.outline.dragging) {
      stopMomentum();
      scrubFromClientY(event.clientY, { preview: true, sampleVelocity: true });
      return;
    }
    scrubber.classList.add("is-hovering");
    const ratio = ratioFromClientY(event.clientY);
    paintOutlinePlayhead({ hoverRatio: ratio });
    showOutlinePreviewAtRatio(ratio, event.clientY, { forceText: false });
  });
  track.addEventListener("pointerenter", (event) => {
    scrubber.classList.add("is-hovering");
    state.outline.hoverClientY = event.clientY;
    const ratio = ratioFromClientY(event.clientY);
    paintOutlinePlayhead({ hoverRatio: ratio });
    showOutlinePreviewAtRatio(ratio, event.clientY, { forceText: true });
  });
  const endDrag = (event) => {
    if (!state.outline.dragging) return;
    state.outline.dragging = false;
    scrubber.classList.remove("is-dragging");
    try { track.releasePointerCapture?.(event.pointerId); } catch {}
    startMomentum();
  };
  track.addEventListener("pointerup", endDrag);
  track.addEventListener("pointercancel", endDrag);
  track.addEventListener("lostpointercapture", endDrag);
  track.addEventListener("pointerleave", () => {
    if (state.outline.dragging) return;
    state.outline.hoverClientY = null;
    scrubber.classList.remove("is-hovering");
    hideOutlineTooltip();
    paintOutlinePlayhead();
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
  const match = location.hash.match(/^#\/thread\/([^/]+)(?:\/(usage)|\/turn\/([^/]+))?$/);
  if (match) {
    let id;
    let turnAnchor = "";
    try { id = decodeURIComponent(match[1]); } catch { id = match[1]; }
    try { turnAnchor = match[3] ? decodeURIComponent(match[3]) : ""; } catch { turnAnchor = match[3] || ""; }
    state.pendingTurnAnchor = turnAnchor;
    selectThread(id, match[2] === "usage" ? "usage" : "thread", false);
  } else if (state.threads.length) {
    state.pendingTurnAnchor = "";
    selectThread(state.threads[0].session_id, "thread", false);
  }
}

function selectThread(id, view = "thread", updateHash = true) {
  state.currentID = id;
  state.view = view;
  if (updateHash) {
    state.pendingTurnAnchor = "";
    updateLocationHash();
  }
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
  renderHumanHeader(thread, graph.events || []);

  const topModel = $("#topbar-model");
  const topEffort = $("#topbar-effort");
  const topSep = document.querySelector(".topbar-sep");
  const modelLabel = compactModel(thread.model);
  const effortLabel = String(thread.effort || "").trim();
  if (topModel) {
    topModel.textContent = modelLabel;
    topModel.hidden = !modelLabel;
  }
  if (topEffort) {
    topEffort.textContent = effortLabel;
    topEffort.hidden = !effortLabel;
  }
  if (topSep) topSep.hidden = !modelLabel || !effortLabel;

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
  renderThreadBadges(graph);
  renderExecutionStrip(graph);
  syncTimelineModeUI();
  // Timeline modes live in Details drawer only (P6 visual: no mid-page segmented control).
  renderTurns(graph);
  renderArtifacts(graph.artifacts || []);
  renderUsage(graph.usage || {});
  const policy = $("#redaction-policy");
  if (policy) policy.textContent = graph.redaction?.policy_version || "unknown";
  renderViewState();
}

function setTimelineMode(mode) {
  if (!["decision", "full", "errors"].includes(mode)) return;
  state.timelineMode = mode;
  syncTimelineModeUI();
  if (state.current) renderTurns(state.current);
}

function syncTimelineModeUI() {
  document.querySelectorAll(".timeline-mode-btn").forEach((btn) => {
    const active = btn.dataset.mode === state.timelineMode;
    btn.classList.toggle("is-active", active);
    btn.setAttribute("aria-selected", String(active));
  });
}

function renderHumanHeader(thread, events) {
  const title = $("#human-thread-title");
  const meta = $("#human-thread-meta");
  if (title) title.textContent = stableTitle(thread);
  if (!meta) return;
  meta.replaceChildren();
  const entries = humanThreadMetadata(thread, Array.isArray(events) ? events.length : undefined);
  for (const entry of entries) {
    const item = document.createElement("span");
    item.className = `human-meta-item human-meta-${entry.key}`;
    if (entry.key === "state") {
      item.dataset.state = String(entry.value || "");
      item.textContent = stateLabel(entry.value);
    } else if (entry.key === "updated_at" || entry.key === "started_at") {
      const time = document.createElement("time");
      time.dateTime = String(entry.value);
      const prefix = entry.key === "updated_at" ? "Updated" : "Started";
      time.textContent = `${prefix} ${formatDate(entry.value)} (${relativeTime(entry.value)})`;
      meta.append(time);
      continue;
    } else if (entry.key === "project") {
      item.textContent = String(entry.value);
    } else if (entry.key === "model") {
      item.textContent = compactModel(entry.value);
    } else if (entry.key === "effort") {
      item.textContent = String(entry.value);
    } else if (entry.key === "events") {
      item.textContent = `${number(entry.value)} event${Number(entry.value) === 1 ? "" : "s"}`;
    }
    if (item.textContent) meta.append(item);
  }
}

function renderThreadBadges(graph) {
  const host = $("#thread-badges");
  if (!host) return;
  host.replaceChildren();
  // Degrade-friendly: no events / no matches → hide quietly (never throw).
  // P6: short topbar meta (dot + word), not mid-page pill wall.
  let flags = [];
  try {
    const events = Array.isArray(graph?.events) ? graph.events : [];
    const detected = detectHandoffSticky(events);
    if (detected.sticky) flags.push(["Sticky", "badge-sticky", "Sticky · ack required"]);
    if (detected.handoff) flags.push(["Handoff", "badge-handoff", "Handoff required"]);
    if (detected.hasGate) {
      if (detected.gateOpen && !detected.gateClose) flags.push(["Gate", "badge-gate", "Gate · open"]);
      else if (detected.gateClose && !detected.gateOpen) flags.push(["Gate", "badge-gate", "Gate · cleared"]);
      else flags.push(["Gate", "badge-gate", "Gate"]);
    }
  } catch {
    flags = [];
  }
  if (!flags.length) {
    host.hidden = true;
    return;
  }
  host.hidden = false;
  for (const [label, cls, title] of flags) {
    const chip = document.createElement("span");
    chip.className = `topbar-flag ${cls}`;
    chip.textContent = label;
    if (title) chip.title = title;
    host.append(chip);
  }
}

function renderExecutionStrip(graph) {
  const host = $("#execution-strip");
  if (!host) return;
  const events = Array.isArray(graph.events) ? graph.events : [];
  const summary = summarizeExecutions(events);
  if (!summary.items.length) {
    host.hidden = true;
    host.replaceChildren();
    return;
  }
  host.hidden = false;
  host.replaceChildren();

  // P6: Amp-like collapsed Work line; chips only after expand.
  const details = document.createElement("details");
  details.className = "work-strip";
  const summaryEl = document.createElement("summary");
  const label = document.createElement("span");
  label.className = "work-strip-label";
  const parts = [];
  if (summary.workers) parts.push(`${summary.workers} worker${summary.workers === 1 ? "" : "s"}`);
  if (summary.agents) parts.push(`${summary.agents} agent${summary.agents === 1 ? "" : "s"}`);
  if (summary.failed) parts.push(`${summary.failed} failed`);
  if (summary.totalMs > 0) parts.push(formatDuration(summary.totalMs));
  label.textContent = parts.length ? `Work · ${parts.join(" · ")}` : "Work";
  summaryEl.append(label);
  details.append(summaryEl);

  const list = document.createElement("div");
  list.className = "execution-strip-list";
  const visibleItems = summary.items.slice(0, WORK_CLUSTER_PREVIEW);
  for (const item of visibleItems) {
    const chip = document.createElement("button");
    chip.type = "button";
    chip.className = `execution-chip${item.failed ? " is-failed" : ""}`;
    const model = compactModel(item.model);
    const durationText = item.duration_ms != null && Number(item.duration_ms) > 0
      ? formatDuration(item.duration_ms)
      : "";
    chip.textContent = [
      item.kind === "agent" ? "Agent" : "Worker",
      model,
      item.effort,
      stateLabel(item.status),
      durationText,
    ].filter(Boolean).join(" · ");
    chip.title = item.summary || chip.textContent;
    if (item.event_id) {
      chip.addEventListener("click", () => {
        state.pendingEventID = item.event_id;
        closeMetadata();
        if (state.timelineMode !== "full") setTimelineMode("full");
        else renderTurns(state.current);
      });
    }
    list.append(chip);
  }
  if (summary.items.length > visibleItems.length) {
    const remaining = summary.items.length - visibleItems.length;
    const note = document.createElement("p");
    note.className = "execution-strip-more";
    note.textContent = `${remaining} more execution${remaining === 1 ? "" : "s"} available in Full timeline.`;
    list.append(note);
  }
  details.append(list);
  host.append(details);
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

/* ── Human View turn model ── */

function eventsForHumanView(graph) {
  const events = Array.isArray(graph.events) ? graph.events : [];
  const rootID = graph.thread?.session_id;
  const scoped = !rootID
    ? events.slice()
    : events.filter((event) => {
      // Main timeline: this thread's own messages/tools + worker cards.
      // Child chat text lives in the child thread.
      if (!event.session_id || event.session_id === rootID) return true;
      if (event.type === "worker") return true;
      return false;
    });

  const filtered = filterEventsByMode(scoped, state.timelineMode || "decision");

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
  const token = ++state.renderToken;
  const allScoped = (() => {
    const events = Array.isArray(graph.events) ? graph.events : [];
    const rootID = graph.thread?.session_id;
    if (!rootID) return events.slice();
    return events.filter((event) => {
      if (!event.session_id || event.session_id === rootID) return true;
      if (event.type === "worker") return true;
      return false;
    });
  })();
  const events = eventsForHumanView(graph);
  if (!events.length) {
    list.append(inlineState(
      state.timelineMode === "errors"
        ? "No failed events in this Thread."
        : state.timelineMode === "full"
          ? "No canonical events recorded for this Thread."
          : "No decision-path events for this Thread. Try Full mode.",
    ));
    hideOutlineScrubber();
    return;
  }

  // Compact markers numbered in full-session order for stable labels.
  const compactIndex = new Map();
  let compactN = 0;
  for (const event of allScoped) {
    if (event.type === "compact") {
      compactIndex.set(event.event_id, compactN);
      compactN += 1;
    }
  }
  state._compactIndex = compactIndex;

  // Progressive paint for d791-scale threads (P5): keep first paint interactive.
  const turns = buildHumanTurns(events);
  let index = 0;
  const paintChunk = () => {
    if (token !== state.renderToken) return;
    const frag = document.createDocumentFragment();
    const end = Math.min(index + TURN_RENDER_CHUNK, turns.length);
    for (; index < end; index += 1) {
      frag.append(renderTurn(turns[index], graph, index));
    }
    list.append(frag);
    if (index < turns.length) {
      requestAnimationFrame(paintChunk);
      return;
    }
    requestAnimationFrame(() => {
      if (token !== state.renderToken) return;
      rebuildOutlineMarks();
      updateOutlineThumb();
      if (state.pendingTurnAnchor) {
        focusTurn(state.pendingTurnAnchor, { updateHash: false });
        state.pendingTurnAnchor = "";
      }
      if (state.pendingEventID) {
        const eventID = state.pendingEventID;
        state.pendingEventID = "";
        focusEvent(eventID);
      }
    });
  };
  paintChunk();
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
    tip.classList.remove("is-visible");
    // Keep in DOM for exit transition; hide after fade.
    window.clearTimeout(state.outline.tipHideTimer);
    state.outline.tipHideTimer = window.setTimeout(() => {
      if (tip && !tip.classList.contains("is-visible")) tip.hidden = true;
    }, 180);
  }
  state.outline.hoverIndex = -1;
}

function clipOutlineText(value, max = 220) {
  // Keep line breaks for Amp-style multi-line tooltip cards.
  const text = String(value || "").replace(/\r\n/g, "\n").replace(/[ \t]+\n/g, "\n").replace(/\n{3,}/g, "\n\n").trim();
  if (!text) return "";
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

function outlinePreviewForNode(node) {
  if (!node) return "";
  if (node.classList.contains("user-bubble")) {
    return clipOutlineText(node.querySelector(".user-text")?.textContent || "", 260);
  }
  if (node.matches("h1, h2, h3, h4")) {
    const heading = (node.textContent || "").trim();
    const next = node.nextElementSibling;
    const body = next?.textContent || "";
    return clipOutlineText([heading, body].filter(Boolean).join("\n"), 260);
  }
  if (node.classList.contains("assistant-answer") || node.classList.contains("assistant-note")) {
    return clipOutlineText(node.querySelector(".message-content")?.textContent || "", 260);
  }
  if (node.classList.contains("tool-event") || node.classList.contains("execution-card")) {
    return clipOutlineText(node.querySelector(".tool-summary-copy")?.textContent || "", 200);
  }
  if (node.classList.contains("worked-for")) {
    // Prefer surrounding human text over the "Worked for" chrome label.
    return "";
  }
  return clipOutlineText(node.textContent || "", 220);
}

function collectOutlineNodes() {
  const list = $("#event-list");
  if (!list) return [];
  const nodes = [];
  const seen = new Set();
  const push = (node, kind, priority) => {
    if (!node || seen.has(node)) return;
    seen.add(node);
    if (!node.id) {
      node.id = `outline-${kind}-${nodes.length}-${Math.random().toString(36).slice(2, 7)}`;
    }
    const preview = outlinePreviewForNode(node);
    if (!preview && kind === "work") return; // skip empty work chrome
    nodes.push({
      node,
      kind,
      priority,
      label: clipOutlineText(preview.split("\n")[0] || kind, 80),
      preview,
    });
  };

  // Priority matches Amp tooltip preference: user messages > answers > headings > notes > tools
  for (const el of list.querySelectorAll(".user-bubble")) push(el, "user", 0);
  for (const el of list.querySelectorAll(".assistant-answer")) push(el, "answer", 1);
  for (const el of list.querySelectorAll(".assistant-answer .message-content h1, .assistant-answer .message-content h2, .assistant-answer .message-content h3")) {
    push(el, "heading", 2);
  }
  for (const el of list.querySelectorAll(".assistant-note")) push(el, "note", 3);
  for (const el of list.querySelectorAll(".tool-event, .execution-card")) push(el, "tool", 4);
  return nodes;
}

function rebuildOutlineMarks() {
  const scrubber = $("#outline-scrubber");
  const marksRoot = $("#outline-marks");
  const scroller = $("#conversation-scroll");
  const track = $("#outline-track");
  if (!scrubber || !marksRoot || !scroller) return;

  if (state.view !== "thread" || !state.currentID || scroller.scrollHeight <= scroller.clientHeight + 24) {
    hideOutlineScrubber();
    return;
  }

  const items = collectOutlineNodes();
  if (!items.length && scroller.scrollHeight <= scroller.clientHeight + 24) {
    hideOutlineScrubber();
    return;
  }

  // Cache absolute tops once — binary search later, no getBoundingClientRect per frame.
  const scrollerRect = scroller.getBoundingClientRect();
  const contentH = Math.max(1, scroller.scrollHeight - 1);
  state.outline.contentH = contentH;
  const anchors = [];
  for (const item of items) {
    const rect = item.node.getBoundingClientRect();
    const top = rect.top - scrollerRect.top + scroller.scrollTop;
    anchors.push({
      top,
      ratio: Math.min(1, Math.max(0, top / contentH)),
      preview: item.preview,
      label: item.label,
      kind: item.kind,
      priority: item.priority,
      node: item.node,
    });
  }
  anchors.sort((a, b) => a.top - b.top);
  state.outline.anchors = anchors;
  state.outline.marks = anchors;

  const trackH = Math.max(1, track?.clientHeight || scroller.clientHeight - 28);
  state.outline.trackH = trackH;
  const spacing = 6.5;
  const microCount = Math.min(140, Math.max(48, Math.round(trackH / spacing)));
  // Rebuild static ticks only when count changes (avoid DOM churn on every select).
  if (marksRoot.childElementCount !== microCount) {
    const frag = document.createDocumentFragment();
    for (let i = 0; i < microCount; i += 1) {
      const ratio = microCount === 1 ? 0 : i / (microCount - 1);
      const micro = document.createElement("span");
      micro.className = "outline-mark";
      micro.style.top = `${ratio * 100}%`;
      frag.append(micro);
    }
    marksRoot.replaceChildren(frag);
  }

  scrubber.hidden = false;
  paintOutlinePlayhead();
}

function contentAtRatio(ratio) {
  const anchors = state.outline.anchors;
  if (!anchors.length) return { label: "", preview: "…", node: null };
  const contentH = state.outline.contentH || 1;
  const targetY = ratio * contentH;
  // Binary search nearest anchor by top.
  let lo = 0;
  let hi = anchors.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (anchors[mid].top < targetY) lo = mid + 1;
    else hi = mid;
  }
  let best = anchors[lo];
  if (lo > 0) {
    const prev = anchors[lo - 1];
    if (Math.abs(prev.top - targetY) < Math.abs(best.top - targetY)) best = prev;
  }
  // Prefer a nearby higher-priority (user/answer) anchor within 120px.
  const window = 120;
  for (const a of anchors) {
    if (Math.abs(a.top - targetY) > window) continue;
    if ((a.priority ?? 9) < (best.priority ?? 9)) best = a;
  }
  return {
    label: best.label,
    preview: best.preview || best.label || "…",
    mark: best,
    node: best.node,
  };
}

function extractOutlineFileMeta(text) {
  const raw = String(text || "");
  const pathMatch = raw.match(/(?:[\w.-]+\/)+[\w.-]+\.[a-zA-Z0-9]{1,8}\b/);
  if (pathMatch) return pathMatch[0].split("/").pop() || pathMatch[0];
  const bare = raw.match(/\b[\w.-]+\.(?:md|ts|tsx|js|mjs|css|json|go|py|toml|yml|yaml)\b/);
  return bare ? bare[0] : "";
}

function showOutlinePreviewAtRatio(ratio, clientY, { forceText = false } = {}) {
  const tip = $("#outline-tooltip");
  const scrubber = $("#outline-scrubber");
  if (!tip || !scrubber) return;

  const trackH = state.outline.trackH || $("#outline-track")?.clientHeight || 1;
  let y;
  if (typeof clientY === "number") {
    const scrubTop = scrubber.getBoundingClientRect().top;
    y = clientY - scrubTop;
  } else {
    y = ratio * trackH;
  }
  y = Math.min(trackH - 16, Math.max(16, y));
  // Amp: tooltip locked to pointer — no lag / no spring.
  tip.style.top = `${y}px`;

  window.clearTimeout(state.outline.tipHideTimer);
  tip.hidden = false;
  tip.classList.add("is-visible");
  scrubber.classList.add("is-hovering");

  const now = performance.now();
  if (!forceText && now - state.outline.tipThrottle < 32) return;
  state.outline.tipThrottle = now;

  const { preview, mark, node } = contentAtRatio(ratio);
  const full = clipOutlineText(preview || "…", 320);
  const lines = full.split("\n").map((l) => l.trim()).filter(Boolean);
  const titleText = lines[0] || "…";
  const bodyText = lines.slice(1).join("\n") || lines[0] || "…";
  const fileMeta = extractOutlineFileMeta(full)
    || ($("#detail-title")?.textContent || "").trim().slice(0, 40);

  let titleEl = tip.querySelector(".outline-tip-title");
  let bodyEl = tip.querySelector(".outline-tip-body");
  let fileEl = tip.querySelector(".outline-tip-file");
  if (!titleEl || !bodyEl) {
    tip.replaceChildren();
    titleEl = document.createElement("strong");
    titleEl.className = "outline-tip-title";
    bodyEl = document.createElement("p");
    bodyEl.className = "outline-tip-body";
    tip.append(titleEl, bodyEl);
  }
  const tipKey = `${titleText}\n${bodyText}\n${fileMeta}`;
  if (tipKey !== state.outline.lastTipText) {
    titleEl.textContent = titleText;
    if (lines.length <= 1) {
      titleEl.hidden = true;
      bodyEl.textContent = titleText;
    } else {
      titleEl.hidden = false;
      bodyEl.textContent = bodyText;
    }
    if (fileMeta) {
      if (!fileEl) {
        fileEl = document.createElement("div");
        fileEl.className = "outline-tip-file";
        fileEl.innerHTML = `<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path fill="none" stroke="currentColor" stroke-width="1.7" d="M14 2v6h6"/></svg><span></span>`;
        tip.append(fileEl);
      }
      fileEl.hidden = false;
      fileEl.querySelector("span").textContent = fileMeta;
    } else if (fileEl) {
      fileEl.hidden = true;
    }
    state.outline.lastTipText = tipKey;
  }
  state.outline.hoverIndex = mark ? state.outline.anchors.indexOf(mark) : -1;
  void node;
}

/**
 * Amp mid-5s consecutive frames: playhead + focus band stick to pointer 1:1.
 * Motion comes from CONTENT scrolling (drag + inertia), not springy chrome.
 * No scaleY stretch, no laggy float.
 */
function paintOutlinePlayhead(opts = {}) {
  const scroller = $("#conversation-scroll");
  const thumb = $("#outline-thumb");
  const focus = $("#outline-focus-band");
  const scrubber = $("#outline-scrubber");
  const track = $("#outline-track");
  if (!scroller || !thumb || !scrubber || scrubber.hidden) return;

  const trackH = track?.clientHeight || state.outline.trackH || 1;
  state.outline.trackH = trackH;
  const contentH = Math.max(1, scroller.scrollHeight - 1);
  state.outline.contentH = contentH;

  const hovering = scrubber.classList.contains("is-hovering") || state.outline.dragging;
  const readY = scroller.scrollTop + scroller.clientHeight * 0.18;
  const scrollRatio = Math.min(1, Math.max(0, readY / contentH));

  let playRatio = scrollRatio;
  if (typeof opts.hoverRatio === "number") {
    playRatio = opts.hoverRatio;
  } else if (hovering && state.outline.hoverClientY != null && track) {
    const rect = track.getBoundingClientRect();
    if (rect.height > 0) {
      playRatio = Math.min(1, Math.max(0, (state.outline.hoverClientY - rect.top) / rect.height));
    }
  }

  const y = playRatio * trackH;
  if (Math.abs(y - state.outline.lastPlayY) < 0.15 && opts.hoverRatio == null && !hovering) return;
  state.outline.lastPlayY = y;

  // Direct GPU transforms — 1:1 with target (Amp is not springy here).
  const t = `translate3d(0, ${y}px, 0) translateY(-50%)`;
  thumb.style.transform = t;
  if (focus) focus.style.transform = `translate3d(0, ${y}px, 0)`;
}

function updateOutlineThumb(opts = {}) {
  paintOutlinePlayhead(opts);
}

function renderTurn(turn, graph, index = 0) {
  const card = document.createElement("section");
  const anchor = stableTurnAnchor(turn, index);
  card.className = "turn-card";
  card.id = anchor;
  card.dataset.turnId = turn.id;
  card.append(renderTurnHeader(turn, anchor));

  if (turn.user) card.append(renderUserBubble(turn.user));

  const workItems = [];
  for (const note of turn.processNotes || []) {
    workItems.push({ kind: "message", event: note });
  }
  workItems.push(...turn.activity);
  workItems.sort((a, b) => (Date.parse(a.event?.started_at) || 0) - (Date.parse(b.event?.started_at) || 0));

  if (workItems.length) card.append(renderWorkedFor(turn, workItems, graph));

  for (const answer of turn.finalAnswers || []) {
    card.append(renderAssistantAnswer(answer));
  }

  if (!turn.user && !(turn.finalAnswers || []).length && !workItems.length) {
    card.append(inlineState("Empty turn"));
  }
  return card;
}

function renderTurnHeader(turn, anchor) {
  const header = document.createElement("header");
  header.className = "turn-header";
  const meta = document.createElement("div");
  meta.className = "turn-meta";
  if (turn.startedAt) meta.append(timestampNode(turn.startedAt));
  if (turn.durationMs > 0) meta.append(metaSeparator(), textMeta(formatDuration(turn.durationMs), "turn-duration"));
  if (turn.status) meta.append(metaSeparator(), textMeta(stateLabel(turn.status), "turn-status"));

  const actions = document.createElement("div");
  actions.className = "turn-actions";
  const link = document.createElement("a");
  link.className = "turn-anchor-link";
  link.href = turnDeepLinkHash(state.currentID, { id: anchor });
  link.textContent = "#";
  link.setAttribute("aria-label", "Link to this turn");
  link.addEventListener("click", (event) => {
    event.preventDefault();
    history.replaceState(null, "", link.href);
    focusTurn(anchor, { updateHash: false });
  });
  const copy = document.createElement("button");
  copy.type = "button";
  copy.className = "copy-turn-link";
  copy.textContent = "Copy link";
  copy.addEventListener("click", async () => {
    const url = new URL(location.href);
    url.hash = turnDeepLinkHash(state.currentID, { id: anchor }).slice(1);
    await copyText(url.toString());
    copy.textContent = "Copied";
    setTimeout(() => { copy.textContent = "Copy link"; }, 1200);
  });
  actions.append(link, copy);
  header.append(meta, actions);
  return header;
}

function timestampNode(value) {
  const time = document.createElement("time");
  time.dateTime = String(value);
  time.textContent = formatDate(value);
  time.title = String(value);
  return time;
}

function metaSeparator() {
  return document.createTextNode(" · ");
}

function textMeta(value, className) {
  const span = document.createElement("span");
  span.className = className;
  span.textContent = value;
  return span;
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
  article.append(renderMessageMeta("Assistant work note", event));
  article.append(renderExpandableContent(event.content || event.summary || "", { label: "Show full note" }));
  return article;
}

function renderUserBubble(event) {
  const article = document.createElement("article");
  article.className = "user-bubble";
  article.id = eventAnchor(event.event_id);
  article.dataset.eventId = event.event_id || "";
  article.append(renderMessageMeta("User", event));
  article.append(renderExpandableContent(event.content || event.summary || "", {
    className: "user-text",
    label: "Show full message",
    plain: true,
  }));
  return article;
}

function renderMessageMeta(role, event) {
  const meta = document.createElement("div");
  meta.className = "message-meta";
  const label = document.createElement("strong");
  label.textContent = role;
  meta.append(label);
  if (event.started_at) meta.append(metaSeparator(), timestampNode(event.started_at));
  return meta;
}

function renderExpandableContent(source, options = {}) {
  const text = String(source || "");
  const className = options.className || "message-content";
  const container = document.createElement("div");
  container.className = className;
  const lines = text.split("\n");
  const long = text.length > LONG_CONTENT_CHARS || lines.length > LONG_CONTENT_LINES;
  const appendContent = (host, value) => {
    if (options.plain) host.textContent = value;
    else host.append(renderMarkdown(value));
  };
  if (!long) {
    appendContent(container, text);
    return container;
  }

  container.classList.add("is-expandable");
  const preview = document.createElement("div");
  preview.className = "content-preview";
  const clippedLines = lines.slice(0, LONG_CONTENT_LINES);
  let clipped = clippedLines.join("\n").slice(0, LONG_CONTENT_CHARS).trimEnd();
  if (clipped.length < text.length) clipped += "\n…";
  appendContent(preview, clipped);

  const details = document.createElement("details");
  details.className = "content-expander";
  const summary = document.createElement("summary");
  summary.textContent = options.label || "Show full content";
  const full = document.createElement("div");
  full.className = "expanded-content";
  let mounted = false;
  details.addEventListener("toggle", () => {
    preview.hidden = details.open;
    summary.textContent = details.open ? "Collapse content" : (options.label || "Show full content");
    if (details.open && !mounted) {
      appendContent(full, text);
      mounted = true;
    }
  });
  details.append(summary, full);
  container.append(preview, details);
  return container;
}

function renderWorkedFor(turn, activityItems, graph) {
  if (!activityItems.length) return document.createDocumentFragment();

  const details = document.createElement("details");
  details.className = "worked-for";
  const summary = document.createElement("summary");
  const label = document.createElement("span");
  label.className = "worked-label";
  const eventCount = activityItems.length;
  const countText = `${eventCount} event${eventCount === 1 ? "" : "s"}`;
  const closedText = `Show Work · ${countText}`;
  label.textContent = closedText;
  summary.append(label);

  const body = document.createElement("div");
  body.className = "worked-body";
  let mounted = false;
  let remainingClusters = [];
  let moreButton = null;
  const revealAll = () => {
    if (!mounted) mount();
    if (!remainingClusters.length) return;
    for (const cluster of remainingClusters) body.insertBefore(renderWorkCluster(cluster, graph), moreButton);
    remainingClusters = [];
    moreButton?.remove();
    moreButton = null;
  };
  const mount = () => {
    if (mounted) return;
    mounted = true;
    body.replaceChildren();
    const clusters = clusterActivity(activityItems);
    const initial = clusters.slice(0, WORK_CLUSTER_PREVIEW);
    remainingClusters = clusters.slice(initial.length);
    for (const cluster of initial) body.append(renderWorkCluster(cluster, graph));
    if (remainingClusters.length) {
      const remaining = remainingClusters.length;
      moreButton = document.createElement("button");
      moreButton.type = "button";
      moreButton.className = "show-more-work";
      moreButton.textContent = `Show ${remaining} more work group${remaining === 1 ? "" : "s"}`;
      moreButton.addEventListener("click", revealAll);
      body.append(moreButton);
    }
  };
  workRevealers.set(details, revealAll);
  details.addEventListener("toggle", () => {
    label.textContent = details.open ? `Hide Work · ${countText}` : closedText;
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
    // Explore groups read+search together while preserving every recorded event.

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
  for (const item of cluster.items) body.append(renderActivityItem(item, graph));
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

  article.append(renderMessageMeta("Assistant", event));
  article.append(toolbar, renderExpandableContent(event.content || event.summary || "", {
    label: "Show full response",
  }));
  return article;
}

function renderProcessNote(event) {
  return renderAssistantNote(event);
}

function renderTool(event, result) {
  const details = document.createElement("details");
  const failed = isFailedStatus(result?.status) || isFailedStatus(event.status);
  details.className = `tool-event activity-item${failed ? " failed" : ""}`;
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
    nodes.push(fieldBlock("Output", compactOutput(output, 4000) || "No visible output."));
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
  rawPre.textContent = compactOutput(
    JSON.stringify({ input: event.raw, result: result?.raw ?? result?.content ?? null }, null, 2),
    6000,
  );
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
  // P6: single quiet line + thin left rail for failed — not a status-chip wall.
  const details = document.createElement("details");
  const failed = ["failed", "error", "blocked", "cancelled"].includes(String(event.status || "").toLowerCase());
  details.className = `execution-card tool-event${failed ? " failed" : ""}`;
  details.id = eventAnchor(event.event_id);
  details.dataset.eventId = event.event_id || "";

  const kind = event.role === "agent" ? "Agent" : "Worker";
  const objective = firstLine(event.raw?.objective || event.summary || `${kind} invocation`);
  const summary = document.createElement("summary");
  summary.className = "tool-summary execution-summary";
  const heading = document.createElement("span");
  heading.className = "tool-summary-copy execution-copy";
  const name = document.createElement("strong");
  name.textContent = kind;
  const description = document.createElement("span");
  const requested = compactModel(event.raw?.requested_model);
  const resolved = compactModel(event.raw?.resolved_model || event.model);
  const modelBits = [];
  if (requested && resolved && requested !== resolved) modelBits.push(`${requested}→${resolved}`);
  else if (resolved || requested) modelBits.push(resolved || requested);
  const duration = Number(event.duration_ms);
  description.textContent = [
    objective,
    ...modelBits,
    event.effort || event.raw?.resolved_effort,
    stateLabel(event.status),
    Number.isFinite(duration) && duration > 0 ? formatDuration(duration) : "",
  ].filter(Boolean).join(" · ");
  description.title = description.textContent;
  heading.append(name, description);
  summary.append(heading);
  details.append(summary);

  const body = document.createElement("div");
  body.className = "tool-body";
  if (failed && (event.summary || event.content)) {
    const failNote = document.createElement("p");
    failNote.className = "execution-fail-note";
    failNote.textContent = firstLine(event.summary || event.content || "Failed");
    body.append(failNote);
  }
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
  const isGate = event.type === "gate" || event.type === "workflow";
  article.className = [
    "activity-item",
    failed ? "failed" : "",
    event.type === "compact" ? "compact-marker" : "",
    isGate ? "gate-marker" : "",
  ].filter(Boolean).join(" ");
  article.id = eventAnchor(event.event_id);
  const header = document.createElement("header");
  const label = document.createElement("strong");
  if (event.type === "compact") {
    const idx = state._compactIndex?.get(event.event_id) ?? 0;
    label.textContent = formatCompactLabel(event, idx);
  } else if (event.type === "gate") {
    label.textContent = `Gate · ${event.status || "event"}`;
  } else if (event.type === "workflow") {
    label.textContent = `Workflow · ${event.status || "event"}`;
  } else {
    label.textContent = event.type === "error" ? "Error" : event.type || "Event";
  }
  const time = document.createElement("time");
  time.textContent = formatTime(event.started_at);
  header.append(label, time);
  const content = document.createElement("p");
  content.className = "execution-result";
  // Gate cloud payloads should stay class/counts only — show summary as-is, no prompt dump.
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

  // Observed models from participants/worker cards (honest, non-usage).
  const observed = collectObservedModels(
    state.current?.participants || [],
    state.current?.events || [],
  );
  const missing = underAttributedModels(observed, usage.models || []);
  if (missing.length || ((usage.models || []).length <= 1 && observed.length > 1)) {
    const banner = document.createElement("p");
    banner.className = "usage-attribution-banner";
    const listed = missing.slice(0, 8).join(", ");
    banner.textContent = listed
      ? `Historical under-attribution: usage rows omit ${listed}. Those models appear under Observed executors / execution cards — do not treat missing usage rows as “never ran”.`
      : "Historical under-attribution: usage may only show the supervisor model. Child/agent models appear in Observed executors when recorded; do not treat missing rows as “never ran”.";
    container.append(banner);
  }

  const defs = document.createElement("p");
  defs.className = "usage-definitions";
  defs.textContent = "Active duration excludes idle before first model activity. Subscription ≠ invoice amount. Unpriced costs show Price pending (never $0). Observed executors are not billed tokens.";
  container.append(defs);

  if (observed.length) {
    container.append(observedModelsSection(observed, missing));
  }

  const cacheRate = cacheHitRate(totals);
  const summary = document.createElement("dl");
  summary.className = "usage-summary";
  for (const [label, value] of [
    ["Total tokens", usageNumber(totals.total_tokens, totals.updated_at)],
    ["Requests", usageNumber(totals.requests, totals.updated_at)],
    ["Cache hit rate", cacheRate == null ? "Unknown" : `${Math.round(cacheRate * 100)}%`],
    ["Active duration", formatOptionalDuration(totals.active_duration_ms)],
    ["Worker compute", formatOptionalDuration(totals.worker_compute_duration_ms)],
    ["Cost", formatCost(totals)],
  ]) {
    const item = document.createElement("div");
    const term = document.createElement("dt");
    term.textContent = label;
    const detail = document.createElement("dd");
    detail.textContent = value;
    if (label === "Cost" && value === "Price pending") detail.className = "pending-cost";
    item.append(term, detail);
    summary.append(item);
  }
  container.append(summary);

  // Supervisor vs workers share when roles exist
  const roles = usage.roles || [];
  if (roles.length) {
    const sup = roles.find((r) => String(r.role).toLowerCase() === "supervisor");
    const workerish = roles.filter((r) => /worker|agent|specialist/i.test(String(r.role || "")));
    const supTok = Number(sup?.total_tokens) || 0;
    const workTok = workerish.reduce((n, r) => n + (Number(r.total_tokens) || 0), 0);
    if (supTok + workTok > 0) {
      const bar = document.createElement("div");
      bar.className = "usage-role-bar";
      const supPct = Math.round((supTok / (supTok + workTok)) * 100);
      bar.innerHTML = `<div class="usage-role-bar-track"><span class="sup" style="width:${supPct}%"></span></div>
        <p class="section-copy">Supervisor ${supPct}% · Workers/agents ${100 - supPct}% (of attributed tokens)</p>`;
      container.append(bar);
    }
  }

  for (const [title, rows, dimension] of [
    ["Models", usage.models || [], "model"],
    ["Roles", usage.roles || [], "role"],
    ["Sessions", usage.sessions || [], "session_id"],
  ]) {
    container.append(usageTable(title, rows, dimension));
  }
}

function observedModelsSection(observed, missing = []) {
  // P6: secondary collapsed block — honest, not competing with primary tables.
  const section = document.createElement("section");
  section.className = "usage-section observed-models is-secondary";
  const details = document.createElement("details");
  details.className = "observed-models-fold";
  const summary = document.createElement("summary");
  summary.textContent = `Observed executors · ${observed.length}`;
  details.append(summary);
  const note = document.createElement("p");
  note.className = "section-copy";
  note.textContent = "From participants and worker/agent cards. Not usage_records — no token counts invented.";
  details.append(note);
  const list = document.createElement("ul");
  list.className = "observed-model-list";
  const missingSet = new Set(missing);
  for (const row of observed) {
    const item = document.createElement("li");
    item.className = missingSet.has(row.model) ? "is-missing-usage" : "";
    const model = document.createElement("strong");
    model.textContent = row.model;
    const meta = document.createElement("span");
    meta.textContent = [
      (row.roles || []).join("/"),
      (row.sources || []).join("+"),
      missingSet.has(row.model) ? "not in usage" : "",
    ].filter(Boolean).join(" · ");
    item.append(model, meta);
    list.append(item);
  }
  details.append(list);
  section.append(details);
  return section;
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

function focusTurn(anchor, { updateHash = true } = {}) {
  const node = document.getElementById(anchor);
  if (!node) return false;
  if (updateHash && state.currentID) {
    history.replaceState(null, "", turnDeepLinkHash(state.currentID, { id: anchor }));
  }
  node.scrollIntoView({ behavior: preferredScrollBehavior(), block: "start" });
  node.tabIndex = -1;
  node.focus({ preventScroll: true });
  node.classList.add("event-highlight");
  setTimeout(() => node.classList.remove("event-highlight"), 1600);
  return true;
}

function preferredScrollBehavior() {
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth";
}

function focusEvent(eventID) {
  let node = document.getElementById(eventAnchor(eventID));
  if (!node && state.current) {
    const turns = buildHumanTurns(eventsForHumanView(state.current));
    const index = turns.findIndex((turn) => {
      const events = [turn.user, ...(turn.assistantMessages || [])];
      for (const item of turn.activity || []) events.push(item.event, item.result);
      return events.some((event) => event?.event_id === eventID);
    });
    if (index >= 0) {
      const turnNode = document.getElementById(stableTurnAnchor(turns[index], index));
      const worked = turnNode?.querySelector("details.worked-for");
      if (worked) {
        worked.open = true;
        workRevealers.get(worked)?.();
        requestAnimationFrame(() => focusEvent(eventID));
      }
    }
    return;
  }
  if (!node) return;
  const worked = node.closest("details.worked-for");
  if (worked) worked.open = true;
  if (node instanceof HTMLDetailsElement) node.open = true;
  node.scrollIntoView({ behavior: preferredScrollBehavior(), block: "center" });
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
  if (!value || typeof value !== "object") return honestCostLabel(value);
  if ((value.cost_status === "reported" || value.cost_status === "partially_reported")
    && typeof value.reported_cost_usd === "number") {
    const suffix = value.cost_status === "partially_reported" ? " partial" : "";
    return `$${value.reported_cost_usd.toFixed(4)}${suffix}`;
  }
  if (value.cost_status === "reported" && typeof value.cost_usd === "number") {
    return honestCostLabel(value);
  }
  // Never display $0 for unpriced / unknown
  return "Price pending";
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
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) return "—";
  return formatDuration(value);
}

function formatDuration(milliseconds) {
  // P6: unknown / zero duration reads as em dash, not "0ms" dashboard noise.
  if (milliseconds == null || milliseconds === "") return "—";
  const raw = Number(milliseconds);
  if (!Number.isFinite(raw) || raw <= 0) return "—";
  const ms = Math.max(0, raw);
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
