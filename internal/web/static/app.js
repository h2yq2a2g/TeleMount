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
    $('dash-title').textContent = isDesktop ? 'Tổng quan & Vận hành' : 'Tổng quan & Vận hành WebDAV';
  }
  if (!isDesktop && $('desktop-winfsp-banner')) {
    $('desktop-winfsp-banner').classList.add('hidden');
  }
}

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
  return new Date(unix * 1000).toLocaleString('vi-VN');
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
  try {
    const st = await api('/api/status');
    state.desktopEnabled = Boolean(st.desktop_enabled);
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
  if (p1 !== p2) return toast('Mật khẩu nhập lại không khớp', 'err');
  try {
    await api('/api/setup/admin', { method: 'POST', body: JSON.stringify({ username: u, password: p1 }) });
    toast('Đã tạo tài khoản Admin Web UI');
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
    toast('Đã kết nối MTProto tới Telegram');
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
    const t = btn.dataset.authTab;
    document.querySelectorAll('[data-auth-tab]').forEach((b) => b.classList.toggle('active', b === btn));
    $('auth-pane-qr').classList.toggle('hidden', t !== 'qr');
    $('auth-pane-phone').classList.toggle('hidden', t !== 'phone');
  });
});

function stopQRPoll() {
  if (state.qrPollTimer) { clearInterval(state.qrPollTimer); state.qrPollTimer = null; }
}

async function startQRLogin() {
  stopQRPoll();
  $('qr-holder').innerHTML = '<span class="muted">Đang tạo mã QR...</span>';
  try {
    const st = await api('/api/setup/telegram/qr/start', { method: 'POST' });
    renderQR(st);
    state.qrPollTimer = setInterval(async () => {
      try {
        const res = await api('/api/setup/telegram/qr/status');
        if (res.authorized || res.qr.status === 'authorized') {
          stopQRPoll();
          toast('Đăng nhập Telegram thành công!');
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
    $('qr-holder').innerHTML = `<span class="muted">${esc(qr.error || 'Lỗi QR')}</span>`;
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
    toast('Đã gửi mã OTP tới Telegram của anh');
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-verify-otp').addEventListener('click', async () => {
  try {
    const res = await api('/api/setup/telegram/phone/verify', { method: 'POST', body: JSON.stringify({ code: $('tg-otp').value }) });
    if (res.need_password) {
      $('auth-pane-2fa').classList.remove('hidden');
      toast('Vui lòng nhập mật khẩu 2FA Telegram');
      return;
    }
    toast('Đăng nhập Telegram thành công!');
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-verify-2fa').addEventListener('click', async () => {
  try {
    await api('/api/setup/telegram/password', { method: 'POST', body: JSON.stringify({ password: $('tg-2fa').value }) });
    toast('Xác thực 2FA thành công!');
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
});

// Channel selection
async function loadChannels() {
  $('channel-list').innerHTML = '<div class="muted">Đang tải danh sách Private Channel...</div>';
  try {
    const res = await api('/api/setup/channels');
    if (!res.channels || res.channels.length === 0) {
      $('channel-list').innerHTML = '<div class="muted">Chưa có Private Channel nào. Hãy tạo mới ở ô trên.</div>';
      return;
    }
    $('channel-list').innerHTML = '';
    res.channels.forEach((ch) => {
      const row = document.createElement('div');
      row.className = 'list-item';
      row.innerHTML = `<div><strong>${esc(ch.title)}</strong> <span class="muted small">(#${ch.id})</span></div>`;
      const btn = document.createElement('button');
      btn.className = 'btn btn-secondary small-btn';
      btn.textContent = 'Chọn';
      btn.onclick = async () => {
        try {
          await api('/api/setup/channels/select', { method: 'POST', body: JSON.stringify(ch) });
          toast(`Đã chọn channel "${ch.title}"`);
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
    toast(`Đã tạo channel "${res.channel.title}"`);
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
});

// Key Setup / Restore
document.querySelectorAll('[data-key-tab]').forEach((btn) => {
  btn.addEventListener('click', () => {
    const t = btn.dataset.keyTab;
    document.querySelectorAll('[data-key-tab]').forEach((b) => b.classList.toggle('active', b === btn));
    $('key-pane-fresh').classList.toggle('hidden', t !== 'fresh');
    $('key-pane-restore').classList.toggle('hidden', t !== 'restore');
    if (t === 'restore' && ($('restore-backup-select').options.length === 0 || $('restore-backup-select').value === '0')) {
      probeChannelBackupsForWizard();
    }
  });
});

async function probeChannelBackupsForWizard() {
  const sel = $('restore-backup-select');
  sel.innerHTML = '<option value="0">Đang quét bản backup trên Channel...</option>';
  $('existing-backup-notice').classList.add('hidden');
  try {
    const res = await api('/api/ops/backups');
    const list = res.backups || [];
    sel.innerHTML = '';
    if (list.length > 0) {
      $('existing-backup-notice').textContent = `Phát hiện ${list.length} bản backup trên Channel này! Anh có thể sang tab "Khôi phục từ Channel cũ" và nhập Encryption Key cũ.`;
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
      opt.textContent = '(Chưa tìm thấy bản backup .tmdb / .tddb nào trong Channel)';
      sel.appendChild(opt);
    }
  } catch (err) {
    sel.innerHTML = '';
    const opt = document.createElement('option');
    opt.value = '0';
    opt.textContent = `(Lỗi tải danh sách backup: ${err.message})`;
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
  if (!v) return toast('Chưa có key để copy', 'err');
  navigator.clipboard.writeText(v);
  toast('Đã copy Encryption Key vào clipboard');
});

$('btn-dl-key').addEventListener('click', () => {
  const v = $('fresh-key-input').value;
  if (!v) return toast('Chưa có key để tải', 'err');
  const blob = new Blob([`TeleMount Encryption Key\nCreated: ${new Date().toISOString()}\nKey: ${v}\n`], { type: 'text/plain' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = 'telemount-encryption-key.txt';
  a.click();
});

$('btn-init-key').addEventListener('click', async () => {
  const passphrase = $('fresh-key-input').value;
  if (passphrase.length < 4) return toast('Encryption Key cần ít nhất 4 ký tự', 'err');
  if (!$('chk-saved-key').checked) return toast('Vui lòng tick xác nhận anh đã cất giữ Encryption Key', 'err');
  const btn = $('btn-init-key');
  btn.disabled = true;
  try {
    await api('/api/setup/key/init', { method: 'POST', body: JSON.stringify({ passphrase }) });
    toast('Đã băm & khởi tạo Encryption Key thành công!');
    await syncWizard();
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});

$('btn-restore-key').addEventListener('click', async () => {
  const passphrase = $('restore-key-input').value;
  const msg_id = Number($('restore-backup-select').value || 0);
  if (!passphrase) return toast('Vui lòng nhập Encryption Key cũ', 'err');
  const btn = $('btn-restore-key');
  btn.disabled = true;
  try {
    const res = await api('/api/setup/key/restore', { method: 'POST', body: JSON.stringify({ passphrase, msg_id }) });
    toast(`Khôi phục thành công! (${res.stats ? res.stats.file_count : 0} tệp)`);
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
    toast('Hoàn tất thiết lập TeleMount!');
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
    $('sidebar-channel').textContent = d.channel?.title || 'Chưa chọn channel';
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
      $('kpi-parts').textContent = `Mã hóa: ${fmtBytes(d.stats.total_cipher_size)} (${d.stats.part_count} parts)`;
      $('kpi-files').textContent = `${d.stats.file_count} tệp`;
      $('kpi-folders').textContent = `${d.stats.folder_count} thư mục`;

      const pk = d.stats.packs || {};
      const stg = d.packer || {};
      $('pack-summary-line').textContent =
        `Đang gom trên RAM: ${stg.buffered_files || 0} tệp (${fmtBytes(stg.buffered_bytes || 0)})` +
        (stg.inflight_packs ? ` · Đang đẩy: ${stg.inflight_packs} pack` : '') +
        `  |  Trên Telegram: ${pk.pack_count || 0} packs (Sống: ${fmtBytes(pk.live_pack_bytes || 0)} / Tổng: ${fmtBytes(pk.total_pack_bytes || 0)} · Rác chờ compact: ${fmtBytes(pk.dead_pack_bytes || 0)})`;
    }
    if (d.cache) {
      const usedMB = (d.cache.used_bytes / 1048576).toFixed(1);
      const maxMB = (d.cache.max_bytes / 1048576).toFixed(0);
      const total = (d.cache.hits || 0) + (d.cache.misses || 0);
      const rate = total > 0 ? Math.round((d.cache.hits / total) * 100) : 0;
      $('kpi-cache').textContent = `${usedMB} / ${maxMB} MB`;
      $('kpi-cache-hit').textContent = `Hit: ${rate}% (${d.cache.entries} chunks)`;
    }
    $('kpi-tg').textContent = d.telegram?.authorized ? 'Online' : 'Offline';
    $('kpi-mem').textContent = `Go RAM: ${fmtBytes(d.mem_alloc_bytes)} · Uptime ${Math.floor((d.uptime_sec || 0) / 60)}m`;

    const trBox = $('active-transfers');
    const trs = d.telegram?.active_transfers || [];
    if (!trs.length) {
      trBox.innerHTML = 'Không có luồng nào đang chạy.';
    } else {
      trBox.innerHTML = trs.map((t) => {
        const pct = t.total > 0 ? Math.min(100, Math.round((t.done / t.total) * 100)) : 0;
        return `<div class="list-item">
          <div style="flex:1">
            <div>${t.kind === 'upload' ? '⬆' : '⬇'} <strong>${esc(t.name)}</strong> (${fmtBytes(t.done)} / ${t.total > 0 ? fmtBytes(t.total) : '?'} · ${fmtBytes(t.speed_bps)}/s)</div>
            <div class="progress-track"><div class="progress-fill" style="width:${pct}%"></div></div>
          </div>
        </div>`;
      }).join('');
    }

    const flBox = $('flood-events');
    const fls = d.telegram?.recent_floods || [];
    if (!fls.length) {
      flBox.innerHTML = 'Chưa ghi nhận FloodWait.';
    } else {
      flBox.innerHTML = fls.slice(0, 8).map((f) =>
        `<div class="list-item"><span><code>${esc(f.method)}</code> — chờ <strong>${f.duration_sec}s</strong></span><span class="muted small">${fmtTime(f.at)}</span></div>`
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
  try { await api('/api/ops/cache/clear', { method: 'POST' }); toast('Đã giải phóng RAM cache'); loadDashboard(); }
  catch (err) { toast(err.message, 'err'); }
});
$('btn-flush-pack').addEventListener('click', async () => {
  const btn = $('btn-flush-pack');
  btn.disabled = true;
  try {
    await api('/api/ops/packs/flush', { method: 'POST' });
    toast('Đã đẩy toàn bộ RAM Pack lên Telegram!');
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
      toast('Các Pack hiện đã tối ưu, chưa cần compact thêm');
    } else {
      toast(`Đã compact ${r.compacted_packs} pack -> ${r.created_packs} pack mới, thu hồi ${fmtBytes(r.reclaimed_bytes)}!`);
    }
    loadDashboard();
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});

async function loadRcloneSnippet() {
  try {
    const r = await api('/api/ops/rclone-config');
    state.rcloneConf = r.rclone_conf;
    $('rclone-int-url').textContent = r.internal_url;
    $('rclone-ext-url').textContent = r.external_url;
    $('rclone-conf-box').textContent = r.rclone_conf;
    $('rclone-mount-box').textContent = r.mount_cmd;
  } catch (_) {}
}

$('btn-copy-rclone').addEventListener('click', () => {
  navigator.clipboard.writeText(state.rcloneConf || '');
  toast('Đã copy snippet rclone.conf');
});

$('btn-scan-orphans').addEventListener('click', async () => {
  $('orphan-result').textContent = 'Đang quét lịch sử Telegram Channel...';
  try {
    const r = await api('/api/ops/orphans');
    state.orphanIds = (r.orphans || []).map((o) => o.msg_id);
    $('btn-clean-orphans').classList.toggle('hidden', state.orphanIds.length === 0);
    if (!state.orphanIds.length) {
      $('orphan-result').textContent = `Đã quét ${r.scanned_docs} document trên Channel — không có blob thừa nào!`;
    } else {
      $('orphan-result').textContent = `Đã quét ${r.scanned_docs} document — phát hiện ${state.orphanIds.length} blob thừa (${fmtBytes(r.total_bytes)}).`;
    }
  } catch (err) {
    $('orphan-result').textContent = `Lỗi: ${err.message}`;
  }
});

$('btn-clean-orphans').addEventListener('click', async () => {
  if (!state.orphanIds.length) return;
  if (!confirm(`Xóa ${state.orphanIds.length} blob thừa khỏi Telegram Channel?`)) return;
  try {
    const r = await api('/api/ops/orphans/clean', { method: 'POST', body: JSON.stringify({ msg_ids: state.orphanIds }) });
    toast(`Đã dọn ${r.deleted} blob thừa trên Telegram`);
    state.orphanIds = [];
    $('btn-clean-orphans').classList.add('hidden');
    $('orphan-result').textContent = 'Đã dọn sạch.';
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
      ? `Lần backup gần nhất: ${fmtTime(s.last_backup_at)} (msg #${s.last_backup_msg_id})`
      : 'Chưa có bản backup nào.';
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
    toast('Đã lưu cài đặt Hybrid Pack & RAM cache');
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
    toast('Đã cập nhật tài khoản WebDAV');
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
    toast('Đã đổi mật khẩu Admin Web UI');
  } catch (err) { toast(err.message, 'err'); }
});

$('btn-backup-now').addEventListener('click', async () => {
  const btn = $('btn-backup-now');
  btn.disabled = true;
  try {
    const r = await api('/api/ops/backup', { method: 'POST' });
    toast(`Đã backup DB mã hóa lên Telegram (msg #${r.msg_id}, ${fmtBytes(r.size)})`);
    loadSettings();
    loadBackupsInSettings();
  } catch (err) { toast(err.message, 'err'); }
  finally { btn.disabled = false; }
});

async function loadBackupsInSettings() {
  const box = $('backup-list');
  const sel = $('settings-restore-select');
  box.innerHTML = '<div class="muted small">Đang tải...</div>';
  sel.innerHTML = '';
  try {
    const r = await api('/api/ops/backups');
    const list = r.backups || [];
    if (!list.length) {
      box.innerHTML = '<div class="muted small">Chưa có bản backup nào trên Channel.</div>';
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
  if (!passphrase) return toast('Nhập Encryption Key để xác nhận khôi phục', 'err');
  if (!confirm('Khôi phục DB sẽ ghi đè cây thư mục hiện tại bằng bản backup đã chọn. Tiếp tục?')) return;
  try {
    await api('/api/ops/restore', { method: 'POST', body: JSON.stringify({ msg_id, passphrase }) });
    $('settings-restore-key').value = '';
    toast('Khôi phục DB thành công!');
    loadSettings();
  } catch (err) { toast(err.message, 'err'); }
});

// --- Multi-Account Telegram Backup Mirroring ---
function fmtTGUser(u) {
  if (!u) return 'Chưa rõ';
  const name = [u.first_name, u.last_name].filter(Boolean).join(' ') || 'User';
  return u.username ? `${name} (@${u.username})` : (u.phone ? `${name} (+${u.phone})` : name);
}

async function loadAccounts() {
  const box = $('accounts-box');
  try {
    const res = await api('/api/accounts');
    const p = res.primary || {};
    const backups = res.backups || [];

    let html = `
      <div class="list-item" style="border-color:rgba(34,197,94,0.45)">
        <div>
          <strong>⭐ Nick Chính: ${esc(fmtTGUser(p.user))}</strong>
          <div class="muted small">Channel chính: <strong>${esc(p.channel_title || '—')}</strong> (#${p.channel_id || 0}) · Trạng thái: ${p.authorized ? 'Online' : 'Offline'} · Tổng gốc: ${p.total_blobs || 0} blobs</div>
        </div>
        <span class="status-pill ${p.authorized ? 'ok' : 'err'}">${p.authorized ? '● Đang làm chính' : '● Mất kết nối'}</span>
      </div>
    `;

    if (!backups.length) {
      html += `<div class="muted small">Chưa có tài khoản Backup nào. Bấm <strong>+ Thêm nick Backup</strong> để nhân bản dữ liệu dự phòng.</div>`;
    } else {
      backups.forEach((b) => {
        const pct = b.total_blobs > 0 ? Math.min(100, Math.round((b.synced_blobs / b.total_blobs) * 100)) : 100;
        html += `
          <div class="list-item">
            <div style="flex:1">
              <strong>🛡️ Backup: ${esc(fmtTGUser(b.user))} <span class="muted small">(${esc(b.id)})</span></strong>
              <div class="muted small">Channel riêng: <strong>${esc(b.own_channel_title || 'Chưa tạo')}</strong> · Đã mirror: <strong>${b.synced_blobs}/${b.total_blobs}</strong> blobs (${pct}%) · ${b.authorized ? 'Online' : b.connection_state}</div>
            </div>
            <div class="row-inline">
              <button type="button" class="btn btn-secondary small-btn" data-promote="${esc(b.id)}">Đặt làm Nick Chính</button>
              <button type="button" class="btn btn-danger small-btn" data-del-acc="${esc(b.id)}">Gỡ</button>
            </div>
          </div>
        `;
      });
    }
    box.innerHTML = html;

    box.querySelectorAll('[data-promote]').forEach((btn) => {
      btn.onclick = async () => {
        const id = btn.dataset.promote;
        if (!confirm(`Chuyển tài khoản Backup (${id}) và Channel riêng của nó lên làm Nick Chính? (Dùng khi nick chính bị khóa hoặc muốn đổi nick chính)`)) return;
        btn.disabled = true;
        try {
          const r = await api(`/api/accounts/backup/${id}/promote`, { method: 'POST' });
          toast(`Đã chuyển sang Nick Chính mới (Channel: ${r.new_channel})!`);
          loadAccounts();
          loadDashboard();
        } catch (err) { toast(err.message, 'err'); }
        finally { btn.disabled = false; }
      };
    });

    box.querySelectorAll('[data-del-acc]').forEach((btn) => {
      btn.onclick = async () => {
        const id = btn.dataset.delAcc;
        if (!confirm(`Gỡ tài khoản Backup (${id}) khỏi hệ thống? (Các blob đã copy trên Channel riêng của nick đó vẫn được giữ nguyên trên Telegram)`)) return;
        try {
          await api(`/api/accounts/backup/${id}`, { method: 'DELETE' });
          toast('Đã gỡ tài khoản Backup');
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
      toast(`Đã đồng bộ ${r.synced_blobs} blobs (Lưu ý: ${r.errors.join('; ')})`, 'err');
    } else {
      toast(`Đã đồng bộ xong (${r.synced_blobs} blobs mới được copy sang Backup)!`);
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
    const t = btn.dataset.baccTab;
    document.querySelectorAll('[data-bacc-tab]').forEach((b) => b.classList.toggle('active', b === btn));
    $('bacc-pane-qr').classList.toggle('hidden', t !== 'qr');
    $('bacc-pane-phone').classList.toggle('hidden', t !== 'phone');
  });
});

$('btn-add-backup-acc').addEventListener('click', async () => {
  $('backup-acc-modal').classList.remove('hidden');
  $('bacc-pane-2fa').classList.add('hidden');
  $('bacc-qr-holder').innerHTML = '<span class="muted">Đang kết nối MTProto...</span>';
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
  $('bacc-qr-holder').innerHTML = '<span class="muted">Đang tạo mã QR...</span>';
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
    toast('Đã gửi mã OTP tới nick Backup');
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
      toast('Nhập thêm mật khẩu 2FA của nick Backup');
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
  $('bacc-qr-holder').innerHTML = '<span class="muted">Đang tạo Channel riêng &amp; liên kết đồng bộ...</span>';
  try {
    const r = await api(`/api/accounts/backup/${state.baccPendingId}/finalize`, { method: 'POST' });
    closeBaccModal();
    toast(`Đã thêm nick Backup & tạo channel "${r.account.own_channel_title}" — đang tự đồng bộ dữ liệu!`);
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
  const winfspOk = Boolean(d.winfsp_installed);
  $('desktop-winfsp-banner').classList.toggle('hidden', winfspOk);
  $('desktop-winfsp-guidance').textContent =
    d.winfsp_guidance || 'Cài đặt WinFsp từ winfsp.dev hoặc chạy winget install WinFsp.WinFsp để bật ổ đĩa ảo.';

  $('desktop-drive-letter').textContent = d.drive_letter || 'V:';
  const badge = $('desktop-mount-badge');
  badge.textContent = d.mounted ? '● Đã gắn (Mounted)' : '● Chưa gắn (Unmounted)';
  badge.className = `status-pill ${d.mounted ? 'ok' : 'err'}`;
  $('desktop-startup-badge').textContent = d.run_at_startup
    ? '· Tự khởi động Windows: Bật'
    : '· Tự khởi động Windows: Tắt';

  const mErrEl = $('desktop-mount-error');
  mErrEl.textContent = d.mount_error || '';
  mErrEl.classList.toggle('hidden', !d.mount_error);

  $('btn-desktop-mount').disabled = Boolean(d.mounted || !winfspOk);
  $('btn-desktop-unmount').disabled = !d.mounted;
  $('btn-desktop-open').disabled = !d.mounted;

  const qState = $('desktop-queue-state');
  const qCount = d.queue_pending_count || 0;
  if (d.queue_syncing) {
    qState.textContent = '● Đang đồng bộ...';
    qState.className = 'status-pill warn';
  } else if (qCount > 0) {
    qState.textContent = '● Chờ đồng bộ';
    qState.className = 'status-pill warn';
  } else {
    qState.textContent = '● Đã đồng bộ';
    qState.className = 'status-pill ok';
  }
  $('desktop-queue-summary').textContent = `Chờ đồng bộ: ${qCount} tác vụ (${fmtBytes(d.queue_pending_bytes || 0)})`;
  const qErrEl = $('desktop-queue-error');
  qErrEl.textContent = d.queue_last_error || '';
  qErrEl.classList.toggle('hidden', !d.queue_last_error);

  const used = d.disk_cache_bytes || 0;
  const max = d.disk_cache_max_bytes || 0;
  const pct = max > 0 ? Math.min(100, Math.round((used / max) * 100)) : 0;
  $('desktop-disk-cache-usage').textContent = `${fmtBytes(used)} / ${fmtBytes(max)}`;
  $('desktop-disk-cache-bar').style.width = `${pct}%`;
  $('desktop-disk-cache-meta').textContent =
    `Đã ghim (Pinned): ${fmtBytes(d.disk_cache_pinned_bytes || 0)} · ${d.disk_cache_entries || 0} mục`;
}

$('btn-copy-winget').addEventListener('click', () => {
  const cmd = $('winget-winfsp-cmd').textContent || 'winget install WinFsp.WinFsp';
  if (navigator.clipboard) navigator.clipboard.writeText(cmd);
  toast('Đã copy lệnh winget cài đặt WinFsp');
});

$('btn-desktop-mount').addEventListener('click', async () => {
  const btn = $('btn-desktop-mount');
  btn.disabled = true;
  try {
    const r = await api('/api/desktop/mount', { method: 'POST' });
    if (r && r.status) renderDesktopStatus(r.status);
    toast('Đã gắn ổ đĩa ảo TeleMount!');
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
    toast('Đã ngắt ổ đĩa ảo TeleMount');
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
    toast('Đã đồng bộ hàng đợi ghi offline!');
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
    toast('Đã xóa cache đĩa chưa ghim');
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
  }
});

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
  else throw new Error('Chữ cái ổ đĩa không hợp lệ (ví dụ: V:)');
  if (s === 'C:') throw new Error('Không thể dùng ổ đĩa hệ thống C:');
  return s;
}

function normalizePinnedPathsInput(text) {
  const out = [];
  const seen = new Set();
  for (const line of String(text || '').split(/\r?\n/)) {
    let p = line.trim();
    if (!p) continue;
    p = p.replace(/\\/g, '/').replace(/\/+/g, '/');
    if (p.split('/').includes('..')) throw new Error('Đường dẫn ghim không được chứa ".."');
    if (!p.startsWith('/')) p = '/' + p;
    if (p.length > 1) p = p.replace(/\/+$/, '');
    if (!seen.has(p)) {
      seen.add(p);
      out.push(p);
    }
  }
  return out;
}

function populateDesktopSettingsForm(d) {
  $('set-desktop-drive').value = d.drive_letter || 'V:';
  const c = bytesToCacheInput(d.disk_cache_max_bytes || 10 * 1024 * 1024 * 1024);
  $('set-desktop-cache-val').value = c.val;
  $('set-desktop-cache-unit').value = c.unit;
  $('set-desktop-pinned').value = (d.pinned_paths || []).join('\n');
  $('set-desktop-startup').checked = Boolean(d.run_at_startup);
  $('set-desktop-automount').checked = d.auto_mount !== undefined ? Boolean(d.auto_mount) : true;
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
      throw new Error('Giới hạn Disk Cache phải lớn hơn 0');
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
      }),
    });
    populateDesktopSettingsForm(updated);
    toast('Đã lưu cài đặt Desktop');
  } catch (err) {
    errBox.textContent = err.message;
    errBox.classList.remove('hidden');
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
  }
});

boot();
