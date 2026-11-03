(() => {
  "use strict";

  const ttydFrame = document.getElementById("ttyd-frame");
  const terminalWrap = document.getElementById("terminal-wrap");
  const scrollBottomBtn = document.getElementById("scroll-bottom-btn");
  const folderSelect = document.getElementById("folder-select");
  const clearConvBtn = document.getElementById("clear-conv-btn");
  const restartBtn = document.getElementById("restart-btn");
  const interruptBtn = document.getElementById("interrupt-btn");
  const sendNowBtn = document.getElementById("send-now-btn");
  const toastEl = document.getElementById("toast");
  const authOverlay = document.getElementById("auth-overlay");
  const tokenInput = document.getElementById("token-input");
  const tokenSaveBtn = document.getElementById("token-save-btn");
  const btnSlash = document.getElementById("btn-slash");
  const btnTab = document.getElementById("btn-tab");
  const btnEsc = document.getElementById("btn-esc");
  const btnLeft = document.getElementById("btn-left");
  const btnUp = document.getElementById("btn-up");
  const btnDown = document.getElementById("btn-down");
  const btnRight = document.getElementById("btn-right");
  const btnEnter = document.getElementById("btn-enter");

  // ---------------------------------------------------------------------------
  // 1. Token & Cookie Bootstrap (Before Loading /terminal/)
  // ---------------------------------------------------------------------------
  function readCookieToken() {
    const match = document.cookie.match(/(?:^|;\s*)agy_token=([^;]*)/);
    return match ? decodeURIComponent(match[1]) : "";
  }

  function persistToken(token) {
    if (!token) return;
    localStorage.setItem("agy_token", token);
    document.cookie =
      "agy_token=" +
      encodeURIComponent(token) +
      "; path=/; max-age=31536000; SameSite=Lax";
  }

  function getToken() {
    return localStorage.getItem("agy_token") || readCookieToken() || "";
  }

  const params = new URLSearchParams(window.location.search);
  const urlToken = params.get("token");
  if (urlToken) {
    persistToken(urlToken.trim());
    params.delete("token");
    const cleanSearch = params.toString();
    const cleanUrl =
      window.location.pathname + (cleanSearch ? "?" + cleanSearch : "");
    window.history.replaceState({}, "", cleanUrl);
  } else {
    const existingToken = getToken();
    if (existingToken) {
      persistToken(existingToken);
    }
  }

  // ---------------------------------------------------------------------------
  // 2. Same-Origin Terminal Loading & Active Freeze Watchdog
  // ---------------------------------------------------------------------------
  let lastPanePid = null;
  let ttydLoaded = false;
  let lastReloadAt = 0;
  let trackedSockets = [];
  let wsClosedSince = 0;
  let hookBurstTimer = null;

  function hookIframeWebSocket() {
    try {
      const win = ttydFrame.contentWindow;
      if (!win || win.location.href === "about:blank") return;
      if (win.__agyWsHooked) return;
      const NativeWebSocket = win.WebSocket;
      if (!NativeWebSocket) return;

      function WrappedWebSocket(url, protocols) {
        const ws =
          protocols !== undefined
            ? new NativeWebSocket(url, protocols)
            : new NativeWebSocket(url);
        trackedSockets.push(ws);
        wsClosedSince = 0;

        ws.addEventListener("open", () => {
          wsClosedSince = 0;
        });

        const markClosed = () => {
          if (wsClosedSince === 0) {
            wsClosedSince = Date.now();
          }
        };
        ws.addEventListener("close", markClosed);
        ws.addEventListener("error", markClosed);
        return ws;
      }

      WrappedWebSocket.prototype = NativeWebSocket.prototype;
      WrappedWebSocket.CONNECTING = NativeWebSocket.CONNECTING;
      WrappedWebSocket.OPEN = NativeWebSocket.OPEN;
      WrappedWebSocket.CLOSING = NativeWebSocket.CLOSING;
      WrappedWebSocket.CLOSED = NativeWebSocket.CLOSED;

      win.WebSocket = WrappedWebSocket;
      win.__agyWsHooked = true;
    } catch (_) {
      // Ignore transient navigation errors before /terminal/ commits
    }
  }

  // ---------------------------------------------------------------------------
  // 2b. Touch Scrolling Engine for Mobile Terminal View
  // ---------------------------------------------------------------------------
  let isCopyMode = false;
  let isUserDragging = false;
  let scrollOffset = 0;
  let cancelTerminalMomentum = () => {};

  function updateScrollBottomBtn(visible) {
    if (!scrollBottomBtn) return;
    if (visible) {
      scrollBottomBtn.classList.add("visible");
    } else {
      scrollBottomBtn.classList.remove("visible");
    }
  }

  function dispatchTerminalWheel(deltaY, clientX, clientY) {
    try {
      const win = ttydFrame.contentWindow;
      if (!win || win.location.href === "about:blank") return false;
      const doc = win.document;
      const term = win.term;
      const target =
        term && term.element
          ? term.element
          : (doc && doc.querySelector(".xterm, .xterm-screen, canvas")) ||
            (doc && doc.body);
      if (!target) return false;

      const rect = target.getBoundingClientRect();
      const cx =
        clientX !== undefined && clientX > 0 && clientX < win.innerWidth
          ? clientX
          : rect.left + rect.width / 2;
      const cy =
        clientY !== undefined && clientY > 0 && clientY < win.innerHeight
          ? clientY
          : rect.top + rect.height / 2;

      const evt = new win.WheelEvent("wheel", {
        deltaY: deltaY,
        clientX: cx,
        clientY: cy,
        deltaMode: win.WheelEvent.DOM_DELTA_PIXEL,
        bubbles: true,
        cancelable: true,
      });
      target.dispatchEvent(evt);
      return true;
    } catch (_) {
      return false;
    }
  }

  function hookIframeTouchScroll() {
    try {
      const win = ttydFrame.contentWindow;
      if (!win || win.location.href === "about:blank") return;
      if (win.__agyTouchScrollHooked) return;
      const doc = win.document;
      if (!doc) return;

      if (doc.body) {
        doc.body.style.touchAction = "none";
      }

      let startX = 0;
      let startY = 0;
      let lastX = 0;
      let lastY = 0;
      let lastTime = 0;
      let velocityY = 0;
      let accumulatedDeltaY = 0;
      let isScrolling = false;
      let momentumTimer = null;

      function stopMomentum() {
        if (momentumTimer) {
          cancelAnimationFrame(momentumTimer);
          momentumTimer = null;
        }
      }
      cancelTerminalMomentum = stopMomentum;

      const STEP = 20;

      function onTouchStart(e) {
        stopMomentum();
        if (e.touches.length !== 1) {
          isScrolling = false;
          return;
        }
        const t = e.touches[0];
        startX = t.clientX;
        startY = t.clientY;
        lastX = t.clientX;
        lastY = t.clientY;
        lastTime = Date.now();
        velocityY = 0;
        accumulatedDeltaY = 0;
        isScrolling = false;
      }

      function onTouchMove(e) {
        if (e.touches.length !== 1) return;
        const t = e.touches[0];
        const diffY = lastY - t.clientY;
        const diffX = lastX - t.clientX;
        const totalDistY = Math.abs(t.clientY - startY);
        const totalDistX = Math.abs(t.clientX - startX);

        if (!isScrolling) {
          if (totalDistY > 6 && totalDistY > totalDistX * 1.1) {
            isScrolling = true;
            isUserDragging = true;
          } else if (totalDistX > 12) {
            return;
          }
        }

        if (isScrolling) {
          e.preventDefault();
          e.stopPropagation();

          const now = Date.now();
          const dt = now - lastTime;
          if (dt > 0) {
            const currentVel = (lastY - t.clientY) / dt;
            velocityY = 0.7 * velocityY + 0.3 * currentVel;
          }
          lastTime = now;
          lastX = t.clientX;
          lastY = t.clientY;

          accumulatedDeltaY += diffY;
          while (Math.abs(accumulatedDeltaY) >= STEP) {
            if (accumulatedDeltaY > 0) {
              // Dragged finger UP -> push content up -> scroll DOWN in history
              dispatchTerminalWheel(100, t.clientX, t.clientY);
              accumulatedDeltaY -= STEP;
              scrollOffset = Math.max(0, scrollOffset - 1);
            } else {
              // Dragged finger DOWN -> pull content down -> scroll UP in history
              dispatchTerminalWheel(-100, t.clientX, t.clientY);
              accumulatedDeltaY += STEP;
              scrollOffset += 1;
            }
          }

          if (scrollOffset > 0) {
            updateScrollBottomBtn(true);
          } else {
            scrollOffset = 0;
            isCopyMode = false;
            updateScrollBottomBtn(false);
          }
        }
      }

      function onTouchEnd() {
        isUserDragging = false;
        if (!isScrolling) return;
        isScrolling = false;

        if (Math.abs(velocityY) > 0.35) {
          let currentVel = velocityY;
          let momentumDelta = 0;
          const decay = 0.92;
          const minVel = 0.08;

          function stepMomentum() {
            currentVel *= decay;
            if (Math.abs(currentVel) < minVel) {
              stopMomentum();
              return;
            }
            momentumDelta += currentVel * 16;
            while (Math.abs(momentumDelta) >= STEP) {
              if (momentumDelta > 0) {
                dispatchTerminalWheel(100, lastX, lastY);
                momentumDelta -= STEP;
                scrollOffset = Math.max(0, scrollOffset - 1);
              } else {
                dispatchTerminalWheel(-100, lastX, lastY);
                momentumDelta += STEP;
                scrollOffset += 1;
              }
            }
            if (scrollOffset > 0) {
              updateScrollBottomBtn(true);
            } else if (scrollOffset === 0 && !isCopyMode) {
              updateScrollBottomBtn(false);
            }
            momentumTimer = requestAnimationFrame(stepMomentum);
          }
          momentumTimer = requestAnimationFrame(stepMomentum);
        }
      }

      win.addEventListener("touchstart", onTouchStart, {
        capture: true,
        passive: false,
      });
      win.addEventListener("touchmove", onTouchMove, {
        capture: true,
        passive: false,
      });
      win.addEventListener("touchend", onTouchEnd, {
        capture: true,
        passive: false,
      });
      win.addEventListener("touchcancel", onTouchEnd, {
        capture: true,
        passive: false,
      });

      win.__agyTouchScrollHooked = true;
    } catch (_) {}
  }

  function hookIframe() {
    hookIframeWebSocket();
    hookIframeTouchScroll();
  }

  // Parent terminalWrap touch listener (fallback for edge touches)
  if (terminalWrap) {
    let wrapStartY = 0;
    let wrapLastY = 0;
    let wrapAccum = 0;
    let wrapScrolling = false;

    terminalWrap.addEventListener(
      "touchstart",
      (e) => {
        if (e.target === scrollBottomBtn) return;
        if (e.touches.length !== 1) return;
        wrapStartY = e.touches[0].clientY;
        wrapLastY = wrapStartY;
        wrapAccum = 0;
        wrapScrolling = false;
      },
      { passive: false }
    );

    terminalWrap.addEventListener(
      "touchmove",
      (e) => {
        if (e.target === scrollBottomBtn) return;
        if (e.touches.length !== 1) return;
        const t = e.touches[0];
        const diffY = wrapLastY - t.clientY;
        const totalY = Math.abs(t.clientY - wrapStartY);

        if (!wrapScrolling && totalY > 6) {
          wrapScrolling = true;
          isUserDragging = true;
        }

        if (wrapScrolling) {
          e.preventDefault();
          wrapAccum += diffY;
          wrapLastY = t.clientY;
          const STEP = 20;
          while (Math.abs(wrapAccum) >= STEP) {
            if (wrapAccum > 0) {
              dispatchTerminalWheel(100, t.clientX, t.clientY);
              wrapAccum -= STEP;
              scrollOffset = Math.max(0, scrollOffset - 1);
            } else {
              dispatchTerminalWheel(-100, t.clientX, t.clientY);
              wrapAccum += STEP;
              scrollOffset += 1;
            }
          }
          if (scrollOffset > 0) {
            updateScrollBottomBtn(true);
          } else {
            scrollOffset = 0;
            isCopyMode = false;
            updateScrollBottomBtn(false);
          }
        }
      },
      { passive: false }
    );

    terminalWrap.addEventListener("touchend", () => {
      wrapScrolling = false;
      isUserDragging = false;
    });
  }

  async function scrollToBottom() {
    haptic(16);
    cancelTerminalMomentum();
    scrollOffset = 0;
    isCopyMode = false;
    updateScrollBottomBtn(false);
    await sendKeyAction("ScrollBottom");
  }

  if (scrollBottomBtn) {
    scrollBottomBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      scrollToBottom();
    });
    scrollBottomBtn.addEventListener("pointerdown", (e) => {
      e.stopPropagation();
      e.preventDefault();
    });
  }

  function loadTtyd(force = false) {
    const now = Date.now();
    if (!force && now - lastReloadAt < 1200) {
      return;
    }
    lastReloadAt = now;
    ttydLoaded = true;
    trackedSockets = [];
    wsClosedSince = 0;
    ttydFrame.src = "/terminal/?t=" + now;

    // Hook contentWindow.WebSocket and touch scroll as soon as navigation commits
    if (hookBurstTimer) clearInterval(hookBurstTimer);
    let ticks = 0;
    hookBurstTimer = setInterval(() => {
      hookIframe();
      ticks += 1;
      if (ticks > 100) {
        clearInterval(hookBurstTimer);
        hookBurstTimer = null;
      }
    }, 25);
  }

  ttydFrame.addEventListener("load", () => {
    hookIframe();
  });

  function isTerminalSocketHealthy() {
    if (!ttydLoaded) return false;
    let doc = null;
    try {
      const win = ttydFrame.contentWindow;
      if (!win || win.location.href === "about:blank") {
        return false;
      }
      hookIframe();
      doc = ttydFrame.contentDocument;
    } catch (_) {
      return false;
    }

    if (trackedSockets.length > 0) {
      const latestWs = trackedSockets[trackedSockets.length - 1];
      if (latestWs && (latestWs.readyState === 0 || latestWs.readyState === 1)) {
        wsClosedSince = 0;
        return true;
      }
      // readyState === 2 (CLOSING) or 3 (CLOSED)
      if (wsClosedSince === 0) {
        wsClosedSince = Date.now();
      }
      return false;
    }

    // If no WebSocket was captured yet, allow initial load grace period, then verify .xterm exists
    const age = Date.now() - lastReloadAt;
    if (age < 5000) {
      return true;
    }
    if (doc && doc.querySelector(".xterm, canvas")) {
      return true;
    }
    return false;
  }

  // Periodic 3-second same-origin WebSocket & canvas freeze watchdog
  setInterval(() => {
    if (document.visibilityState !== "visible") return;
    if (authOverlay.classList.contains("visible")) return;
    if (!getToken()) return;

    hookIframe();

    if (!isTerminalSocketHealthy()) {
      const closedDuration =
        wsClosedSince > 0 ? Date.now() - wsClosedSince : Date.now() - lastReloadAt;
      if (closedDuration > 1500) {
        loadTtyd(true);
      }
    }
  }, 3000);

  // Immediately load /terminal/ if token is already initialized
  if (getToken()) {
    loadTtyd(true);
  }

  // Wake-up & page restoration handlers for mobile lock/unlock
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") {
      checkStatus();
      if (!isTerminalSocketHealthy()) {
        loadTtyd(true);
      }
    }
  });

  window.addEventListener("pageshow", (e) => {
    if (e.persisted || !isTerminalSocketHealthy()) {
      loadTtyd(true);
    }
    checkStatus();
  });

  // ---------------------------------------------------------------------------
  // 3. Toast & Haptic Feedback Helpers
  // ---------------------------------------------------------------------------
  let toastTimer = null;
  function showToast(msg) {
    toastEl.textContent = msg;
    toastEl.classList.add("visible");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => {
      toastEl.classList.remove("visible");
    }, 1500);
  }

  function haptic(ms = 14) {
    if (navigator.vibrate) {
      try {
        navigator.vibrate(ms);
      } catch (_) {}
    }
  }

  // ---------------------------------------------------------------------------
  // 4. Authenticated REST API Helper & Manual Token Entry
  // ---------------------------------------------------------------------------
  async function apiRequest(path, method = "GET", body = null) {
    const token = getToken();
    const headers = {};
    if (token) {
      headers["X-AGY-Token"] = token;
    }
    if (body !== null) {
      headers["Content-Type"] = "application/json";
    }
    const resp = await fetch(path, {
      method,
      headers,
      credentials: "same-origin",
      body: body !== null ? JSON.stringify(body) : null,
    });
    if (resp.status === 401) {
      authOverlay.classList.add("visible");
      throw new Error("Unauthorized");
    }
    authOverlay.classList.remove("visible");
    return resp.json();
  }

  tokenSaveBtn.addEventListener("click", async () => {
    const val = tokenInput.value.trim();
    if (!val) return;
    persistToken(val);
    await checkStatus();
    loadTtyd(true);
  });

  tokenInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      tokenSaveBtn.click();
    }
  });

  // ---------------------------------------------------------------------------
  // 5. Top Action Buttons (Interrupt, Send Now, /clear)
  // ---------------------------------------------------------------------------
  async function sendKeyAction(action, successToast = "") {
    haptic(12);
    if (
      action === "Escape" ||
      action === "Interrupt" ||
      action === "ScrollBottom"
    ) {
      cancelTerminalMomentum();
      scrollOffset = 0;
      isCopyMode = false;
      updateScrollBottomBtn(false);
    }
    try {
      const res = await apiRequest("/api/key", "POST", { action });
      if (res.ok) {
        if (
          action === "Escape" ||
          action === "Interrupt" ||
          action === "ScrollBottom"
        ) {
          scrollOffset = 0;
          isCopyMode = false;
          updateScrollBottomBtn(false);
        }
        if (successToast) {
          showToast(successToast);
        }
      } else {
        showToast("Error: " + (res.error || "key failed"));
      }
    } catch (err) {
      if (err.message !== "Unauthorized") {
        showToast("Connection error");
      }
    }
  }

  // Prevent button taps from stealing focus from the terminal iframe
  [
    interruptBtn,
    sendNowBtn,
    clearConvBtn,
    restartBtn,
    btnSlash,
    btnTab,
    btnEsc,
    btnLeft,
    btnUp,
    btnDown,
    btnRight,
    btnEnter,
  ].forEach((btn) => {
    if (!btn) return;
    btn.addEventListener("pointerdown", (e) => {
      e.preventDefault();
    });
    btn.addEventListener("mousedown", (e) => {
      e.preventDefault();
    });
  });

  if (interruptBtn) {
    interruptBtn.addEventListener("click", () => {
      sendKeyAction("Interrupt", "Interrupted ✓");
    });
  }

  if (sendNowBtn) {
    sendNowBtn.addEventListener("click", () => {
      sendKeyAction("Enter", "Sent Enter ✓");
    });
  }

  clearConvBtn.addEventListener("click", async () => {
    haptic(16);
    scrollOffset = 0;
    isCopyMode = false;
    updateScrollBottomBtn(false);
    clearConvBtn.disabled = true;
    try {
      const res = await apiRequest("/api/clear", "POST", {});
      if (res.ok) {
        showToast("Sent /clear ✓");
      } else {
        showToast("Error: " + (res.error || "failed"));
      }
    } catch (err) {
      if (err.message !== "Unauthorized") {
        showToast("Connection error");
      }
    } finally {
      clearConvBtn.disabled = false;
    }
  });

  let currentWorkdir = "";
  let lastFoldersKey = "";

  function syncFolderSelect(folders, workdir) {
    if (!folderSelect || !Array.isArray(folders)) return;
    if (document.activeElement === folderSelect) return;

    const foldersKey = folders.map((f) => f.path).join("|");
    if (foldersKey !== lastFoldersKey) {
      lastFoldersKey = foldersKey;
      folderSelect.innerHTML = "";
      folders.forEach((f) => {
        const opt = document.createElement("option");
        opt.value = f.path;
        opt.textContent = "📁 " + f.name;
        folderSelect.appendChild(opt);
      });
    }

    if (workdir) {
      currentWorkdir = workdir;
      folderSelect.value = workdir;
    }
  }

  if (folderSelect) {
    folderSelect.addEventListener("change", async () => {
      const targetFolder = folderSelect.value;
      folderSelect.blur();
      if (!targetFolder) return;

      const selectedOpt = folderSelect.options[folderSelect.selectedIndex];
      const label = selectedOpt
        ? selectedOpt.textContent.replace(/^📁\s*/, "")
        : targetFolder;

      haptic(25);
      folderSelect.disabled = true;
      if (restartBtn) restartBtn.disabled = true;
      showToast("Restarting in " + label + "...");

      try {
        const res = await apiRequest("/api/restart", "POST", {
          folder: targetFolder,
        });
        if (res.ok) {
          currentWorkdir = res.workdir || targetFolder;
          setTimeout(() => {
            loadTtyd(true);
            checkStatus();
            showToast("Working in " + label + " ✓");
          }, 500);
        } else {
          if (currentWorkdir) folderSelect.value = currentWorkdir;
          showToast("Error: " + (res.error || "failed"));
        }
      } catch (err) {
        if (currentWorkdir) folderSelect.value = currentWorkdir;
        if (err.message !== "Unauthorized") {
          showToast("Connection error");
        }
      } finally {
        setTimeout(() => {
          folderSelect.disabled = false;
          if (restartBtn) restartBtn.disabled = false;
        }, 700);
      }
    });
  }

  if (restartBtn) {
    restartBtn.addEventListener("click", async () => {
      haptic(25);
      restartBtn.disabled = true;
      if (folderSelect) folderSelect.disabled = true;
      showToast("Restarting agy session...");
      try {
        const res = await apiRequest("/api/restart", "POST", {
          folder: folderSelect ? folderSelect.value : "",
        });
        if (res.ok) {
          setTimeout(() => {
            loadTtyd(true);
            checkStatus();
            showToast("Session restarted ✓");
          }, 600);
        } else {
          showToast("Error: " + (res.error || "failed"));
        }
      } catch (err) {
        if (err.message !== "Unauthorized") {
          showToast("Connection error");
        }
      } finally {
        setTimeout(() => {
          restartBtn.disabled = false;
          if (folderSelect) folderSelect.disabled = false;
        }, 800);
      }
    });
  }

  // ---------------------------------------------------------------------------
  // 6. Bottom Navigation & Key Controls (/, tab, esc, ←, ↑, ↓, →, ↵)
  // ---------------------------------------------------------------------------
  if (btnSlash) {
    btnSlash.addEventListener("click", () => {
      sendKeyAction("/", "Sent / ✓");
    });
  }

  if (btnTab) {
    btnTab.addEventListener("click", () => {
      sendKeyAction("Tab");
    });
  }

  if (btnEsc) {
    btnEsc.addEventListener("click", () => {
      sendKeyAction("Escape");
    });
  }

  if (btnLeft) {
    btnLeft.addEventListener("click", () => {
      sendKeyAction("Left");
    });
  }

  if (btnUp) {
    btnUp.addEventListener("click", () => {
      sendKeyAction("Up");
    });
  }

  if (btnDown) {
    btnDown.addEventListener("click", () => {
      sendKeyAction("Down");
    });
  }

  if (btnRight) {
    btnRight.addEventListener("click", () => {
      sendKeyAction("Right");
    });
  }

  if (btnEnter) {
    btnEnter.addEventListener("click", () => {
      sendKeyAction("Enter", "Sent Return ✓");
    });
  }

  // ---------------------------------------------------------------------------
  // 7. Periodic Status Check & Pane PID Change Detection
  // ---------------------------------------------------------------------------
  async function checkStatus() {
    try {
      const data = await apiRequest("/api/status");
      if (data.ok) {
        syncFolderSelect(data.folders, data.workdir);

        if (!ttydLoaded || !isTerminalSocketHealthy()) {
          loadTtyd(true);
        }

        if (data.pane_pid) {
          lastPanePid = data.pane_pid;
        }

        if (data.in_copy_mode !== undefined) {
          isCopyMode = !!data.in_copy_mode;
          if (isCopyMode) {
            updateScrollBottomBtn(true);
          } else if (!isUserDragging) {
            scrollOffset = 0;
            updateScrollBottomBtn(false);
          }
        }
      }
    } catch (_) {
      // Auth overlay is shown automatically by apiRequest on 401
    }
  }

  checkStatus();
  setInterval(checkStatus, 4000);
})();
