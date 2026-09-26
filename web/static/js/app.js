(() => {
  const pollMs = Number(document.body.dataset.pollMs || 15000);
  const toastStack = document.getElementById("toast-stack");
  const badge = document.getElementById("alarm-badge");
  const seenKey = "mirth.alarm.seen";
  const sinceKey = "mirth.alarm.since";

  function getSeen() {
    try {
      return new Set(JSON.parse(sessionStorage.getItem(seenKey) || "[]"));
    } catch {
      return new Set();
    }
  }

  function saveSeen(set) {
    sessionStorage.setItem(seenKey, JSON.stringify([...set].slice(-200)));
  }

  function getSince() {
    return sessionStorage.getItem(sinceKey) || new Date(Date.now() - 60000).toISOString();
  }

  function setSince(v) {
    sessionStorage.setItem(sinceKey, v);
  }

  function updateBadge(n) {
    if (!badge) return;
    badge.textContent = String(n);
    badge.classList.toggle("hidden", !n);
  }

  function toast(alarm) {
    if (!toastStack) return;
    const el = document.createElement("div");
    el.className = "toast";
    el.innerHTML = `
      <div class="toast-title">Message flow error</div>
      <div class="toast-body">${escapeHtml(alarm.channelName || alarm.channelId)} · ${escapeHtml(alarm.connectorName || "")}</div>
      <div class="toast-meta">msg ${alarm.messageId} · ${escapeHtml((alarm.errorText || "").slice(0, 120))}</div>
    `;
    toastStack.appendChild(el);
    playAlarmSound();
    setTimeout(() => el.remove(), 8000);
  }

  let audioCtx = null;
  function ensureAudio() {
    if (!audioCtx) {
      const Ctx = window.AudioContext || window.webkitAudioContext;
      if (!Ctx) return null;
      audioCtx = new Ctx();
    }
    if (audioCtx.state === "suspended") {
      audioCtx.resume().catch(() => {});
    }
    return audioCtx;
  }

  function playAlarmSound() {
    const ctx = ensureAudio();
    if (!ctx) return;
    const now = ctx.currentTime;
    // Two short beeps — attention-grabbing without being a long siren.
    [[0, 880], [0.18, 1175]].forEach(([offset, freq]) => {
      const osc = ctx.createOscillator();
      const gain = ctx.createGain();
      osc.type = "square";
      osc.frequency.value = freq;
      gain.gain.setValueAtTime(0.0001, now + offset);
      gain.gain.exponentialRampToValueAtTime(0.22, now + offset + 0.02);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + offset + 0.14);
      osc.connect(gain);
      gain.connect(ctx.destination);
      osc.start(now + offset);
      osc.stop(now + offset + 0.16);
    });
  }

  // Browsers block audio until a user gesture; unlock on first interaction.
  ["pointerdown", "keydown", "touchstart"].forEach((evt) => {
    window.addEventListener(
      evt,
      () => {
        ensureAudio();
      },
      { once: true, passive: true }
    );
  });

  function escapeHtml(s) {
    return String(s)
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;");
  }

  async function pollAlarms() {
    try {
      const since = encodeURIComponent(getSince());
      const res = await fetch(`/alarms/feed?since=${since}`);
      if (!res.ok) return;
      const data = await res.json();
      updateBadge(data.unacked || 0);
      const seen = getSeen();
      for (const alarm of data.alarms || []) {
        const key = String(alarm.id);
        if (!seen.has(key)) {
          toast(alarm);
          seen.add(key);
        }
      }
      saveSeen(seen);
      if (data.now) setSince(data.now);
    } catch (err) {
      console.warn("alarm feed", err);
    }
  }

  function stateClass(state) {
    const s = String(state || "").toLowerCase();
    if (s === "started") return "state-ok";
    if (s === "paused") return "state-warn";
    if (s === "stopped" || s === "undeployed") return "state-muted";
    return "state-warn";
  }

  async function refreshDashboard() {
    const table = document.getElementById("channel-table");
    if (!table) return;
    try {
      const res = await fetch("/api/dashboard");
      if (!res.ok) return;
      const data = await res.json();
      const set = (id, val) => {
        const el = document.getElementById(id);
        if (el) el.childNodes[0].nodeValue = String(val);
      };
      const started = document.getElementById("stat-started");
      if (started) {
        started.innerHTML = `${data.channelsStarted}<span class="stat-den">/${data.channelCount}</span>`;
      }
      const recv = document.getElementById("stat-received");
      if (recv) recv.textContent = data.received;
      const sent = document.getElementById("stat-sent");
      if (sent) sent.textContent = data.sent;
      const err = document.getElementById("stat-error");
      if (err) err.textContent = data.error;
      const queued = document.getElementById("stat-queued");
      if (queued) {
        queued.textContent = data.queued;
        queued.classList.toggle("crimson", Number(data.queued) > 0);
        queued.classList.toggle("amber", !(Number(data.queued) > 0));
      }
      const tbody = table.querySelector("tbody");
      tbody.innerHTML = (data.channels || [])
        .map(
          (ch) => `
        <tr class="clickable" tabindex="0" role="link" data-href="/search?channelId=${encodeURIComponent(ch.channelId)}" onclick="location.href=this.dataset.href" onkeydown="if(event.key==='Enter'){location.href=this.dataset.href}">
          <td>
            <div class="cell-title"><a href="/search?channelId=${encodeURIComponent(ch.channelId)}">${escapeHtml(ch.name)}</a></div>
            <div class="cell-sub mono">${escapeHtml(ch.channelId)}</div>
          </td>
          <td><span class="pill ${stateClass(ch.state)}">${escapeHtml(ch.state)}</span></td>
          <td class="num mono">${ch.received}</td>
          <td class="num mono">${ch.sent}</td>
          <td class="num mono ${ch.error ? "hot" : ""}">${ch.error}</td>
          <td class="num mono">${ch.filtered}</td>
          <td class="num mono ${ch.queued ? "hot" : ""}">${ch.queued}</td>
        </tr>`
        )
        .join("");
      const refresh = document.getElementById("last-refresh");
      if (refresh && data.refreshedAt) {
        refresh.textContent = new Date(data.refreshedAt).toLocaleString();
      }
      const label = document.getElementById("conn-label");
      const conn = document.getElementById("conn-indicator");
      if (label && conn && data.mode) {
        conn.className = `conn mode-${data.mode}`;
        label.textContent = data.mode === "connected" ? "Connected" : "Unreachable";
      }
    } catch (err) {
      console.warn("dashboard refresh", err);
    }
  }

  const refreshBtn = document.getElementById("refresh-dashboard");
  if (refreshBtn) refreshBtn.addEventListener("click", refreshDashboard);

  const channelSelect = document.getElementById("channel-select");
  const columnSelect = document.getElementById("column-select");
  const searchForm = document.getElementById("search-form");
  if (channelSelect && columnSelect) {
    channelSelect.addEventListener("change", async () => {
      const id = channelSelect.value;
      columnSelect.innerHTML = `<option value="">Any / none</option>`;
      if (!id) return;
      try {
        const res = await fetch(`/api/channels/${encodeURIComponent(id)}/metadata`);
        if (!res.ok) return;
        const cols = await res.json();
        for (const col of cols) {
          const opt = document.createElement("option");
          opt.value = col.name;
          opt.textContent = `${col.name} (${col.type})`;
          columnSelect.appendChild(opt);
        }
      } catch (err) {
        console.warn("metadata", err);
      }
      // Auto-load messages for the selected channel (reset to page 1).
      if (searchForm) {
        const pageInput = searchForm.querySelector('input[name="page"]');
        if (pageInput) pageInput.value = "1";
        // Clear filter fields that belong to the previous channel context.
        const valueInput = searchForm.querySelector('input[name="value"]');
        if (valueInput) valueInput.value = "";
        searchForm.requestSubmit();
      }
    });
  }

  const watchAll = document.getElementById("watch-all");
  const watchNone = document.getElementById("watch-none");
  const watchForm = document.getElementById("watch-form");
  if (watchForm) {
    const boxes = () => [...watchForm.querySelectorAll('input[name="channelId"]')];
    if (watchAll) watchAll.addEventListener("click", () => boxes().forEach((b) => (b.checked = true)));
    if (watchNone) watchNone.addEventListener("click", () => boxes().forEach((b) => (b.checked = false)));
  }

  pollAlarms();
  setInterval(pollAlarms, Math.max(pollMs, 5000));
  if (document.getElementById("channel-table")) {
    setInterval(refreshDashboard, Math.max(pollMs, 5000));
  }
})();
