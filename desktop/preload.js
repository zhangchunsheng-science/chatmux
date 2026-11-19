// preload: expose a minimal, safe bridge (no node access to the page).
const { contextBridge } = require("electron");

contextBridge.exposeInMainWorld("chatmuxDesktop", {
  desktop: true,
});
