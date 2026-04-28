/* ================= CONFIG ================= */
const MAX_EVENTS = 8;

const THRESHOLD = {
  UNDER_VOLT: 198,
  OVER_VOLT: 231,
  UNDER_FREQ: 49.5,
  OVER_FREQ: 50.5,
  TIMEOUT_SEC: 5
};

/* ================= STATE ================= */
let lastUpdateTs   = 0;
let errorCount     = 0;
let lastAlarmState = null;
// appConfig dideklarasikan di index.html sebagai var global

/* ================= HELPER: getElementById yang aman ================= */
function $id(id) {
  const el = document.getElementById(id);
  if (!el) console.warn("[PZEM] Element tidak ditemukan:", id);
  return el;
}

/* ================= DOM ================= */
const el = {
  v:          $id("v"),
  i:          $id("i"),
  f:          $id("f"),
  pf:         $id("pf"),

  healthDot:  $id("health-status"),
  healthText: $id("health-text"),
  lastUpdate: $id("last-update"),

  alarmState:  $id("alarm-state"),
  alarmReason: $id("alarm-reason"),
  alarmCard:   $id("alarm-card"),

  connStatus:  $id("conn-status"),
  updateAge:   $id("update-age"),
  errorCount:  $id("error-count"),

  p:      $id("p_detail"),
  va:     $id("va"),
  var_:   $id("var"),
  phi:    $id("phi"),
  load:   $id("load"),
  eTotal: $id("e_detail"),
  eToday: $id("e_today"),
  cost:   $id("cost"),

  toggle:      $id("engineerToggle"),
  engineering: $id("engineeringSection"),
  body:        document.body,

  timeline:    $id("eventTimeline"),

  exportBtn:      $id("exportBtn"),
  resetCsvBtn:    $id("resetCsvBtn"),
  resetEnergyBtn: $id("resetEnergyBtn"),

  overlay:      $id("modal-overlay"),
  modalIcon:    $id("modal-icon"),
  modalTitle:   $id("modal-title"),
  modalBody:    $id("modal-body"),
  modalCancel:  $id("modal-cancel"),
  modalConfirm: $id("modal-confirm"),

  infTariff: $id("inf-tariff"),
  infMaxVA:  $id("inf-maxva"),
};

/* ================= UTIL ================= */
function nowTime() {
  return new Date().toLocaleTimeString();
}

/* ================= MODAL KONFIRMASI ================= */
let modalResolve = null;

function showModal({ icon = "⚠", title, body, confirmLabel = "Ya, Lanjutkan", confirmClass = "" }) {
  if (!el.overlay) return Promise.resolve(false);
  el.modalIcon.innerText    = icon;
  el.modalTitle.innerText   = title;
  el.modalBody.innerText    = body;
  el.modalConfirm.innerText = confirmLabel;
  el.modalConfirm.className = "modal-btn modal-btn--confirm" + (confirmClass ? " " + confirmClass : "");
  el.overlay.classList.remove("hidden");
  return new Promise((resolve) => { modalResolve = resolve; });
}

function closeModal(result) {
  if (el.overlay) el.overlay.classList.add("hidden");
  if (modalResolve) { modalResolve(result); modalResolve = null; }
}

if (el.modalCancel)  el.modalCancel.addEventListener("click",  () => closeModal(false));
if (el.modalConfirm) el.modalConfirm.addEventListener("click", () => closeModal(true));
if (el.overlay)      el.overlay.addEventListener("click", (e) => {
  if (e.target === el.overlay) closeModal(false);
});

/* ================= UPDATE CONFIG INFO ================= */
function updateConfigInfo() {
  if (el.infTariff) el.infTariff.innerText = `Rp ${appConfig.tariff.toLocaleString()}/kWh`;
  if (el.infMaxVA)  el.infMaxVA.innerText  = `${appConfig.max_va.toLocaleString()} VA`;
}

/* ================= EVENT TIMELINE ================= */
function addEvent(state, message) {
  if (!el.timeline) return;
  const empty = el.timeline.querySelector(".empty");
  if (empty) el.timeline.removeChild(empty);
  const div = document.createElement("div");
  div.className = `event ${state.toLowerCase()}`;
  div.innerHTML = `<time>[${nowTime()}]</time> ${message}`;
  el.timeline.prepend(div);
  while (el.timeline.children.length > MAX_EVENTS) {
    el.timeline.removeChild(el.timeline.lastChild);
  }
}

/* ================= CHART (terisolasi — error di sini tidak mempengaruhi tombol lain) ================= */
let chart = null;
try {
  const ctx = document.getElementById("powerChart");
  if (ctx && typeof Chart !== "undefined") {
    chart = new Chart(ctx.getContext("2d"), {
      type: "line",
      data: {
        labels: [],
        datasets: [{
          data: [],
          borderColor: "#3b82f6",
          borderWidth: 2,
          tension: 0.4,
          pointRadius: 0
        }]
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        animation: false,
        events: [],
        plugins: {
          legend:  { display: false },
          tooltip: { enabled: false }
        },
        scales: {
          x: { display: false },
          y: { ticks: { color: "#9ca3af" } }
        }
      }
    });
  } else {
    console.warn("[PZEM] Chart.js tidak tersedia atau canvas tidak ditemukan — grafik dinonaktifkan");
  }
} catch (e) {
  console.error("[PZEM] Gagal inisialisasi Chart:", e);
}

/* ================= MODE SWITCH ================= */
if (el.engineering) el.engineering.classList.add("hidden");
if (el.body)        el.body.classList.add("display-mode");

if (el.toggle) {
  el.toggle.addEventListener("change", () => {
    const engineer = el.toggle.checked;
    if (el.engineering) el.engineering.classList.toggle("hidden", !engineer);
    if (el.body) {
      el.body.classList.toggle("engineer-mode", engineer);
      el.body.classList.toggle("display-mode", !engineer);
    }
  });
}

/* ================= MAIN LOOP ================= */
function update() {
  fetch("/api/realtime")
  .then((r) => {
    if (!r.ok) throw new Error("API error");
    return r.json();
  })
  .then((d) => {
    lastUpdateTs = Date.now();
    const m   = d.metrics;
    const dev = d.device;

    if (d.config) {
      appConfig = d.config;
      updateConfigInfo();
    }

    /* === PERANGKAT TERPUTUS === */
    if (!dev.connected) {
      if (el.healthDot)  el.healthDot.style.color  = "#ef4444";
      if (el.healthText) el.healthText.innerText   = "PERANGKAT TERPUTUS";
      if (el.connStatus) el.connStatus.innerText   = "TERPUTUS";
      if (el.errorCount) el.errorCount.innerText   = dev.error_count || 0;
      if (el.alarmState) {
        el.alarmState.className  = "alarm-critical";
        el.alarmState.innerText  = "PERANGKAT TERPUTUS";
      }
      if (el.alarmReason) el.alarmReason.innerText = "Koneksi RS485 terputus — mencoba sambung kembali...";
      if (lastAlarmState !== "disconnected") {
        addEvent("critical", "Perangkat terputus");
        lastAlarmState = "disconnected";
      }
      return;
    }

    /* === KPI === */
    if (el.v)  el.v.innerText  = m.voltage.toFixed(1);
    if (el.i)  el.i.innerText  = m.current.toFixed(3);
    if (el.f)  el.f.innerText  = m.frequency.toFixed(1);
    if (el.pf) el.pf.innerText = m.pf.toFixed(2);

    /* === GRAFIK === */
    if (chart) {
      chart.data.labels.push("");
      chart.data.datasets[0].data.push(m.power);
      if (chart.data.datasets[0].data.length > 60) {
        chart.data.labels.shift();
        chart.data.datasets[0].data.shift();
      }
      chart.update();
    }

    /* === TURUNAN === */
    const S       = m.voltage * m.current;
    const Q       = Math.sqrt(Math.max(S * S - m.power * m.power, 0));
    const phi     = Math.acos(Math.min(Math.max(m.pf, -1), 1)) * 180 / Math.PI;
    const loadPct = S / appConfig.max_va * 100;
    const cost    = d.energy_today * appConfig.tariff;

    /* === UI ENGINEERING === */
    if (el.p)      el.p.innerText      = m.power.toFixed(2);
    if (el.va)     el.va.innerText     = S.toFixed(1);
    if (el.var_)   el.var_.innerText   = Q.toFixed(1);
    if (el.phi)    el.phi.innerText    = phi.toFixed(1);
    if (el.load)   el.load.innerText   = loadPct.toFixed(1);
    if (el.eTotal) el.eTotal.innerText = m.energy.toFixed(3);
    if (el.eToday) el.eToday.innerText = d.energy_today.toFixed(3);
    if (el.cost)   el.cost.innerText   = cost.toFixed(0);

    /* === KESEHATAN === */
    if (el.healthDot)  el.healthDot.style.color = "#22c55e";
    if (el.healthText) el.healthText.innerText  = "ONLINE";
    if (el.connStatus) el.connStatus.innerText  = "OK";
    if (el.errorCount) el.errorCount.innerText  = dev.error_count || 0;

    /* === ALARM === */
    let state = "normal";
    let reason = "Semua parameter dalam batas normal";

    if (m.voltage < THRESHOLD.UNDER_VOLT) {
      state = "warning";
      reason = "Under Voltage (< 198V)";
    } else if (m.voltage > THRESHOLD.OVER_VOLT) {
      state = "warning";
      reason = "Over Voltage (> 231V)";
    } else if (m.frequency < THRESHOLD.UNDER_FREQ) {
      state = "warning";
      reason = "Under Frequency (< 49.5Hz)";
    } else if (m.frequency > THRESHOLD.OVER_FREQ) {
      state = "warning";
      reason = "Over Frequency (> 50.5Hz)";
    }

    if (el.alarmState) {
      el.alarmState.className = `alarm-${state}`;
      el.alarmState.innerText = state === "normal" ? "NORMAL" : state.toUpperCase();
    }
    if (el.alarmReason) el.alarmReason.innerText = reason;

    if (state !== lastAlarmState) {
      addEvent(state, reason);
      lastAlarmState = state;
    }
  })
  .catch(() => {
    errorCount++;
    if (el.healthDot)  el.healthDot.style.color = "#ef4444";
    if (el.healthText) el.healthText.innerText  = "OFFLINE";
    if (el.connStatus) el.connStatus.innerText  = "TERPUTUS";
  });
}

/* ================= TIMER STATUS ================= */
setInterval(() => {
  if (!lastUpdateTs) return;
  const age = Math.floor((Date.now() - lastUpdateTs) / 1000);
  if (el.updateAge)  el.updateAge.innerText  = `${age}d lalu`;
  if (el.lastUpdate) el.lastUpdate.innerText = `Diperbarui ${age}d lalu`;
  if (age > THRESHOLD.TIMEOUT_SEC) {
    if (el.healthDot)  el.healthDot.style.color = "#ef4444";
    if (el.healthText) el.healthText.innerText  = "TIMEOUT";
    if (el.connStatus) el.connStatus.innerText  = "TIMEOUT";
  }
}, 1000);

/* ================= EXPORT CSV ================= */
if (el.exportBtn) {
  el.exportBtn.addEventListener("click", () => {
    el.exportBtn.disabled  = true;
    el.exportBtn.innerText = "Mengekspor...";
    fetch("/api/export")
    .then((r) => {
      if (!r.ok) throw new Error("Ekspor gagal");
      return r.blob();
    })
    .then((blob) => {
      const url = URL.createObjectURL(blob);
      const a   = document.createElement("a");
      a.href     = url;
      a.download = `pzem_export_${new Date().toISOString().slice(0,10)}.csv`;
      a.click();
      URL.revokeObjectURL(url);
    })
    .catch(() => alert("Ekspor gagal. Belum ada data."))
    .finally(() => {
      el.exportBtn.disabled  = false;
      el.exportBtn.innerText = "Export CSV";
    });
  });
}

/* ================= RESET CSV ================= */
if (el.resetCsvBtn) {
  el.resetCsvBtn.addEventListener("click", async () => {
    const confirmed = await showModal({
      icon:         "🗑",
      title:        "Hapus Riwayat CSV?",
      body:         "Seluruh data history.csv akan dihapus permanen. Pastikan sudah mengekspor data terlebih dahulu jika masih diperlukan.",
      confirmLabel: "Ya, Hapus Data",
    });
    if (!confirmed) return;
    el.resetCsvBtn.disabled  = true;
    el.resetCsvBtn.innerText = "Menghapus...";
    fetch("/api/reset-csv", { method: "POST" })
    .then((r) => { if (!r.ok) throw new Error(); addEvent("normal", "history.csv dihapus oleh pengguna"); })
    .catch(() => alert("Reset CSV gagal."))
    .finally(() => { el.resetCsvBtn.disabled = false; el.resetCsvBtn.innerText = "Reset CSV"; });
  });
}

/* ================= RESET ENERGI ================= */
if (el.resetEnergyBtn) {
  el.resetEnergyBtn.addEventListener("click", async () => {
    const confirmed = await showModal({
      icon:         "⚡",
      title:        "Reset Total Energi?",
      body:         "Perintah reset akan dikirim ke hardware PZEM-016. Counter energi pada sensor akan kembali ke 0 dan tidak bisa dikembalikan.",
      confirmLabel: "Ya, Reset Energi",
    });
    if (!confirmed) return;
    el.resetEnergyBtn.disabled  = true;
    el.resetEnergyBtn.innerText = "Mereset...";
    fetch("/api/reset-energy", { method: "POST" })
    .then((r) => { if (!r.ok) throw new Error(r.statusText); addEvent("normal", "Reset energi dijadwalkan — menunggu proses..."); })
    .catch((e) => alert("Reset energi gagal: " + e.message))
    .finally(() => { el.resetEnergyBtn.disabled = false; el.resetEnergyBtn.innerText = "Reset Energy"; });
  });
}

/* ================= KEYBOARD SHORTCUT ================= */
document.addEventListener("keydown", (e) => {
  if (e.key === "e" || e.key === "E") {
    if (el.toggle) {
      el.toggle.checked = !el.toggle.checked;
      el.toggle.dispatchEvent(new Event("change"));
    }
  }
  if (e.key === "Escape") {
    closeModal(false);
    closeSettings();
  }
});

/* ================= MULAI ================= */
updateConfigInfo();
setInterval(update, 1000);
