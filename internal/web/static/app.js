const $ = (id) => document.getElementById(id);

const state = {
  qrPollTimer: null,
  baccQrTimer: null,
  baccPendingId: '',
  dashPollTimer: null,
  activePage: 'dashboard',
  orphanIds: [],
  rcloneConf: '',
  desktopEnabled: false,
  canPersistLang: false,
  lastDesktopStatus: null,
};

function applyDesktopVisibility() {
  const isDesktop = state.desktopEnabled;
  [
    'desktop-drive-card',
    'desktop-sync-grid',
    'desktop-settings-card',
  ].forEach((id) => {
    const el = $(id);
    if (el) el.classList.toggle('hidden', !isDesktop);
  });
  // Windows Desktop mounts directly via WinFsp; hide Server/Portainer WebDAV & rclone UI.
  [
    'wizard-step-pill-webdav',
    'card-rclone-config',
    'card-webdav-settings',
  ].forEach((id) => {
    const el = $(id);
    if (el) el.classList.toggle('hidden', isDesktop);
  });
  if ($('dash-title')) {
    $('dash-title').textContent = isDesktop ? t('dashTitleDesktop') : t('dashTitleServer');
  }
  if (!isDesktop) {
    if ($('desktop-winfsp-banner')) $('desktop-winfsp-banner').classList.add('hidden');
    if ($('desktop-update-banner')) $('desktop-update-banner').classList.add('hidden');
  }
}

async function setLanguage(lang, persist = true) {
  const norm = normalizeLang(lang);
  applyI18n(norm);
  applyDesktopVisibility();

  if (persist && state.canPersistLang) {
    try {
      await api('/api/language', { method: 'PUT', body: JSON.stringify({ language: norm }) });
    } catch (_) {}
  }

  // Re-render dynamic views immediately in the new language.
  if (!$('view-app').classList.contains('hidden')) {
    if (state.activePage === 'dashboard') {
      loadDashboard();
      if (!state.desktopEnabled) loadRcloneSnippet();
    } else if (state.activePage === 'settings') {
      loadSettings();
      loadAccounts();
      if (state.desktopEnabled && state.lastDesktopStatus) {
        populateDesktopSettingsForm(state.lastDesktopStatus);
      } else if (state.desktopEnabled) {
        loadDesktopSettings();
      }
    }
  }
}

document.querySelectorAll('.lang-btn[data-lang]').forEach((btn) => {
  btn.addEventListener('click', () => setLanguage(btn.dataset.lang, true));
});

// --- Helpers ---
function toast(msg, type = 'ok') {
  const el = document.createElement('div');
  el.className = `toast ${type}`;
  el.textContent = msg;
  $('toast-container').appendChild(el);
  setTimeout(() => el.remove(), 4200);
}

async function api(url, opts = {}) {
  const res = await fetch(url, {
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', ...(opts.headers || {}) },
    ...opts,
  });
  let data = null;
  const ct = res.headers.get('content-type') || '';
  if (ct.includes('application/json')) {
    data = await res.json();
  }
  if (!res.ok) {
    throw new Error((data && data.error) || `HTTP ${res.status}`);
  }
  return data;
}

function fmtBytes(b) {
  if (!b || b <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = b;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(i === 0 ? 0 : 2)} ${units[i]}`;
}

function fmtTime(unix) {
  if (!unix) return '—';
  const locale = getLang() === 'vi' ? 'vi-VN' : 'en-US';
  return new Date(unix * 1000).toLocaleString(locale);
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function showView(viewId) {
  ['view-login', 'view-setup', 'view-app'].forEach((id) => $(id).classList.toggle('hidden', id !== viewId));
  if (viewId !== 'view-app') stopDashPoll();
  if (viewId !== 'view-setup') stopQRPoll();
}

// --- Bootstrap ---
async function boot() {
  applyI18n();
  try {
    const st = await api('/api/status');
    state.desktopEnabled = Boolean(st.desktop_enabled);
    state.canPersistLang = Boolean(!st.has_admin || st.authenticated);
    if (st.language) {
      applyI18n(st.language);
    }
    applyDesktopVisibility();
    if (!st.has_admin) {
      showView('view-setup');
      renderWizardStep('admin');
      return;
    }
    if (!st.authenticated) {
      showView('view-login');
      return;
    }
    if (st.setup_step !== 'ready') {
      showView('view-setup');
      await syncWizard();
      return;
    }
    showView('view-app');
    switchPage('dashboard');
  } catch (err) {
    toast(err.message, 'err');
  }
}

// --- Login ---
$('form-login').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await api('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ username: $('login-user').value, password: $('login-pass').value }),
    });
    $('login-pass').value = '';
    await boot();
  } catch (err) {
    toast(err.message, 'err');
  }
});

$('btn-logout').addEventListener('click', async () => {
  await api('/api/auth/logout', { method: 'POST' });
  await boot();
});

// --- Wizard ---
const wizardOrder = ['admin', 'telegram', 'channel', 'key', 'webdav'];

function renderWizardStep(step) {
  const idx = wizardOrder.indexOf(step);
  document.querySelectorAll('#wizard-stepper .step').forEach((el) => {
    const si = wizardOrder.indexOf(el.dataset.step);
    el.classList.toggle('active', el.dataset.step === step);
    el.classList.toggle('done', si >= 0 && si < idx);
  });
  wizardOrder.forEach((s) => $(`step-${s}`).classList.toggle('hidden', s !== step));
}

async function syncWizard() {
  const s = await api('/api/setup/state');
  if (typeof s.desktop_enabled === 'boolean') {
    state.desktopEnabled = s.desktop_enabled;
    applyDesktopVisibility();
  }
  if (s.setup_step === 'ready') {
    showView('view-app');
    switchPage('dashboard');
    return;
  }
  renderWizardStep(s.setup_step);

  if (s.setup_step === 'telegram') {
    if (s.tg_api_id) $('tg-api-id').value = s.tg_api_id;
    $('tg-auth-box').classList.toggle('hidden', !s.tg_api_configured);
  } else if (s.setup_step === 'channel') {
    await loadChannels();
  } else if (s.setup_step === 'key') {
    await probeChannelBackupsForWizard();
  } else if (s.setup_step === 'webdav') {
    if (s.webdav_user) $('setup-dav-user').value = s.webdav_user;
  }
}

$('form-setup-admin').addEventListener('submit', async (e) => {
  e.preventDefault();
  const u = $('setup-admin-user').value.trim();
  const p1 = $('setup-admin-pass').value;
  const p2 = $('setup-admin-pass2').value;
  if (p1 !== p2) return toast(t('passMismatch'), 'err');
  try {
    await api('/api/setup/admin', { method: 'POST', body: JSON.stringify({ username: u, password: p1 }) });
    state.canPersistLang = true;
    toast(t('adminCreated'));
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
});

// Telegram API connect
$('form-tg-api').addEventListener('submit', async (e) => {
  e.preventDefault();
  const btn = $('btn-tg-connect');
  btn.disabled = true;
  try {
    const res = await api('/api/setup/telegram/connect', {
      method: 'POST',
      body: JSON.stringify({
        api_id: Number($('tg-api-id').value),
        api_hash: $('tg-api-hash').value.trim(),
      }),
    });
    toast(t('mtprotoConnected'));
    if (res.authorized) {
      await syncWizard();
    } else {
      $('tg-auth-box').classList.remove('hidden');
      startQRLogin();
    }
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});

document.querySelectorAll('[data-auth-tab]').forEach((btn) => {
  btn.addEventListener('click', () => {
    const tab = btn.dataset.authTab;
    document.querySelectorAll('[data-auth-tab]').forEach((b) => b.classList.toggle('active', b === btn));
    $('auth-pane-qr').classList.toggle('hidden', tab !== 'qr');
    $('auth-pane-phone').classList.toggle('hidden', tab !== 'phone');
  });
});

function stopQRPoll() {
  if (state.qrPollTimer) { clearInterval(state.qrPollTimer); state.qrPollTimer = null; }
}

async function startQRLogin() {
  stopQRPoll();
  $('qr-holder').innerHTML = `<span class="muted">${esc(t('qrGenerating'))}</span>`;
  try {
    const st = await api('/api/setup/telegram/qr/start', { method: 'POST' });
    renderQR(st);
    state.qrPollTimer = setInterval(async () => {
      try {
        const res = await api('/api/setup/telegram/qr/status');
        if (res.authorized || res.qr.status === 'authorized') {
          stopQRPoll();
          toast(t('tgLoginOk'));
          await syncWizard();
          return;
        }
        renderQR(res.qr);
      } catch (_) {}
    }, 1500);
  } catch (err) {
    $('qr-holder').innerHTML = `<span class="muted">${esc(err.message)}</span>`;
  }
}

function renderQR(qr) {
  if (!qr) return;
  if (qr.status === 'password_required') {
    stopQRPoll();
    $('auth-pane-2fa').classList.remove('hidden');
    return;
  }
  if (qr.status === 'error') {
    stopQRPoll();
    $('qr-holder').innerHTML = `<span class="muted">${esc(qr.error || t('qrError'))}</span>`;
    return;
  }
  if (qr.qr_data_url) {
    $('qr-holder').innerHTML = `<img src="${qr.qr_data_url}" alt="Telegram QR" />`;
  }
}

$('btn-qr-start').addEventListener('click', startQRLogin);

$('btn-send-otp').addEventListener('click', async () => {
  try {
    await api('/api/setup/telegram/phone/code', { method: 'POST', body: JSON.stringify({ phone: $('tg-phone').value }) });
    toast(t('otpSent'));
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-verify-otp').addEventListener('click', async () => {
  try {
    const res = await api('/api/setup/telegram/phone/verify', { method: 'POST', body: JSON.stringify({ code: $('tg-otp').value }) });
    if (res.need_password) {
      $('auth-pane-2fa').classList.remove('hidden');
      toast(t('twoFaPrompt'));
      return;
    }
    toast(t('tgLoginOk'));
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-verify-2fa').addEventListener('click', async () => {
  try {
    await api('/api/setup/telegram/password', { method: 'POST', body: JSON.stringify({ password: $('tg-2fa').value }) });
    toast(t('twoFaOk'));
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
});

// Channel selection
async function loadChannels() {
  $('channel-list').innerHTML = `<div class="muted">${esc(t('loadingChannels'))}</div>`;
  try {
    const res = await api('/api/setup/channels');
    if (!res.channels || res.channels.length === 0) {
      $('channel-list').innerHTML = `<div class="muted">${esc(t('noChannels'))}</div>`;
      return;
    }
    $('channel-list').innerHTML = '';
    res.channels.forEach((ch) => {
      const row = document.createElement('div');
      row.className = 'list-item';
      row.innerHTML = `<div><strong>${esc(ch.title)}</strong> <span class="muted small">(#${ch.id})</span></div>`;
      const btn = document.createElement('button');
      btn.className = 'btn btn-secondary small-btn';
      btn.textContent = t('choose');
      btn.onclick = async () => {
        try {
          await api('/api/setup/channels/select', { method: 'POST', body: JSON.stringify(ch) });
          toast(t('channelSelected', { title: ch.title }));
          await syncWizard();
        } catch (err) { toast(err.message, 'err'); }
      };
      row.appendChild(btn);
      $('channel-list').appendChild(row);
    });
  } catch (err) {
    $('channel-list').innerHTML = `<div class="muted">${esc(err.message)}</div>`;
  }
}

$('btn-reload-channels').addEventListener('click', loadChannels);
$('btn-create-channel').addEventListener('click', async () => {
  const title = $('new-channel-title').value.trim() || 'TeleMount Storage';
  try {
    const res = await api('/api/setup/channels/create', { method: 'POST', body: JSON.stringify({ title }) });
    toast(t('channelCreated', { title: res.channel.title }));
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
});

// Key Setup / Restore
document.querySelectorAll('[data-key-tab]').forEach((btn) => {
  btn.addEventListener('click', () => {
    const tab = btn.dataset.keyTab;
    document.querySelectorAll('[data-key-tab]').forEach((b) => b.classList.toggle('active', b === btn));
    $('key-pane-fresh').classList.toggle('hidden', tab !== 'fresh');
    $('key-pane-restore').classList.toggle('hidden', tab !== 'restore');
    if (tab === 'restore' && ($('restore-backup-select').options.length === 0 || $('restore-backup-select').value === '0')) {
      probeChannelBackupsForWizard();
    }
  });
});

async function probeChannelBackupsForWizard() {
  const sel = $('restore-backup-select');
  sel.innerHTML = `<option value="0">${esc(t('scanningBackups'))}</option>`;
  $('existing-backup-notice').classList.add('hidden');
  try {
    const res = await api('/api/ops/backups');
    const list = res.backups || [];
    sel.innerHTML = '';
    if (list.length > 0) {
      $('existing-backup-notice').textContent = t('foundBackupsNotice', { count: list.length });
      $('existing-backup-notice').classList.remove('hidden');
      list.forEach((b) => {
        const opt = document.createElement('option');
        opt.value = b.msg_id;
        opt.textContent = `${b.file_name} (${fmtBytes(b.size)} — ${fmtTime(b.date)})`;
        sel.appendChild(opt);
      });
    } else {
      const opt = document.createElement('option');
      opt.value = '0';
      opt.textContent = t('noBackupsInChannel');
      sel.appendChild(opt);
    }
  } catch (err) {
    sel.innerHTML = '';
    const opt = document.createElement('option');
    opt.value = '0';
    opt.textContent = t('backupLoadErr', { msg: err.message });
    sel.appendChild(opt);
    toast(err.message, 'err');
  }
}

$('btn-reload-wizard-backups').addEventListener('click', probeChannelBackupsForWizard);
$('btn-change-channel').addEventListener('click', async () => {
  try {
    await api('/api/setup/channels/clear', { method: 'POST' });
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-gen-key').addEventListener('click', async () => {
  try {
    const res = await api('/api/setup/key/generate');
    $('fresh-key-input').value = res.passphrase;
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-copy-key').addEventListener('click', () => {
  const v = $('fresh-key-input').value;
  if (!v) return toast(t('noKeyCopy'), 'err');
  navigator.clipboard.writeText(v);
  toast(t('keyCopied'));
});

$('btn-dl-key').addEventListener('click', () => {
  const v = $('fresh-key-input').value;
  if (!v) return toast(t('noKeyDownload'), 'err');
  const blob = new Blob([`TeleMount Encryption Key\nCreated: ${new Date().toISOString()}\nKey: ${v}\n`], { type: 'text/plain' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = 'telemount-encryption-key.txt';
  a.click();
});

$('btn-init-key').addEventListener('click', async () => {
  const passphrase = $('fresh-key-input').value;
  if (passphrase.length < 8) return toast(t('keyTooShort'), 'err');
  if (!$('chk-saved-key').checked) return toast(t('mustConfirmKey'), 'err');
  const btn = $('btn-init-key');
  btn.disabled = true;
  try {
    await api('/api/setup/key/init', { method: 'POST', body: JSON.stringify({ passphrase }) });
    toast(t('keyInitialized'));
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});

$('btn-restore-key').addEventListener('click', async () => {
  const passphrase = $('restore-key-input').value;
  const msg_id = Number($('restore-backup-select').value || 0);
  if (!passphrase) return toast(t('enterOldKeyErr'), 'err');
  const btn = $('btn-restore-key');
  btn.disabled = true;
  try {
    const res = await api('/api/setup/key/restore', { method: 'POST', body: JSON.stringify({ passphrase, msg_id }) });
    toast(t('restoreOk', { count: res.stats ? res.stats.file_count : 0 }));
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});

$('btn-gen-dav-pass').addEventListener('click', async () => {
  try {
    const res = await api('/api/setup/key/generate');
    $('setup-dav-pass').value = res.passphrase;
  } catch (err) { toast(err.message, 'err'); }
});

$('form-setup-webdav').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await api('/api/setup/webdav', {
      method: 'POST',
      body: JSON.stringify({ username: $('setup-dav-user').value.trim(), password: $('setup-dav-pass').value }),
    });
    toast(t('setupComplete'));
    await boot();
  } catch (err) { toast(err.message, 'err'); }
});

// --- Navigation (Dashboard & Settings only) ---
document.querySelectorAll('.nav-item').forEach((btn) => {
  btn.addEventListener('click', () => switchPage(btn.dataset.nav));
});

function switchPage(page) {
  state.activePage = page;
  document.querySelectorAll('.nav-item').forEach((b) => b.classList.toggle('active', b.dataset.nav === page));
  ['dashboard', 'settings'].forEach((p) => $(`page-${p}`).classList.toggle('hidden', p !== page));
  stopDashPoll();
  if (page === 'dashboard') {
    loadDashboard();
    if (!state.desktopEnabled) {
      loadRcloneSnippet();
    }
    state.dashPollTimer = setInterval(loadDashboard, 2500);
  }
  if (page === 'settings') {
    loadSettings();
    loadAccounts();
    if (state.desktopEnabled) {
      loadDesktopSettings();
    }
  }
}

function stopDashPoll() {
  if (state.dashPollTimer) { clearInterval(state.dashPollTimer); state.dashPollTimer = null; }
}

// --- Dashboard & Ops ---
async function loadDashboard() {
  try {
    const d = await api('/api/dashboard');
    $('sidebar-channel').textContent = d.channel?.title || t('noChannelSelected');
    const pill = $('tg-status-pill');
    if (d.telegram?.authorized && d.telegram?.connected) {
      const uname = d.telegram.user?.username ? `@${d.telegram.user.username}` : (d.telegram.user?.first_name || 'Connected');
      pill.textContent = `● Telegram: ${uname}`;
      pill.className = 'status-pill ok';
    } else {
      pill.textContent = `● Telegram: ${d.telegram?.connection_state || 'offline'}`;
      pill.className = 'status-pill err';
    }

    if (d.stats) {
      $('kpi-size').textContent = fmtBytes(d.stats.total_plain_size);
      $('kpi-parts').textContent = t('kpiEncryptedParts', { bytes: fmtBytes(d.stats.total_cipher_size), count: d.stats.part_count });
      $('kpi-files').textContent = t('kpiFiles', { count: d.stats.file_count });
      $('kpi-folders').textContent = t('kpiFolders', { count: d.stats.folder_count });

      const pk = d.stats.packs || {};
      const stg = d.packer || {};
      const inflight = stg.inflight_packs ? t('packInflight', { n: stg.inflight_packs }) : '';
      $('pack-summary-line').textContent = t('packSummaryLine', {
        bufFiles: stg.buffered_files || 0,
        bufBytes: fmtBytes(stg.buffered_bytes || 0),
        inflight,
        packs: pk.pack_count || 0,
        live: fmtBytes(pk.live_pack_bytes || 0),
        total: fmtBytes(pk.total_pack_bytes || 0),
        dead: fmtBytes(pk.dead_pack_bytes || 0),
      });
    }
    if (d.cache) {
      const usedMB = (d.cache.used_bytes / 1048576).toFixed(1);
      const maxMB = (d.cache.max_bytes / 1048576).toFixed(0);
      const total = (d.cache.hits || 0) + (d.cache.misses || 0);
      const rate = total > 0 ? Math.round((d.cache.hits / total) * 100) : 0;
      $('kpi-cache').textContent = `${usedMB} / ${maxMB} MB`;
      $('kpi-cache-hit').textContent = t('kpiHitChunks', { rate, count: d.cache.entries });
    }
    $('kpi-tg').textContent = d.telegram?.authorized ? t('online') : t('offline');
    $('kpi-mem').textContent = t('goRamUptime', { ram: fmtBytes(d.mem_alloc_bytes), mins: Math.floor((d.uptime_sec || 0) / 60) });

    const trBox = $('active-transfers');
    const trs = d.telegram?.active_transfers || [];
    if (!trs.length) {
      trBox.textContent = t('noActiveTransfers');
    } else {
      trBox.innerHTML = trs.map((tr) => {
        const pct = tr.total > 0 ? Math.min(100, Math.round((tr.done / tr.total) * 100)) : 0;
        return `<div class="list-item">
          <div style="flex:1">
            <div>${tr.kind === 'upload' ? '⬆' : '⬇'} <strong>${esc(tr.name)}</strong> (${fmtBytes(tr.done)} / ${tr.total > 0 ? fmtBytes(tr.total) : '?'} · ${fmtBytes(tr.speed_bps)}/s)</div>
            <div class="progress-track"><div class="progress-fill" style="width:${pct}%"></div></div>
          </div>
        </div>`;
      }).join('');
    }

    const flBox = $('flood-events');
    const fls = d.telegram?.recent_floods || [];
    if (!fls.length) {
      flBox.textContent = t('noFloodEvents');
    } else {
      flBox.innerHTML = fls.slice(0, 8).map((f) =>
        `<div class="list-item"><span><code>${esc(f.method)}</code> — <strong>${esc(t('floodWaitItem', { sec: f.duration_sec }))}</strong></span><span class="muted small">${fmtTime(f.at)}</span></div>`
      ).join('');
    }
    if (state.desktopEnabled) {
      if (d.desktop) {
        renderDesktopStatus(d.desktop);
      } else {
        await loadDesktopStatus();
      }
    }
  } catch (_) {}
}

$('btn-refresh-dash').addEventListener('click', loadDashboard);
$('btn-clear-cache').addEventListener('click', async () => {
  try { await api('/api/ops/cache/clear', { method: 'POST' }); toast(t('toastRamCleared')); loadDashboard(); }
  catch (err) { toast(err.message, 'err'); }
});
$('btn-flush-pack').addEventListener('click', async () => {
  const btn = $('btn-flush-pack');
  btn.disabled = true;
  try {
    await api('/api/ops/packs/flush', { method: 'POST' });
    toast(t('toastPackFlushed'));
    loadDashboard();
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});
$('btn-compact-packs').addEventListener('click', async () => {
  const btn = $('btn-compact-packs');
  btn.disabled = true;
  try {
    const r = await api('/api/ops/packs/compact', { method: 'POST' });
    if (!r.compacted_packs) {
      toast(t('toastPacksOptimal'));
    } else {
      toast(t('toastPacksCompacted', { old: r.compacted_packs, new: r.created_packs, bytes: fmtBytes(r.reclaimed_bytes) }));
    }
    loadDashboard();
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});

async function loadRcloneSnippet() {
  try {
    const r = await api('/api/ops/rclone-config');
    state.rcloneConf = r.rclone_conf;
    if ($('rclone-int-url')) $('rclone-int-url').textContent = r.internal_url;
    if ($('rclone-ext-url')) $('rclone-ext-url').textContent = r.external_url;
    $('rclone-conf-box').textContent = r.rclone_conf;
    $('rclone-mount-box').textContent = r.mount_cmd;
  } catch (_) {}
}

$('btn-copy-rclone').addEventListener('click', () => {
  navigator.clipboard.writeText(state.rcloneConf || '');
  toast(t('rcloneCopied'));
});

$('btn-scan-orphans').addEventListener('click', async () => {
  $('orphan-result').textContent = t('scanningHistory');
  try {
    const r = await api('/api/ops/orphans');
    state.orphanIds = (r.orphans || []).map((o) => o.msg_id);
    $('btn-clean-orphans').classList.toggle('hidden', state.orphanIds.length === 0);
    if (!state.orphanIds.length) {
      $('orphan-result').textContent = t('noOrphansFound', { docs: r.scanned_docs });
    } else {
      $('orphan-result').textContent = t('orphansFound', { docs: r.scanned_docs, count: state.orphanIds.length, bytes: fmtBytes(r.total_bytes) });
    }
  } catch (err) {
    $('orphan-result').textContent = t('errorPrefix', { msg: err.message });
  }
});

$('btn-clean-orphans').addEventListener('click', async () => {
  if (!state.orphanIds.length) return;
  if (!confirm(t('confirmCleanOrphans', { count: state.orphanIds.length }))) return;
  try {
    const r = await api('/api/ops/orphans/clean', { method: 'POST', body: JSON.stringify({ msg_ids: state.orphanIds }) });
    toast(t('orphansCleaned', { count: r.deleted }));
    state.orphanIds = [];
    $('btn-clean-orphans').classList.add('hidden');
    $('orphan-result').textContent = t('cleanedDone');
  } catch (err) { toast(err.message, 'err'); }
});

// --- Settings ---
async function loadSettings() {
  try {
    const s = await api('/api/settings');
    $('set-small-mb').value = s.small_file_max_mb;
    $('set-pack-mb').value = s.pack_max_mb;
    $('set-pack-idle').value = s.pack_idle_sec;
    $('set-part-mb').value = s.part_size_mb;
    $('set-cache-mb').value = s.ram_cache_mb;
    $('set-threads').value = s.upload_threads;
    $('set-backup-min').value = s.backup_interval_min;
    $('set-dav-user').value = s.webdav_user || '';
    $('last-backup-info').textContent = s.last_backup_at
      ? t('lastBackupLine', { time: fmtTime(s.last_backup_at), id: s.last_backup_msg_id })
      : t('noBackupYet');
  } catch (err) { toast(err.message, 'err'); }
}

$('form-settings').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        small_file_max_mb: Number($('set-small-mb').value),
        pack_max_mb: Number($('set-pack-mb').value),
        pack_idle_sec: Number($('set-pack-idle').value),
        part_size_mb: Number($('set-part-mb').value),
        ram_cache_mb: Number($('set-cache-mb').value),
        upload_threads: Number($('set-threads').value),
        backup_interval_min: Number($('set-backup-min').value),
      }),
    });
    toast(t('toastSettingsSaved'));
  } catch (err) { toast(err.message, 'err'); }
});

$('form-set-webdav').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await api('/api/settings/webdav', {
      method: 'POST',
      body: JSON.stringify({ username: $('set-dav-user').value.trim(), password: $('set-dav-pass').value }),
    });
    $('set-dav-pass').value = '';
    toast(t('toastWebdavUpdated'));
  } catch (err) { toast(err.message, 'err'); }
});

$('form-set-admin-pass').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await api('/api/settings/admin-password', {
      method: 'POST',
      body: JSON.stringify({
        current_password: $('cur-admin-pass').value,
        new_username: $('new-admin-user').value.trim(),
        new_password: $('new-admin-pass').value,
      }),
    });
    $('cur-admin-pass').value = '';
    $('new-admin-pass').value = '';
    toast(t('toastAdminPassChanged'));
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-backup-now').addEventListener('click', async () => {
  const btn = $('btn-backup-now');
  btn.disabled = true;
  try {
    const r = await api('/api/ops/backup', { method: 'POST' });
    toast(t('toastBackupDone', { id: r.msg_id, bytes: fmtBytes(r.size) }));
    loadSettings();
    loadBackupsInSettings();
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});

async function loadBackupsInSettings() {
  const box = $('backup-list');
  const sel = $('settings-restore-select');
  box.innerHTML = `<div class="muted small">${esc(t('loading'))}</div>`;
  sel.innerHTML = '';
  try {
    const r = await api('/api/ops/backups');
    const list = r.backups || [];
    if (!list.length) {
      box.innerHTML = `<div class="muted small">${esc(t('noBackupsOnChannel'))}</div>`;
      return;
    }
    box.innerHTML = list.map((b) =>
      `<div class="list-item"><span>${esc(b.file_name)}</span><span class="muted small">${fmtBytes(b.size)} · ${fmtTime(b.date)}</span></div>`
    ).join('');
    list.forEach((b) => {
      const opt = document.createElement('option');
      opt.value = b.msg_id;
      opt.textContent = `${b.file_name} (${fmtTime(b.date)})`;
      sel.appendChild(opt);
    });
  } catch (err) {
    box.innerHTML = `<div class="muted small">${esc(err.message)}</div>`;
  }
}
$('btn-load-backups').addEventListener('click', loadBackupsInSettings);

$('btn-settings-restore').addEventListener('click', async () => {
  const msg_id = Number($('settings-restore-select').value || 0);
  const passphrase = $('settings-restore-key').value;
  if (!passphrase) return toast(t('enterKeyToRestore'), 'err');
  if (!confirm(t('confirmRestoreDb'))) return;
  try {
    await api('/api/ops/restore', { method: 'POST', body: JSON.stringify({ msg_id, passphrase }) });
    $('settings-restore-key').value = '';
    toast(t('toastDbRestored'));
    loadSettings();
  } catch (err) { toast(err.message, 'err'); }
});

// --- Multi-Account Telegram Backup Mirroring ---
function fmtTGUser(u) {
  if (!u) return t('unknownUser');
  const name = [u.first_name, u.last_name].filter(Boolean).join(' ') || 'User';
  return u.username ? `${name} (@${u.username})` : (u.phone ? `${name} (+${u.phone})` : name);
}

async function loadAccounts() {
  const box = $('accounts-box');
  try {
    const res = await api('/api/accounts');
    const p = res.primary || {};
    const backups = res.backups || [];

    const primaryMeta = t('primaryChannelMeta', {
      title: p.channel_title || '—',
      id: p.channel_id || 0,
      status: p.authorized ? t('online') : t('offline'),
      total: p.total_blobs || 0,
    });

    let html = `
      <div class="list-item" style="border-color:rgba(34,197,94,0.45)">
        <div>
          <strong>⭐ ${esc(t('primaryAccLabel'))} ${esc(fmtTGUser(p.user))}</strong>
          <div class="muted small">${esc(primaryMeta)}</div>
        </div>
        <span class="status-pill ${p.authorized ? 'ok' : 'err'}">${esc(p.authorized ? t('badgePrimaryActive') : t('badgeDisconnected'))}</span>
      </div>
    `;

    if (!backups.length) {
      html += `<div class="muted small">${t('noBackupAccountsHtml')}</div>`;
    } else {
      backups.forEach((b) => {
        const pct = b.total_blobs > 0 ? Math.min(100, Math.round((b.synced_blobs / b.total_blobs) * 100)) : 100;
        const backupMeta = t('backupChannelMeta', {
          title: b.own_channel_title || t('notCreatedYet'),
          synced: b.synced_blobs,
          total: b.total_blobs,
          pct,
          status: b.authorized ? t('online') : b.connection_state,
        });
        html += `
          <div class="list-item">
            <div style="flex:1">
              <strong>🛡️ ${esc(t('backupAccLabel'))} ${esc(fmtTGUser(b.user))} <span class="muted small">(${esc(b.id)})</span></strong>
              <div class="muted small">${esc(backupMeta)}</div>
            </div>
            <div class="row-inline">
              <button type="button" class="btn btn-secondary small-btn" data-promote="${esc(b.id)}">${esc(t('btnPromotePrimary'))}</button>
              <button type="button" class="btn btn-danger small-btn" data-del-acc="${esc(b.id)}">${esc(t('btnRemove'))}</button>
            </div>
          </div>
        `;
      });
    }
    box.innerHTML = html;

    box.querySelectorAll('[data-promote]').forEach((btn) => {
      btn.onclick = async () => {
        const id = btn.dataset.promote;
        if (!confirm(t('confirmPromote', { id }))) return;
        btn.disabled = true;
        try {
          const r = await api(`/api/accounts/backup/${id}/promote`, { method: 'POST' });
          toast(t('toastPromoted', { title: r.new_channel }));
          loadAccounts();
          loadDashboard();
        } catch (err) { toast(err.message, 'err'); }
        finally { btn.disabled = false; }
      };
    });

    box.querySelectorAll('[data-del-acc]').forEach((btn) => {
      btn.onclick = async () => {
        const id = btn.dataset.delAcc;
        if (!confirm(t('confirmRemoveAcc', { id }))) return;
        try {
          await api(`/api/accounts/backup/${id}`, { method: 'DELETE' });
          toast(t('toastAccRemoved'));
          loadAccounts();
        } catch (err) { toast(err.message, 'err'); }
      };
    });
  } catch (err) {
    box.innerHTML = `<div class="muted small">${esc(err.message)}</div>`;
  }
}

$('btn-sync-accounts').addEventListener('click', async () => {
  const btn = $('btn-sync-accounts');
  btn.disabled = true;
  try {
    const r = await api('/api/accounts/sync', { method: 'POST', body: JSON.stringify({}) });
    if (r.errors && r.errors.length) {
      toast(t('toastSyncedWithErr', { count: r.synced_blobs, errs: r.errors.join('; ') }), 'err');
    } else {
      toast(t('toastSyncedOk', { count: r.synced_blobs }));
    }
    loadAccounts();
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});

function stopBaccQR() {
  if (state.baccQrTimer) { clearInterval(state.baccQrTimer); state.baccQrTimer = null; }
}

function closeBaccModal() {
  stopBaccQR();
  $('backup-acc-modal').classList.add('hidden');
  $('bacc-pane-2fa').classList.add('hidden');
}
$('btn-close-bacc-modal').addEventListener('click', closeBaccModal);

document.querySelectorAll('[data-bacc-tab]').forEach((btn) => {
  btn.addEventListener('click', () => {
    const tab = btn.dataset.baccTab;
    document.querySelectorAll('[data-bacc-tab]').forEach((b) => b.classList.toggle('active', b === btn));
    $('bacc-pane-qr').classList.toggle('hidden', tab !== 'qr');
    $('bacc-pane-phone').classList.toggle('hidden', tab !== 'phone');
  });
});

$('btn-add-backup-acc').addEventListener('click', async () => {
  $('backup-acc-modal').classList.remove('hidden');
  $('bacc-pane-2fa').classList.add('hidden');
  $('bacc-qr-holder').innerHTML = `<span class="muted">${esc(t('baccConnecting'))}</span>`;
  try {
    const r = await api('/api/accounts/backup/start', { method: 'POST', body: JSON.stringify({}) });
    state.baccPendingId = r.id;
    if (r.authorized) {
      await finalizeBackupLogin();
      return;
    }
    await startBaccQR();
  } catch (err) {
    $('bacc-qr-holder').innerHTML = `<span class="muted">${esc(err.message)}</span>`;
  }
});

async function startBaccQR() {
  if (!state.baccPendingId) return;
  stopBaccQR();
  $('bacc-qr-holder').innerHTML = `<span class="muted">${esc(t('qrGenerating'))}</span>`;
  try {
    const qr = await api(`/api/accounts/backup/${state.baccPendingId}/qr/start`, { method: 'POST' });
    renderBaccQR(qr);
    state.baccQrTimer = setInterval(async () => {
      try {
        const res = await api(`/api/accounts/backup/${state.baccPendingId}/qr/status`);
        if (res.authorized || res.qr.status === 'authorized') {
          stopBaccQR();
          await finalizeBackupLogin();
          return;
        }
        renderBaccQR(res.qr);
      } catch (_) {}
    }, 1500);
  } catch (err) {
    $('bacc-qr-holder').innerHTML = `<span class="muted">${esc(err.message)}</span>`;
  }
}

function renderBaccQR(qr) {
  if (!qr) return;
  if (qr.status === 'password_required') {
    stopBaccQR();
    $('bacc-pane-2fa').classList.remove('hidden');
    return;
  }
  if (qr.qr_data_url) {
    $('bacc-qr-holder').innerHTML = `<img src="${qr.qr_data_url}" alt="Backup QR" />`;
  }
}

$('btn-bacc-qr-refresh').addEventListener('click', startBaccQR);

$('btn-bacc-send-otp').addEventListener('click', async () => {
  if (!state.baccPendingId) return;
  try {
    await api(`/api/accounts/backup/${state.baccPendingId}/phone/code`, {
      method: 'POST',
      body: JSON.stringify({ phone: $('bacc-phone').value }),
    });
    toast(t('baccOtpSent'));
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-bacc-verify-otp').addEventListener('click', async () => {
  if (!state.baccPendingId) return;
  try {
    const r = await api(`/api/accounts/backup/${state.baccPendingId}/phone/verify`, {
      method: 'POST',
      body: JSON.stringify({ code: $('bacc-otp').value }),
    });
    if (r.need_password) {
      $('bacc-pane-2fa').classList.remove('hidden');
      toast(t('baccNeed2fa'));
      return;
    }
    await finalizeBackupLogin();
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-bacc-verify-2fa').addEventListener('click', async () => {
  if (!state.baccPendingId) return;
  try {
    await api(`/api/accounts/backup/${state.baccPendingId}/password`, {
      method: 'POST',
      body: JSON.stringify({ password: $('bacc-2fa').value }),
    });
    await finalizeBackupLogin();
  } catch (err) { toast(err.message, 'err'); }
});

async function finalizeBackupLogin() {
  stopBaccQR();
  $('bacc-qr-holder').innerHTML = `<span class="muted">${esc(t('baccFinalizing'))}</span>`;
  try {
    const r = await api(`/api/accounts/backup/${state.baccPendingId}/finalize`, { method: 'POST' });
    closeBaccModal();
    toast(t('baccAddedToast', { title: r.account.own_channel_title }));
    loadAccounts();
    setTimeout(loadAccounts, 3000);
  } catch (err) {
    toast(err.message, 'err');
  }
}

// --- Windows Desktop Virtual Drive & Persistent Disk Cache ---
async function loadDesktopStatus() {
  if (!state.desktopEnabled) return;
  try {
    const d = await api('/api/desktop/status');
    renderDesktopStatus(d);
  } catch (_) {}
}

function renderDesktopStatus(d) {
  if (!d) return;
  state.lastDesktopStatus = d;
  const winfspOk = Boolean(d.winfsp_installed);
  $('desktop-winfsp-banner').classList.toggle('hidden', winfspOk);
  $('desktop-winfsp-guidance').textContent = d.winfsp_guidance || t('winfspDefaultGuidance');

  renderDesktopUpdateUI(d);

  $('desktop-drive-letter').textContent = d.drive_letter || 'V:';
  const badge = $('desktop-mount-badge');
  badge.textContent = d.mounted ? t('mountedBadge') : t('unmountedBadge');
  badge.className = `status-pill ${d.mounted ? 'ok' : 'err'}`;
  $('desktop-startup-badge').textContent = d.run_at_startup ? t('startupOnBadge') : t('startupOffBadge');

  const mErrEl = $('desktop-mount-error');
  mErrEl.textContent = d.mount_error || '';
  mErrEl.classList.toggle('hidden', !d.mount_error);

  $('btn-desktop-mount').disabled = Boolean(d.mounted || !winfspOk);
  $('btn-desktop-unmount').disabled = !d.mounted;
  $('btn-desktop-open').disabled = !d.mounted;

  const qState = $('desktop-queue-state');
  const qCount = d.queue_pending_count || 0;
  if (d.queue_syncing) {
    qState.textContent = t('queueSyncing');
    qState.className = 'status-pill warn';
  } else if (qCount > 0) {
    qState.textContent = t('queuePending');
    qState.className = 'status-pill warn';
  } else {
    qState.textContent = t('queueSynced');
    qState.className = 'status-pill ok';
  }
  $('desktop-queue-summary').textContent = t('queueSummary', { count: qCount, bytes: fmtBytes(d.queue_pending_bytes || 0) });
  const qErrEl = $('desktop-queue-error');
  qErrEl.textContent = d.queue_last_error || '';
  qErrEl.classList.toggle('hidden', !d.queue_last_error);

  const used = d.disk_cache_bytes || 0;
  const max = d.disk_cache_max_bytes || 0;
  const pct = max > 0 ? Math.min(100, Math.round((used / max) * 100)) : 0;
  $('desktop-disk-cache-usage').textContent = `${fmtBytes(used)} / ${fmtBytes(max)}`;
  $('desktop-disk-cache-bar').style.width = `${pct}%`;
  $('desktop-disk-cache-meta').textContent = t('cacheMeta', {
    bytes: fmtBytes(d.disk_cache_pinned_bytes || 0),
    count: d.disk_cache_entries || 0,
  });
}

$('btn-copy-winget').addEventListener('click', () => {
  const cmd = $('winget-winfsp-cmd').textContent || 'winget install WinFsp.WinFsp';
  if (navigator.clipboard) navigator.clipboard.writeText(cmd);
  toast(t('wingetCopied'));
});

$('btn-desktop-mount').addEventListener('click', async () => {
  const btn = $('btn-desktop-mount');
  btn.disabled = true;
  try {
    const r = await api('/api/desktop/mount', { method: 'POST' });
    if (r && r.status) renderDesktopStatus(r.status);
    toast(t('toastMounted'));
  } catch (err) {
    toast(err.message, 'err');
    await loadDesktopStatus();
  }
});

$('btn-desktop-unmount').addEventListener('click', async () => {
  const btn = $('btn-desktop-unmount');
  btn.disabled = true;
  try {
    const r = await api('/api/desktop/unmount', { method: 'POST' });
    if (r && r.status) renderDesktopStatus(r.status);
    toast(t('toastUnmounted'));
  } catch (err) {
    toast(err.message, 'err');
    await loadDesktopStatus();
  }
});

$('btn-desktop-open').addEventListener('click', async () => {
  const btn = $('btn-desktop-open');
  btn.disabled = true;
  try {
    await api('/api/desktop/open', { method: 'POST' });
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    await loadDesktopStatus();
  }
});

$('btn-desktop-flush-queue').addEventListener('click', async () => {
  const btn = $('btn-desktop-flush-queue');
  btn.disabled = true;
  try {
    const r = await api('/api/desktop/queue/flush', { method: 'POST' });
    if (r && r.status) renderDesktopStatus(r.status);
    toast(t('toastQueueFlushed'));
  } catch (err) {
    toast(err.message, 'err');
    await loadDesktopStatus();
  } finally {
    btn.disabled = false;
  }
});

$('btn-desktop-clear-cache').addEventListener('click', async () => {
  const btn = $('btn-desktop-clear-cache');
  btn.disabled = true;
  try {
    const r = await api('/api/desktop/cache/clear', { method: 'POST' });
    if (r && r.status) renderDesktopStatus(r.status);
    toast(t('toastDiskCacheCleared'));
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
  }
});

function renderDesktopUpdateUI(d) {
  if (!d) return;
  const verText = d.app_version ? `(${d.app_version})` : '';
  if ($('desktop-app-version')) $('desktop-app-version').textContent = verText;
  if ($('settings-app-version')) $('settings-app-version').textContent = verText;

  const updBanner = $('desktop-update-banner');
  if (updBanner) {
    updBanner.classList.toggle('hidden', !d.update_available);
    if (d.update_available) {
      $('desktop-update-title').textContent = t('updateBannerTitle', { ver: d.latest_version });
      $('desktop-update-desc').textContent = t('updateBannerDesc', { cur: d.app_version || 'dev' });
      if (d.release_url) $('desktop-update-link').href = d.release_url;
    }
  }

  const stEl = $('settings-update-status');
  const linkEl = $('settings-update-link');
  const applyEl = $('btn-settings-apply-update');
  if (stEl && linkEl && applyEl) {
    if (d.update_available) {
      stEl.textContent = t('updateStatusAvail', { ver: d.latest_version, cur: d.app_version || 'dev' });
      stEl.className = 'alert alert-info';
      if (d.release_url) {
        linkEl.href = d.release_url;
        linkEl.classList.remove('hidden');
      }
      applyEl.classList.remove('hidden');
    } else if (d.update_error) {
      stEl.textContent = t('updateStatusErr', { msg: d.update_error });
      stEl.className = 'muted small';
      linkEl.classList.add('hidden');
      applyEl.classList.add('hidden');
    } else if (d.latest_version) {
      stEl.textContent = t('updateStatusLatest', { ver: d.app_version || d.latest_version });
      stEl.className = 'muted small';
      linkEl.classList.add('hidden');
      applyEl.classList.add('hidden');
    } else {
      stEl.classList.add('hidden');
      linkEl.classList.add('hidden');
      applyEl.classList.add('hidden');
    }
  }
}

async function runDesktopUpdateCheck(btn) {
  if (btn) btn.disabled = true;
  try {
    const r = await api('/api/desktop/update/check', { method: 'POST' });
    if (r && r.status) {
      renderDesktopStatus(r.status);
      if (r.status.update_available) {
        toast(t('toastUpdateFound', { ver: r.status.latest_version }));
      } else {
        toast(t('toastUpToDate', { ver: r.status.app_version || 'dev' }));
      }
    }
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function runDesktopUpdateApply(btn) {
  if (btn) btn.disabled = true;
  try {
    toast(t('toastUpdateStarting'));
    await api('/api/desktop/update/apply', { method: 'POST' });
    toast(t('toastUpdateRestarting'));
  } catch (err) {
    toast(err.message, 'err');
    if (btn) btn.disabled = false;
  }
}

$('btn-desktop-check-update').addEventListener('click', () => runDesktopUpdateCheck($('btn-desktop-check-update')));
$('btn-settings-check-update').addEventListener('click', () => runDesktopUpdateCheck($('btn-settings-check-update')));
$('btn-desktop-apply-update').addEventListener('click', () => runDesktopUpdateApply($('btn-desktop-apply-update')));
$('btn-settings-apply-update').addEventListener('click', () => runDesktopUpdateApply($('btn-settings-apply-update')));

function bytesToCacheInput(bytes) {
  const GiB = 1024 * 1024 * 1024;
  const MiB = 1024 * 1024;
  if (bytes > 0 && bytes % GiB === 0) return { val: bytes / GiB, unit: 'GB' };
  if (bytes > 0 && bytes % MiB === 0) return { val: bytes / MiB, unit: 'MB' };
  if (bytes > 0 && bytes < GiB) return { val: Number((bytes / MiB).toFixed(2)), unit: 'MB' };
  return { val: Number(((bytes || 10 * GiB) / GiB).toFixed(2)), unit: 'GB' };
}

function cacheInputToBytes(val, unit) {
  const num = Number(val);
  const mult = unit === 'GB' ? 1024 * 1024 * 1024 : 1024 * 1024;
  return Math.round(num * mult);
}

function normalizeDriveLetterInput(raw) {
  let s = String(raw || '').trim().replace(/[\\/]+$/, '');
  if (/^[A-Za-z]$/.test(s)) s = s.toUpperCase() + ':';
  else if (/^[A-Za-z]:$/.test(s)) s = s[0].toUpperCase() + ':';
  else throw new Error(t('errInvalidDrive'));
  if (s === 'C:') throw new Error(t('errReservedC'));
  return s;
}

function normalizePinnedPathsInput(text) {
  const out = [];
  const seen = new Set();
  for (const line of String(text || '').split(/\r?\n/)) {
    let p = line.trim();
    if (!p) continue;
    p = p.replace(/\\/g, '/').replace(/\/+/g, '/');
    if (p.split('/').includes('..')) throw new Error(t('errPinnedDotDot'));
    if (!p.startsWith('/')) p = '/' + p;
    if (p.length > 1) p = p.replace(/\/+$/, '');
    if (!seen.has(p)) {
      seen.add(p);
      out.push(p);
    }
  }
  return out;
}

let lastDriveListSig = '';
let lastDriveProbeAt = 0;

function populateDriveSelect(d, force) {
  const sel = $('set-desktop-drive');
  if (!sel) return;
  const cur = (d.drive_letter || 'V:').toUpperCase();
  const mounted = Boolean(d.mounted);
  const avail = Array.isArray(d.available_drives) ? d.available_drives.map((x) => x.toUpperCase()) : [];
  const used = new Set(Array.isArray(d.used_drives) ? d.used_drives.map((x) => x.toUpperCase()) : []);
  const allDrives = Array.from({ length: 23 }, (_, i) => String.fromCharCode(68 + i) + ':'); // D:..Z:
  if (!allDrives.includes(cur)) allDrives.unshift(cur);

  if (used.size === 0 && avail.length > 0) {
    const availSet = new Set(avail);
    for (const drv of allDrives) {
      if (!availSet.has(drv) && !(mounted && drv === cur)) {
        used.add(drv);
      }
    }
  }

  const sig = `${getLang()}|${cur}|${mounted}|${[...used].sort().join(',')}`;
  if (!force && sig === lastDriveListSig && sel.options.length > 0) {
    return;
  }
  const prevPicked = sel.value;
  lastDriveListSig = sig;
  sel.innerHTML = '';

  let firstFree = '';
  for (const drv of allDrives) {
    const opt = document.createElement('option');
    opt.value = drv;
    const isOwnMount = mounted && drv === cur;
    const isUsed = !isOwnMount && used.has(drv);
    if (isOwnMount) {
      opt.textContent = t('driveOptMounted', { drv });
    } else if (isUsed) {
      opt.textContent = drv === cur ? t('driveOptConflict', { drv }) : t('driveOptUsed', { drv });
      opt.disabled = true;
    } else {
      opt.textContent = t('driveOptFree', { drv });
      if (!firstFree) firstFree = drv;
    }
    sel.appendChild(opt);
  }

  const desired = !force && prevPicked && !used.has(prevPicked) ? prevPicked : cur;
  if (!used.has(desired) || (mounted && desired === cur)) {
    sel.value = desired;
  } else if (firstFree) {
    sel.value = firstFree;
  } else {
    sel.value = cur;
  }
}

async function refreshDriveDropdownLive() {
  if (!state.desktopEnabled) return;
  const now = Date.now();
  if (now - lastDriveProbeAt < 1500) return;
  lastDriveProbeAt = now;
  try {
    const d = await api('/api/desktop/status');
    state.lastDesktopStatus = d;
    populateDriveSelect(d, false);
  } catch (_) {}
}

$('set-desktop-drive').addEventListener('mouseenter', refreshDriveDropdownLive);
$('set-desktop-drive').addEventListener('focus', refreshDriveDropdownLive);

function populateDesktopSettingsForm(d) {
  state.lastDesktopStatus = d;
  populateDriveSelect(d, true);
  renderDesktopUpdateUI(d);
  const c = bytesToCacheInput(d.disk_cache_max_bytes || 10 * 1024 * 1024 * 1024);
  $('set-desktop-cache-val').value = c.val;
  $('set-desktop-cache-unit').value = c.unit;
  $('set-desktop-pinned').value = (d.pinned_paths || []).join('\n');
  $('set-desktop-startup').checked = Boolean(d.run_at_startup);
  $('set-desktop-automount').checked = d.auto_mount !== undefined ? Boolean(d.auto_mount) : true;
  $('set-desktop-autoupdate').checked = d.auto_update !== undefined ? Boolean(d.auto_update) : true;
}

async function loadDesktopSettings() {
  if (!state.desktopEnabled) return;
  try {
    const d = await api('/api/desktop/status');
    populateDesktopSettingsForm(d);
  } catch (err) {
    toast(err.message, 'err');
  }
}

$('form-desktop-settings').addEventListener('submit', async (e) => {
  e.preventDefault();
  const errBox = $('desktop-settings-err');
  errBox.classList.add('hidden');
  errBox.textContent = '';

  const btn = $('btn-save-desktop-settings');
  btn.disabled = true;
  try {
    const driveLetter = normalizeDriveLetterInput($('set-desktop-drive').value);
    const cacheMaxBytes = cacheInputToBytes($('set-desktop-cache-val').value, $('set-desktop-cache-unit').value);
    if (!Number.isFinite(cacheMaxBytes) || cacheMaxBytes <= 0) {
      throw new Error(t('errCachePositive'));
    }
    const pinnedPaths = normalizePinnedPathsInput($('set-desktop-pinned').value);

    const updated = await api('/api/desktop/settings', {
      method: 'PUT',
      body: JSON.stringify({
        drive_letter: driveLetter,
        disk_cache_max_bytes: cacheMaxBytes,
        pinned_paths: pinnedPaths,
        run_at_startup: $('set-desktop-startup').checked,
        auto_mount: $('set-desktop-automount').checked,
        auto_update: $('set-desktop-autoupdate').checked,
      }),
    });
    populateDesktopSettingsForm(updated);
    toast(t('toastDesktopSaved'));
  } catch (err) {
    errBox.textContent = err.message;
    errBox.classList.remove('hidden');
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
  }
});

boot();
