import "./style.css";

import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebglAddon } from "@xterm/addon-webgl";

import {
  Attach,
  ConnectTo,
  CreateSession,
  DefaultAddr,
  Detach,
  GetUsage,
  Input,
  KillSession,
  ListSessions,
  LocalToken,
  RenameSession,
  ResizeSession,
} from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime/runtime";
import type { proto } from "../wailsjs/go/models";

// --- DOM scaffold -----------------------------------------------------

document.getElementById("app")!.innerHTML = `
  <div class="topbar">
    <div class="brand">
      <span class="mark">&gt;_</span>
      <span>multipx</span>
    </div>
    <div class="topbar-right">
      <div class="usage-stat" id="usage-stat"></div>
      <div class="conn-status" id="conn-status">connecting…</div>
      <div class="icon-btn" id="settings-btn" title="Settings">&#9881;</div>
    </div>
  </div>
  <div class="body">
    <div class="sidebar">
      <div class="sidebar-section">MACHINES</div>
      <div class="machine-list" id="machine-list"></div>

      <div class="sidebar-section">SESSIONS</div>
      <div class="session-list" id="session-list"></div>
      <div class="add-row" id="add-row">+ new session</div>

      <div class="sidebar-footer" id="sidebar-footer"></div>
    </div>
    <div class="main">
      <div class="term-stack" id="term-stack">
        <div class="empty-state" id="empty-state">no session attached</div>
      </div>
    </div>
  </div>
  <div class="modal-overlay" id="settings-overlay" style="display:none;">
    <div class="modal">
      <div class="modal-header">
        <span>Settings</span>
        <span class="modal-close" id="settings-close">&times;</span>
      </div>
      <div class="modal-body" id="settings-body"></div>
    </div>
  </div>
`;

const sessionListEl = document.getElementById("session-list")!;
const termStackEl = document.getElementById("term-stack")!;
const emptyStateEl = document.getElementById("empty-state")!;
const connStatusEl = document.getElementById("conn-status")!;
const usageStatEl = document.getElementById("usage-stat")!;
const addRowEl = document.getElementById("add-row")!;
const sidebarFooterEl = document.getElementById("sidebar-footer")!;
const machineListEl = document.getElementById("machine-list")!;
const settingsBtnEl = document.getElementById("settings-btn")!;
const settingsOverlayEl = document.getElementById("settings-overlay")!;
const settingsCloseEl = document.getElementById("settings-close")!;
const settingsBodyEl = document.getElementById("settings-body")!;

const LAST_ADDR_KEY = "multipx.daemonAddr";
const HOSTS_KEY = "multipx.hosts";

interface SavedHost {
  name: string;
  addr: string;
  // Auth token the daemon at addr expects on connect (see
  // daemon.EnsureToken / "multipxd -print-token"). Empty only for hosts
  // saved before auth existed — reconnecting one of those will fail until
  // its token is filled in via Settings.
  token: string;
}

function loadHosts(): SavedHost[] {
  try {
    const raw = localStorage.getItem(HOSTS_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) as Partial<SavedHost>[];
      return parsed.map((h) => ({ name: h.name ?? h.addr ?? "", addr: h.addr ?? "", token: h.token ?? "" }));
    }
  } catch {
    // corrupt localStorage value — fall through to empty list
  }
  return [];
}

function saveHosts() {
  localStorage.setItem(HOSTS_KEY, JSON.stringify(hosts));
}

let hosts: SavedHost[] = loadHosts();
let currentAddr = "";
// This machine's own daemon token, fetched once at startup — lets the
// default/local connection authenticate with zero manual setup. Remote
// hosts have no such shortcut; their token comes from Settings.
let localToken = "";
let defaultAddr = "";

// --- state --------------------------------------------------------------

interface SessionView {
  info: proto.SessionInfo;
  pane: HTMLDivElement; // wrapper: mini-header + host, the flex child in term-stack
  host: HTMLDivElement; // the xterm.js mount point
  term: Terminal;
  fit: FitAddon;
  attached: boolean;
  unsubOutput: () => void;
  unsubClosed: () => void;
}

const sessions = new Map<string, SessionView>();

// Panes currently laid out side-by-side in the main area, left to right.
// A session can be open as a pane or not; "focused" in the sidebar means
// "currently shown as a pane", not a single exclusive selection — Ctrl+D
// splits, so more than one can be true at once.
let panes: string[] = [];
// Which open pane a plain sidebar click replaces, and where keyboard focus
// last landed. Updated on split/open and whenever a pane gains DOM focus.
let activePaneID = "";
let autoCreateTried = false;

function b64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function bytesToB64(bytes: Uint8Array): string {
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}

// --- session lifecycle ----------------------------------------------------

function ensureView(info: proto.SessionInfo): SessionView {
  let v = sessions.get(info.id);
  if (v) {
    v.info = info;
    const title = v.pane.querySelector(".pane-title") as HTMLElement | null;
    if (title) title.textContent = `${info.name}  ·  ${info.shell}`;
    return v;
  }

  const pane = document.createElement("div");
  pane.className = "term-pane";

  const header = document.createElement("div");
  header.className = "pane-mini-header";
  const title = document.createElement("span");
  title.className = "pane-title";
  title.textContent = `${info.name}  ·  ${info.shell}`;
  const closeBtn = document.createElement("span");
  closeBtn.className = "pane-close";
  closeBtn.textContent = "×";
  closeBtn.title = "Close pane (session keeps running)";
  closeBtn.addEventListener("click", () => closePane(info.id));
  header.appendChild(title);
  header.appendChild(closeBtn);

  const host = document.createElement("div");
  host.className = "term-host";

  pane.appendChild(header);
  pane.appendChild(host);

  // Clicking into a pane's terminal makes it "active" — the one a plain
  // sidebar click will swap out, and where Ctrl+D splits from.
  pane.addEventListener("focusin", () => {
    activePaneID = info.id;
  });

  const term = new Terminal({
    fontFamily: '"Geist Mono", ui-monospace, Consolas, monospace',
    fontSize: 13,
    theme: {
      background: "#24272a",
      foreground: "#dddedf",
      cursor: "#00d4d5",
    },
    allowProposedApi: true,
  });
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(host);
  try {
    term.loadAddon(new WebglAddon());
  } catch {
    // WebGL unavailable (e.g. software-only GPU) — xterm falls back to its
    // canvas renderer automatically, still far cheaper than per-cell DOM.
  }

  // Windows Terminal-style clipboard: selecting text copies it immediately
  // (no explicit Ctrl+C, which the shell already owns for SIGINT), and
  // right-click either copies-and-clears an active selection or, with
  // nothing selected, pastes — the native context menu is suppressed
  // globally (see the window "contextmenu" listener below) so this is the
  // only copy/paste path the terminal pane has.
  term.onSelectionChange(() => {
    const sel = term.getSelection();
    if (sel) void navigator.clipboard.writeText(sel).catch(() => {});
  });
  host.addEventListener("contextmenu", (ev) => {
    ev.preventDefault();
    ev.stopPropagation();
    const sel = term.getSelection();
    if (sel) {
      void navigator.clipboard.writeText(sel).catch(() => {});
      term.clearSelection();
      return;
    }
    void navigator.clipboard
      .readText()
      .then((text) => {
        if (text) void Input(info.id, bytesToB64(encoder.encode(text)));
      })
      .catch(() => {});
  });

  const encoder = new TextEncoder();
  term.onData((data) => {
    void Input(info.id, bytesToB64(encoder.encode(data)));
  });

  const unsubOutput = EventsOn(`session:output:${info.id}`, (b64: string) => {
    term.write(b64ToBytes(b64));
  });
  const unsubClosed = EventsOn(`session:closed:${info.id}`, () => {
    term.write("\r\n\x1b[90m[session ended]\x1b[0m\r\n");
  });

  v = { info, pane, host, term, fit, attached: false, unsubOutput, unsubClosed };
  sessions.set(info.id, v);
  return v;
}

/** Drag handle between two side-by-side panes. Dragging pins the LEFT
 * pane to an explicit pixel width (flex: 0 0 Npx); every other pane keeps
 * flex: 1 1 0 and shares whatever space is left, same model most split
 * UIs use for 3+ panes. */
function makeDivider(leftID: string): HTMLDivElement {
  const div = document.createElement("div");
  div.className = "pane-divider";

  div.addEventListener("mousedown", (ev) => {
    ev.preventDefault();
    const left = sessions.get(leftID)?.pane;
    if (!left) return;
    const startX = ev.clientX;
    const startWidth = left.getBoundingClientRect().width;
    document.body.style.cursor = "col-resize";

    const onMove = (me: MouseEvent) => {
      const w = Math.max(160, startWidth + (me.clientX - startX));
      left.style.flex = `0 0 ${w}px`;
    };
    const onUp = () => {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
      document.body.style.cursor = "";
      for (const id of panes) {
        const v = sessions.get(id);
        if (!v) continue;
        v.fit.fit();
        void ResizeSession(id, v.term.cols, v.term.rows);
      }
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  });

  return div;
}

/** Rebuilds the pane row from `panes`, attaching/resizing each as needed. */
async function layoutPanes() {
  emptyStateEl.style.display = panes.length === 0 ? "flex" : "none";

  for (const v of sessions.values()) {
    if (v.pane.isConnected) v.pane.remove();
  }
  termStackEl.querySelectorAll(".pane-divider").forEach((d) => d.remove());

  panes.forEach((id, i) => {
    const v = sessions.get(id);
    if (!v) return;
    termStackEl.appendChild(v.pane);
    if (i < panes.length - 1) termStackEl.appendChild(makeDivider(id));
  });

  // Let the browser compute the new flex widths before measuring.
  await new Promise((r) => requestAnimationFrame(r));

  for (const id of panes) {
    const v = sessions.get(id);
    if (!v) continue;
    v.fit.fit();
    const { cols, rows } = v.term;
    if (!v.attached) {
      v.attached = true;
      await Attach(id, cols, rows);
    } else {
      await ResizeSession(id, cols, rows);
    }
  }
  renderSidebar();
}

/** Plain sidebar click: if `id` is already showing, just focus it.
 * Otherwise swap it into the active pane's slot — an existing split stays
 * a split, it just shows a different session in that one spot. Only when
 * nothing is open yet does this open a lone pane. */
async function openInActivePane(id: string) {
  if (!sessions.has(id)) return;
  if (panes.includes(id)) {
    activePaneID = id;
    sessions.get(id)?.term.focus();
    renderSidebar();
    return;
  }
  if (panes.length === 0) {
    panes = [id];
  } else {
    const idx = panes.indexOf(activePaneID);
    panes[idx === -1 ? panes.length - 1 : idx] = id;
  }
  activePaneID = id;
  await layoutPanes();
  sessions.get(id)?.term.focus();
}

/** Shift-click a sidebar session: add it as a new pane alongside whatever
 * is already open, instead of replacing anything. */
async function addAsPane(id: string) {
  if (!sessions.has(id)) return;
  if (panes.includes(id)) {
    activePaneID = id;
    sessions.get(id)?.term.focus();
    renderSidebar();
    return;
  }
  panes.push(id);
  activePaneID = id;
  await layoutPanes();
  sessions.get(id)?.term.focus();
}

/** Ctrl+D: split by adding a brand new session as another pane. To split
 * with an EXISTING session instead, Shift-click it in the sidebar. */
async function splitPane() {
  const info = await CreateSession("", 80, 24);
  await refreshList();
  await addAsPane(info.id);
}

/** Removes a pane from view without killing its session (it keeps running
 * on the daemon — same "detach" story as every other client here). */
function closePane(id: string) {
  panes = panes.filter((p) => p !== id);
  if (activePaneID === id) activePaneID = panes[panes.length - 1] ?? "";
  void layoutPanes();
}

async function createSession() {
  const info = await CreateSession("", 80, 24);
  await refreshList();
  await openInActivePane(info.id);
}

async function killSession(id: string) {
  const v = sessions.get(id);
  if (v) {
    v.unsubOutput();
    v.unsubClosed();
    v.term.dispose();
    v.pane.remove();
    sessions.delete(id);
  }
  Detach(id);
  await KillSession(id);
  panes = panes.filter((p) => p !== id);
  if (activePaneID === id) activePaneID = panes[panes.length - 1] ?? "";
  await layoutPanes();
  await refreshList();
}

// --- sidebar rendering ----------------------------------------------------

let renamingID = "";

function renderSidebar() {
  sessionListEl.innerHTML = "";
  for (const v of sessions.values()) {
    const row = document.createElement("div");
    row.className = "session-row" + (panes.includes(v.info.id) ? " focused" : "");
    row.title = "Click to switch · Shift-click to split alongside current";

    const dot = document.createElement("span");
    dot.className = "dot " + (v.info.status === "running" ? "running" : "exited");
    row.appendChild(dot);

    if (v.info.id === renamingID) {
      const input = document.createElement("input");
      input.className = "rename-input";
      input.value = v.info.name;
      input.addEventListener("click", (ev) => ev.stopPropagation());
      input.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter") input.blur();
        if (ev.key === "Escape") {
          renamingID = "";
          renderSidebar();
        }
      });
      input.addEventListener("blur", () => {
        const name = input.value.trim();
        renamingID = "";
        if (name && name !== v.info.name) void RenameSession(v.info.id, name).then(refreshList);
        else renderSidebar();
      });
      row.appendChild(input);
      row.addEventListener("click", (ev) => ev.stopPropagation());
      sessionListEl.appendChild(row);
      input.focus();
      input.select();
      continue;
    }

    const name = document.createElement("span");
    name.className = "name";
    name.textContent = v.info.name;
    row.appendChild(name);

    row.addEventListener("click", (ev) => {
      if (ev.shiftKey) void addAsPane(v.info.id);
      else void openInActivePane(v.info.id);
    });
    row.addEventListener("contextmenu", (ev) => {
      ev.preventDefault();
      openContextMenu(ev.clientX, ev.clientY, v.info.id, v.info.name);
    });
    sessionListEl.appendChild(row);
  }
}

// --- context menu -----------------------------------------------------

let menuEl: HTMLDivElement | null = null;

function closeContextMenu() {
  menuEl?.remove();
  menuEl = null;
}

function openContextMenu(x: number, y: number, id: string, name: string) {
  closeContextMenu();
  const menu = document.createElement("div");
  menu.className = "context-menu";
  menu.style.left = `${x}px`;
  menu.style.top = `${y}px`;

  const rename = document.createElement("div");
  rename.className = "item";
  rename.textContent = "Rename";
  rename.addEventListener("click", () => {
    closeContextMenu();
    renamingID = id;
    renderSidebar();
  });

  const close = document.createElement("div");
  close.className = "item danger";
  close.textContent = "Close session";
  close.addEventListener("click", () => {
    closeContextMenu();
    void killSession(id);
  });

  menu.appendChild(rename);
  menu.appendChild(close);
  document.body.appendChild(menu);
  menuEl = menu;

  void name; // reserved for a future "rename '<name>'" label
}

window.addEventListener("click", () => closeContextMenu());
window.addEventListener("blur", () => closeContextMenu());
window.addEventListener("contextmenu", (ev) => {
  // Kill the default WebView2/Chromium menu everywhere except our own —
  // "Reload"/"Inspect" showing up is the single biggest "this is a web
  // page" tell a WebView app can have.
  if (!(ev.target as HTMLElement)?.closest(".session-row")) ev.preventDefault();
});

// --- machines (saved remote/local daemon addresses) -------------------

function renderFooter() {
  sidebarFooterEl.textContent = currentAddr ? `${currentAddr} · protocol v1` : "connecting…";
}

function renderMachines() {
  machineListEl.innerHTML = "";
  for (const h of hosts) {
    const row = document.createElement("div");
    row.className = "session-row" + (h.addr === currentAddr ? " focused" : "");
    row.title = h.addr;

    const dot = document.createElement("span");
    dot.className = "dot " + (h.addr === currentAddr ? "running" : "exited");
    row.appendChild(dot);

    if (h.addr === renamingHostAddr) {
      const input = document.createElement("input");
      input.className = "rename-input";
      input.value = h.name;
      input.addEventListener("click", (ev) => ev.stopPropagation());
      input.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter") input.blur();
        if (ev.key === "Escape") {
          renamingHostAddr = "";
          renderMachines();
        }
      });
      input.addEventListener("blur", () => {
        const name = input.value.trim();
        renamingHostAddr = "";
        if (name) {
          h.name = name;
          saveHosts();
        }
        renderMachines();
      });
      row.appendChild(input);
      row.addEventListener("click", (ev) => ev.stopPropagation());
      machineListEl.appendChild(row);
      input.focus();
      input.select();
      continue;
    }

    const name = document.createElement("span");
    name.className = "name";
    name.textContent = h.name;
    row.appendChild(name);

    row.addEventListener("click", () => {
      if (h.addr !== currentAddr) void connectAndRemember(h.addr, h.token, h.name);
    });
    row.addEventListener("contextmenu", (ev) => {
      ev.preventDefault();
      openMachineMenu(ev.clientX, ev.clientY, h);
    });
    machineListEl.appendChild(row);
  }
}

let renamingHostAddr = "";
let machineMenuEl: HTMLDivElement | null = null;

function closeMachineMenu() {
  machineMenuEl?.remove();
  machineMenuEl = null;
}

function openMachineMenu(x: number, y: number, host: SavedHost) {
  closeMachineMenu();
  const menu = document.createElement("div");
  menu.className = "context-menu";
  menu.style.left = `${x}px`;
  menu.style.top = `${y}px`;

  const rename = document.createElement("div");
  rename.className = "item";
  rename.textContent = "Rename";
  rename.addEventListener("click", () => {
    closeMachineMenu();
    renamingHostAddr = host.addr;
    renderMachines();
  });

  const edit = document.createElement("div");
  edit.className = "item";
  edit.textContent = "Edit";
  edit.addEventListener("click", () => {
    closeMachineMenu();
    openSettings(host.addr);
  });

  const forget = document.createElement("div");
  forget.className = "item danger";
  forget.textContent = "Forget";
  forget.addEventListener("click", () => {
    closeMachineMenu();
    hosts = hosts.filter((h) => h.addr !== host.addr);
    saveHosts();
    renderMachines();
  });

  menu.appendChild(rename);
  menu.appendChild(edit);
  menu.appendChild(forget);
  document.body.appendChild(menu);
  machineMenuEl = menu;
}

window.addEventListener("click", () => closeMachineMenu());

settingsBtnEl.addEventListener("click", () => openSettings());
settingsCloseEl.addEventListener("click", () => closeSettings());
settingsOverlayEl.addEventListener("click", (ev) => {
  if (ev.target === settingsOverlayEl) closeSettings();
});

function openSettings(editAddr?: string) {
  renderSettingsBody(editAddr);
  settingsOverlayEl.style.display = "flex";
}

function closeSettings() {
  settingsOverlayEl.style.display = "none";
}

/** The Settings modal is the one place that handles a host's full
 * identity — name, address, and the auth token the daemon at that
 * address expects (see daemon.EnsureToken / "multipxd -print-token" for
 * a remote machine). The sidebar's rename/forget stay as quick actions;
 * anything touching address or token goes through here. */
function renderSettingsBody(editAddr?: string) {
  settingsBodyEl.innerHTML = "";

  const title = document.createElement("div");
  title.className = "settings-section-title";
  title.textContent = "REMOTE MACHINES";
  settingsBodyEl.appendChild(title);

  for (const h of hosts) {
    if (h.addr === editAddr) {
      settingsBodyEl.appendChild(buildHostEditForm(h));
      continue;
    }

    const row = document.createElement("div");
    row.className = "host-row";

    const info = document.createElement("div");
    info.className = "host-info";
    const name = document.createElement("div");
    name.className = "host-name";
    name.textContent = h.name;
    const meta = document.createElement("div");
    meta.className = "host-meta";
    meta.textContent = `${h.addr} · token ${h.token ? h.token.slice(0, 8) + "…" : "(none — won't connect)"}`;
    info.appendChild(name);
    info.appendChild(meta);
    row.appendChild(info);

    const actions = document.createElement("div");
    actions.className = "host-actions";
    const editBtn = document.createElement("div");
    editBtn.className = "btn";
    editBtn.textContent = "Edit";
    editBtn.addEventListener("click", () => renderSettingsBody(h.addr));
    const removeBtn = document.createElement("div");
    removeBtn.className = "btn danger";
    removeBtn.textContent = "Remove";
    removeBtn.addEventListener("click", () => {
      hosts = hosts.filter((x) => x.addr !== h.addr);
      saveHosts();
      renderMachines();
      renderSettingsBody();
    });
    actions.appendChild(editBtn);
    actions.appendChild(removeBtn);
    row.appendChild(actions);

    settingsBodyEl.appendChild(row);
  }

  settingsBodyEl.appendChild(buildAddHostForm());
}

/** Inline edit form for an existing saved host — same three fields as
 * the add form, pre-filled, replacing that host's row in place. */
function buildHostEditForm(h: SavedHost): HTMLDivElement {
  const wrap = document.createElement("div");
  wrap.className = "host-row";
  wrap.style.flexDirection = "column";
  wrap.style.alignItems = "stretch";

  const nameInput = document.createElement("input");
  nameInput.className = "field-input";
  nameInput.value = h.name;
  nameInput.placeholder = "Name";

  const addrInput = document.createElement("input");
  addrInput.className = "field-input";
  addrInput.value = h.addr;
  addrInput.placeholder = "host:port";
  addrInput.style.marginTop = "6px";

  const tokenInput = document.createElement("input");
  tokenInput.className = "field-input";
  tokenInput.value = h.token;
  tokenInput.placeholder = "token";
  tokenInput.style.marginTop = "6px";

  const hint = document.createElement("div");
  hint.className = "field-hint";
  hint.textContent = 'Fetch this machine\'s token over SSH: ssh <host> "multipxd -print-token"';

  const row = document.createElement("div");
  row.style.display = "flex";
  row.style.gap = "6px";
  row.style.marginTop = "8px";
  const save = document.createElement("div");
  save.className = "btn primary";
  save.textContent = "Save";
  save.addEventListener("click", () => {
    const wasCurrent = h.addr === currentAddr;
    h.name = nameInput.value.trim() || h.addr;
    h.addr = addrInput.value.trim();
    h.token = tokenInput.value.trim();
    saveHosts();
    renderMachines();
    renderSettingsBody();
    if (wasCurrent) void connectAndRemember(h.addr, h.token, h.name);
  });
  const cancel = document.createElement("div");
  cancel.className = "btn";
  cancel.textContent = "Cancel";
  cancel.addEventListener("click", () => renderSettingsBody());
  row.appendChild(save);
  row.appendChild(cancel);

  wrap.appendChild(nameInput);
  wrap.appendChild(addrInput);
  wrap.appendChild(tokenInput);
  wrap.appendChild(hint);
  wrap.appendChild(row);
  return wrap;
}

/** Form for adding a brand-new host. Connecting successfully is what
 * actually saves it (via connectAndRemember) — an address/token that
 * can't authenticate never ends up remembered. */
function buildAddHostForm(): HTMLDivElement {
  const wrap = document.createElement("div");
  wrap.className = "settings-add-form";

  const title = document.createElement("div");
  title.className = "settings-section-title";
  title.textContent = "ADD MACHINE";
  wrap.appendChild(title);

  const nameLabel = document.createElement("div");
  nameLabel.className = "field-label";
  nameLabel.textContent = "Name";
  const nameInput = document.createElement("input");
  nameInput.className = "field-input";
  nameInput.placeholder = "e.g. linuxhome";

  const addrLabel = document.createElement("div");
  addrLabel.className = "field-label";
  addrLabel.textContent = "Address";
  const addrInput = document.createElement("input");
  addrInput.className = "field-input";
  addrInput.placeholder = "host:port";

  const tokenLabel = document.createElement("div");
  tokenLabel.className = "field-label";
  tokenLabel.textContent = "Token";
  const tokenInput = document.createElement("input");
  tokenInput.className = "field-input";
  tokenInput.placeholder = "paste token here";

  const hint = document.createElement("div");
  hint.className = "field-hint";
  hint.textContent = 'Fetch this machine\'s token over SSH: ssh <host> "multipxd -print-token", then paste it above.';

  const errEl = document.createElement("div");
  errEl.className = "field-hint";
  errEl.style.color = "var(--error)";

  const addBtn = document.createElement("div");
  addBtn.className = "btn primary";
  addBtn.style.marginTop = "10px";
  addBtn.textContent = "Add & Connect";
  addBtn.addEventListener("click", async () => {
    const addr = addrInput.value.trim();
    const token = tokenInput.value.trim();
    if (!addr) {
      errEl.textContent = "Address is required.";
      return;
    }
    errEl.textContent = "connecting…";
    const ok = await connectAndRemember(addr, token, nameInput.value.trim() || addr);
    if (ok) {
      closeSettings();
    } else {
      errEl.textContent = "Couldn't connect — check the address and token.";
    }
  });

  wrap.appendChild(nameLabel);
  wrap.appendChild(nameInput);
  wrap.appendChild(addrLabel);
  wrap.appendChild(addrInput);
  wrap.appendChild(tokenLabel);
  wrap.appendChild(tokenInput);
  wrap.appendChild(hint);
  wrap.appendChild(errEl);
  wrap.appendChild(addBtn);
  return wrap;
}

/** Tears down every session view (they belong to the OLD daemon) and
 * connects to a new one, local or remote. Sessions themselves aren't
 * touched — they keep running wherever they are; this just changes which
 * daemon's sessions this window is looking at. */
async function switchDaemon(addr: string, token: string): Promise<boolean> {
  for (const v of sessions.values()) {
    v.unsubOutput();
    v.unsubClosed();
    v.term.dispose();
    v.pane.remove();
  }
  sessions.clear();
  panes = [];
  activePaneID = "";
  autoCreateTried = false;
  emptyStateEl.style.display = "flex";
  renderSidebar();

  try {
    await ConnectTo(addr, token);
  } catch (e) {
    connStatusEl.textContent = "daemon unreachable";
    connStatusEl.classList.add("error");
    sidebarFooterEl.textContent = `${addr} · unreachable`;
    return false;
  }
  localStorage.setItem(LAST_ADDR_KEY, addr);
  currentAddr = addr;
  renderFooter();
  renderMachines();
  connStatusEl.textContent = "connected";
  connStatusEl.classList.remove("error");
  void pollUsage();
  await refreshList();
  return true;
}

/** Connects to addr and remembers it (with its token) in the saved
 * machines list — the path every UI entry point (sidebar click, Settings
 * add/edit) goes through, so successfully connecting anywhere also means
 * "remembered". */
async function connectAndRemember(addr: string, token: string, name?: string): Promise<boolean> {
  const ok = await switchDaemon(addr, token);
  if (!ok) return false;
  const existing = hosts.find((h) => h.addr === addr);
  if (existing) {
    if (name) existing.name = name;
    existing.token = token;
  } else {
    hosts.push({ name: name || addr, addr, token });
  }
  saveHosts();
  renderMachines();
  return true;
}

async function refreshList() {
  let list: proto.SessionInfo[];
  try {
    list = await ListSessions();
    connStatusEl.textContent = "connected";
    connStatusEl.classList.remove("error");
  } catch (e) {
    connStatusEl.textContent = "daemon unreachable";
    connStatusEl.classList.add("error");
    return;
  }

  const seen = new Set<string>();
  for (const info of list) {
    seen.add(info.id);
    ensureView(info);
  }
  for (const id of Array.from(sessions.keys())) {
    if (!seen.has(id)) {
      sessions.get(id)?.pane.remove();
      sessions.delete(id); // killed elsewhere
    }
  }
  panes = panes.filter((id) => seen.has(id));
  if (!panes.includes(activePaneID)) activePaneID = panes[panes.length - 1] ?? "";

  if (list.length === 0 && !autoCreateTried) {
    autoCreateTried = true;
    await createSession();
    return;
  }
  if (panes.length === 0 && list.length > 0) {
    await openInActivePane(list[0].id);
    return;
  }
  renderSidebar();
}

// --- wiring ----------------------------------------------------------

addRowEl.addEventListener("click", () => void createSession());

window.addEventListener("keydown", (ev) => {
  if (!ev.ctrlKey) return;
  const key = ev.key.toLowerCase();
  if (key === "n") {
    ev.preventDefault();
    void createSession();
  } else if (key === "d") {
    ev.preventDefault();
    void splitPane();
  }
});

// The OS fires `resize` on every intermediate frame of a window drag, not
// just the final size. Committing each one means a SIGWINCH (and an
// xterm.js reflow) at whatever transient size the window happened to be
// at that instant — full-screen TUIs that redraw a fixed region via
// absolute cursor positioning get desynced by the flood and print fresh
// frames instead of overwriting in place, which looks like "static"
// content scrolling away. Debounce to the settled size only.
let resizeDebounce: number | undefined;
window.addEventListener("resize", () => {
  if (resizeDebounce !== undefined) window.clearTimeout(resizeDebounce);
  resizeDebounce = window.setTimeout(() => {
    for (const id of panes) {
      const v = sessions.get(id);
      if (!v) continue;
      v.fit.fit();
      void ResizeSession(id, v.term.cols, v.term.rows);
    }
  }, 120);
});

function formatBytes(n: number): string {
  const gb = n / 1024 / 1024 / 1024;
  if (gb >= 1) return `${gb.toFixed(1)}GB`;
  return `${Math.round(n / 1024 / 1024)}MB`;
}

async function pollUsage() {
  try {
    const u = await GetUsage();
    usageStatEl.textContent = `${u.hostname} · ${u.cpu_percent.toFixed(0)}% CPU · ${formatBytes(u.mem_used_bytes)}/${formatBytes(u.mem_total_bytes)}`;
    usageStatEl.classList.remove("error");
  } catch {
    usageStatEl.textContent = "";
  }
}

(async () => {
  defaultAddr = await DefaultAddr();
  try {
    localToken = await LocalToken();
  } catch {
    // Local daemon's token file couldn't be read/created — the default
    // connection below will fail with "unauthorized" rather than silently
    // connecting unauthenticated, which is the right failure mode.
  }

  if (hosts.length === 0) {
    hosts.push({ name: "This machine", addr: defaultAddr, token: localToken });
    saveHosts();
  }
  renderMachines();

  const addr = localStorage.getItem(LAST_ADDR_KEY) || defaultAddr;
  const saved = hosts.find((h) => h.addr === addr);
  // A host saved before auth existed (or the default entry, which is
  // always this machine) has no token of its own yet — fall back to the
  // local token only when addr actually IS this machine's default;
  // otherwise an empty token is correct and the connection should fail.
  const token = saved?.token || (addr === defaultAddr ? localToken : "");
  await connectAndRemember(addr, token);
  await pollUsage();
  setInterval(refreshList, 2000);
  setInterval(pollUsage, 3000);
})();
