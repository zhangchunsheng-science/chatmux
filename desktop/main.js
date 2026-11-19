// chatmux desktop shell: loads the gateway's dashboard and surfaces new
// console replies as system notifications. Thin on purpose — everything
// real stays in the Go binary.
//
// Run:  CHATMUX_URL=http://127.0.0.1:8080 CHATMUX_TOKEN=xxx npm start
// Build: npm install && npm run dist   (needs a desktop environment)
const { app, BrowserWindow, Notification, Tray, Menu, nativeImage } = require("electron");
const path = require("path");

const BASE = process.env.CHATMUX_URL || "http://127.0.0.1:8080";
const TOKEN = process.env.CHATMUX_TOKEN || "";
const POLL_MS = 3000;

let win = null;
let tray = null;
let cursor = 0;
let unread = 0;
let booted = false; // don't notify about pre-existing history

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on("second-instance", () => {
    if (win) {
      if (win.isMinimized()) win.restore();
      win.show();
      win.focus();
    }
  });

  app.whenReady().then(() => {
    win = new BrowserWindow({
      width: 1024,
      height: 768,
      title: "chatmux",
      webPreferences: { preload: path.join(__dirname, "preload.js") },
    });
    win.loadURL(BASE + "/");
    win.on("closed", () => {
      win = null;
    });
    setupTray();
    pollLoop();
    app.on("activate", () => {
      if (BrowserWindow.getAllWindows().length === 0) win = makeWindow();
    });
  });

  app.on("window-all-closed", () => {
    if (process.platform !== "darwin") app.quit();
  });
}

function makeWindow() {
  const w = new BrowserWindow({
    width: 1024,
    height: 768,
    title: "chatmux",
    webPreferences: { preload: path.join(__dirname, "preload.js") },
  });
  w.loadURL(BASE + "/");
  return w;
}

function setupTray() {
  try {
    // 16x16 solid-color template-ish icon; replace desktop/icon.png for dist
    const img = nativeImage.createFromPath(path.join(__dirname, "icon.png"));
    if (img.isEmpty()) return;
    tray = new Tray(img);
    tray.setToolTip("chatmux");
    tray.setContextMenu(
      Menu.buildFromTemplate([
        { label: "显示", click: () => (win ? win.show() : (win = makeWindow())) },
        { type: "separator" },
        { label: "退出", click: () => app.quit() },
      ])
    );
  } catch {
    tray = null; // tray is best-effort; the window alone is enough
  }
}

async function pollLoop() {
  for (;;) {
    await new Promise((r) => setTimeout(r, POLL_MS));
    try {
      const res = await fetch(BASE + "/admin/chat?since=" + cursor, {
        headers: { "X-Admin-Token": TOKEN },
      });
      if (!res.ok) continue;
      const data = await res.json();
      if (Array.isArray(data.entries) && data.entries.length > 0) {
        for (const e of data.entries) {
          if (booted && e.from === "bot" && e.text) {
            notify(String(e.text));
          }
        }
        cursor = data.cursor || cursor;
      }
      booted = true; // first successful poll only establishes the baseline
    } catch {
      // gateway down: keep polling, notify nothing
    }
  }
}

function notify(text) {
  if (!Notification.isSupported()) return;
  const n = new Notification({
    title: "chatmux",
    body: text.length > 120 ? text.slice(0, 120) + "…" : text,
  });
  n.on("click", () => {
    if (win) {
      win.show();
      win.focus();
    }
  });
  n.show();
  if (typeof app.dock !== "undefined" && app.dock) {
    app.dock.setBadgeCount(unread + 1);
    unread += 1;
    win && win.once("focus", () => {
      unread = 0;
      app.dock.setBadgeCount(0);
    });
  }
}
