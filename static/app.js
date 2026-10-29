const base = window.BASE_PATH || '';
const $ = (id) => document.getElementById(id);

const MODE_LABEL = {
    smart: 'Auto', random: 'Random', wall_follow: 'Edges', wallfollow: 'Edges', spiral: 'Spot (spiral)',
    sprial: 'Spot (spiral)', single: 'Single', mop: 'Mop', idle: 'Idle',
    left_spiral: 'Left spiral', right_spiral: 'Right spiral', chargego: 'Docking', standby: 'Standby',
};
const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

let vacs = [];
let hasDB = false;
let lastPhase = '';
let editingId = null;

async function api(path, options = {}) {
    const res = await fetch(base + path, {
        ...options,
        headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
    });
    let body = {};
    try { body = await res.json(); } catch (_) { /* non-JSON error page */ }
    if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`);
    return body;
}

const current = () => vacs.find((v) => v.id === $('vacSelect').value);
const label = (m) => MODE_LABEL[m] || m || '—';
const cap = (s) => (s ? s[0].toUpperCase() + s.slice(1) : '—');

function setMsg(el, text, ok) {
    el.textContent = text;
    el.className = 'msg ' + (ok === undefined ? '' : ok ? 'ok' : 'bad');
}

function option(value, text) {
    const o = document.createElement('option');
    o.value = value;
    o.textContent = text;
    return o;
}

function cell(text, cls) {
    const td = document.createElement('td');
    td.textContent = text;
    if (cls) td.className = cls;
    return td;
}

function fmtWhen(iso) {
    if (!iso) return '—';
    const d = new Date(iso);
    const today = new Date().toDateString() === d.toDateString();
    return today
        ? 'Today ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
        : d.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}

function fmtDuration(ms) {
    const m = Math.round(ms / 60000);
    return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

function fmtDays(mask) {
    if (mask === 127) return 'Every day';
    if (mask === 62) return 'Weekdays';
    if (mask === 65) return 'Weekends';
    return DAYS.filter((_, i) => mask & (1 << i)).join(' ');
}

// ---- Status and controls ----

function buildControls() {
    const v = current();
    if (!v) return;
    const fill = (sel, values, blank) => {
        sel.replaceChildren();
        if (blank) sel.appendChild(option('', blank));
        values.forEach((x) => sel.appendChild(option(x, sel.id.includes('uction') ? cap(x) : label(x))));
    };
    fill($('modeSelect'), v.modes);
    fill($('suctionSelect'), v.suction);
    fill($('sfMode'), v.modes);
    fill($('sfSuction'), v.suction, 'Leave as is');
    $('suctionWrap').classList.toggle('hidden', !v.can_suction);
    $('locateBtn').classList.toggle('hidden', !v.can_locate);
}

// What the Play button does now, like the app's play/pause button
let playAction = 'start';

function setPlay(st) {
    const v = current();
    if (st.phase === 'cleaning') {
        playAction = 'pause';
        $('playBtn').textContent = '⏸ Pause';
        $('playBtn').disabled = !st.connected || !v.can_pause;
    } else {
        playAction = st.phase === 'paused' ? 'resume' : 'start';
        $('playBtn').textContent = st.phase === 'paused' ? '▶ Resume' : '▶ Start';
        $('playBtn').disabled = !st.connected;
    }
}

function play() {
    if (playAction === 'start') command('start', $('modeSelect').value);
    else command(playAction);
}

async function refreshStatus() {
    const v = current();
    if (!v) return;
    let data;
    try {
        data = await api(`/api/vacuums/${v.id}/status`);
    } catch (e) {
        setMsg($('ctlMsg'), e.message, false);
        return;
    }
    const st = data.state;
    const PHASE_CLASS = { cleaning: 'ok', paused: 'warn' };
    const [text, cls] = st.connected ? [st.status_label || 'Unknown', PHASE_CLASS[st.phase] || ''] : ['Offline', 'bad'];
    $('stState').textContent = text + (st.error_code ? ` (fault ${st.error_code})` : '');
    $('stState').className = 'v ' + (st.error_code ? 'bad' : cls);

    const b = st.battery;
    $('stBattery').textContent = b == null ? '—' : `${b}%`;
    $('stBatteryBar').style.width = `${b || 0}%`;
    $('stBatteryBar').className = b != null && b < 20 ? 'low' : '';
    $('stMode').textContent = label(st.mode);
    $('stSuction').textContent = cap(st.suction);

    const run = data.current;
    if (run) {
        const area = run.area != null ? ` · ${run.area} m²` : '';
        $('stRun').textContent = fmtDuration(Date.now() - new Date(run.started_at)) + area;
    } else {
        $('stRun').textContent = '—';
    }

    $('seenNote').textContent = st.connected
        ? ''
        : st.last_error ? `· ${st.last_error}` : '· connecting…';

    if (st.suction && document.activeElement !== $('suctionSelect')) $('suctionSelect').value = st.suction;
    ['homeBtn', 'locateBtn', 'suctionSelect'].forEach((id) => { $(id).disabled = !st.connected; });
    setPlay(st);

    // A clean just ended: show it in the history
    if (lastPhase && (lastPhase === 'cleaning') !== (st.phase === 'cleaning') && hasDB) loadHistory();
    lastPhase = st.phase;
}

async function command(action, value = '') {
    const v = current();
    const buttons = document.querySelectorAll('.controls .btn, .controls select');
    buttons.forEach((b) => { b.disabled = true; });
    try {
        await api(`/api/vacuums/${v.id}/command`, { method: 'POST', body: JSON.stringify({ action, value }) });
        setMsg($('ctlMsg'), 'Sent.', true);
        setTimeout(() => { if ($('ctlMsg').textContent === 'Sent.') setMsg($('ctlMsg'), ''); }, 3000);
    } catch (e) {
        setMsg($('ctlMsg'), e.message, false);
    } finally {
        buttons.forEach((b) => { b.disabled = false; });
        setTimeout(refreshStatus, 800);
    }
}

// ---- Schedules ----

function buildDayBoxes(mask) {
    const wrap = $('sfDays');
    wrap.replaceChildren();
    // Monday first, as people read a week
    [1, 2, 3, 4, 5, 6, 0].forEach((i) => {
        const l = document.createElement('label');
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.value = String(i);
        cb.checked = Boolean(mask & (1 << i));
        l.append(cb, DAYS[i]);
        wrap.appendChild(l);
    });
}

function openForm(sc) {
    editingId = sc ? sc.id : null;
    $('sfName').value = sc ? sc.name : '';
    $('sfAt').value = sc ? sc.at : '09:00';
    $('sfMode').value = sc ? sc.mode : current().modes[0];
    $('sfSuction').value = sc ? sc.suction : '';
    buildDayBoxes(sc ? sc.days : 62);
    $('sfSave').textContent = sc ? 'Save changes' : 'Add schedule';
    $('schedForm').classList.remove('hidden');
    $('sfName').focus();
}

function closeForm() {
    editingId = null;
    $('schedForm').classList.add('hidden');
}

function formSchedule(enabled = true) {
    let days = 0;
    $('sfDays').querySelectorAll('input:checked').forEach((cb) => { days |= 1 << Number(cb.value); });
    return {
        name: $('sfName').value.trim(),
        at: $('sfAt').value,
        mode: $('sfMode').value,
        suction: $('sfSuction').value,
        days,
        enabled,
    };
}

async function saveSchedule(ev) {
    ev.preventDefault();
    const v = current();
    try {
        if (editingId) {
            const old = (window.schedules || []).find((s) => s.id === editingId);
            await api(`/api/schedules/${editingId}`, { method: 'PUT', body: JSON.stringify(formSchedule(old ? old.enabled : true)) });
        } else {
            await api(`/api/vacuums/${v.id}/schedules`, { method: 'POST', body: JSON.stringify(formSchedule()) });
        }
        closeForm();
        setMsg($('schedMsg'), 'Saved.', true);
        loadSchedules();
    } catch (e) {
        setMsg($('schedMsg'), e.message, false);
    }
}

async function toggleSchedule(sc, enabled) {
    try {
        await api(`/api/schedules/${sc.id}`, { method: 'PUT', body: JSON.stringify({ ...sc, enabled }) });
        setMsg($('schedMsg'), enabled ? `"${sc.name}" is on.` : `"${sc.name}" is off.`, true);
    } catch (e) {
        setMsg($('schedMsg'), e.message, false);
    }
    loadSchedules();
}

async function deleteSchedule(sc) {
    if (!confirm(`Delete the schedule "${sc.name}"?`)) return;
    try {
        await api(`/api/schedules/${sc.id}`, { method: 'DELETE' });
        setMsg($('schedMsg'), 'Deleted.', true);
    } catch (e) {
        setMsg($('schedMsg'), e.message, false);
    }
    loadSchedules();
}

async function loadSchedules() {
    const v = current();
    if (!v || !hasDB) return;
    let data;
    try {
        data = await api(`/api/vacuums/${v.id}/schedules`);
    } catch (e) {
        setMsg($('schedMsg'), e.message, false);
        return;
    }
    window.schedules = data.schedules;
    $('tzNote').textContent = `Times are the Pi's local time (${data.timezone})`;
    const rows = $('schedRows');
    rows.replaceChildren();
    if (!data.schedules.length) {
        const tr = document.createElement('tr');
        const td = cell('No schedules yet.', 'empty');
        td.colSpan = 6;
        tr.appendChild(td);
        rows.appendChild(tr);
        return;
    }
    for (const sc of data.schedules) {
        const tr = document.createElement('tr');
        tr.append(
            cell(sc.name),
            cell(`${fmtDays(sc.days)} at ${sc.at}`, 'nowrap'),
            cell(label(sc.mode) + (sc.suction ? `, ${sc.suction}` : '')),
            cell(fmtWhen(sc.last_run), 'nowrap'),
        );
        const on = document.createElement('td');
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.checked = sc.enabled;
        cb.title = sc.enabled ? 'Turn off' : 'Turn on';
        cb.addEventListener('change', () => toggleSchedule(sc, cb.checked));
        on.appendChild(cb);

        const act = document.createElement('td');
        act.className = 'nowrap';
        const edit = document.createElement('button');
        edit.className = 'btn secondary small';
        edit.textContent = 'Edit';
        edit.addEventListener('click', () => openForm(sc));
        const del = document.createElement('button');
        del.className = 'btn danger small';
        del.textContent = 'Delete';
        del.style.marginLeft = '6px';
        del.addEventListener('click', () => deleteSchedule(sc));
        act.append(edit, del);

        tr.append(on, act);
        rows.appendChild(tr);
    }
}

// ---- History ----

async function loadHistory() {
    const v = current();
    if (!v || !hasDB) return;
    let data;
    try {
        data = await api(`/api/vacuums/${v.id}/history`);
    } catch (_) {
        return;
    }

    const cleans = $('cleanRows');
    cleans.replaceChildren();
    if (!data.cleans.length) {
        const tr = document.createElement('tr');
        const td = cell('No cleans recorded yet.', 'empty');
        td.colSpan = 7;
        tr.appendChild(td);
        cleans.appendChild(tr);
    }
    for (const c of data.cleans) {
        const tr = document.createElement('tr');
        const len = c.ended_at ? fmtDuration(new Date(c.ended_at) - new Date(c.started_at)) : '—';
        const batt = c.battery_start != null && c.battery_end != null ? `${c.battery_start}% → ${c.battery_end}%` : '—';
        const end = (c.end_status || '—') + (c.error_code ? ` · fault ${c.error_code}` : '');
        tr.append(
            cell(fmtWhen(c.started_at), 'nowrap'),
            cell(len, 'nowrap'),
            cell(label(c.mode)),
            cell(c.area != null ? `${c.area} m²` : '—', 'nowrap'),
            cell(batt, 'nowrap'),
            cell(c.started_by),
            cell(end, c.error_code ? 'result-bad' : ''),
        );
        cleans.appendChild(tr);
    }

    const cmds = $('commandRows');
    cmds.replaceChildren();
    if (!data.commands.length) {
        const tr = document.createElement('tr');
        const td = cell('Nothing sent yet.', 'empty');
        td.colSpan = 4;
        tr.appendChild(td);
        cmds.appendChild(tr);
    }
    for (const e of data.commands) {
        const tr = document.createElement('tr');
        tr.append(
            cell(fmtWhen(e.at), 'nowrap'),
            cell(e.user),
            cell(e.detail || e.action),
            cell(e.ok ? 'OK' : e.error || 'Failed', e.ok ? 'result-ok' : 'result-bad'),
        );
        cmds.appendChild(tr);
    }
}

function showTab(which) {
    $('tabCleans').classList.toggle('active', which === 'cleans');
    $('tabCommands').classList.toggle('active', which === 'commands');
    $('cleansWrap').classList.toggle('hidden', which !== 'cleans');
    $('commandsWrap').classList.toggle('hidden', which !== 'commands');
}

// ---- Start-up ----

function selectVacuum() {
    try { localStorage.setItem('vac', $('vacSelect').value); } catch (_) { /* private mode */ }
    closeForm();
    lastPhase = '';
    buildControls();
    refreshStatus();
    loadSchedules();
    loadHistory();
}

async function init() {
    let data;
    try {
        data = await api('/api/vacuums');
    } catch (e) {
        setMsg($('ctlMsg'), e.message, false);
        return;
    }
    vacs = data.vacuums;
    hasDB = data.database;
    if (!vacs.length) {
        setMsg($('ctlMsg'), 'No vacuums configured: add one to config.yaml.', false);
        return;
    }
    $('schedPanel').classList.toggle('hidden', !hasDB);
    $('historyPanel').classList.toggle('hidden', !hasDB);
    $('noDbPanel').classList.toggle('hidden', hasDB);

    const sel = $('vacSelect');
    vacs.forEach((v) => sel.appendChild(option(v.id, v.name)));
    let saved = null;
    try { saved = localStorage.getItem('vac'); } catch (_) { /* private mode */ }
    if (saved && vacs.some((v) => v.id === saved)) sel.value = saved;
    sel.classList.toggle('hidden', vacs.length < 2);
    sel.addEventListener('change', selectVacuum);

    $('playBtn').addEventListener('click', play);
    $('homeBtn').addEventListener('click', () => command('dock'));
    $('locateBtn').addEventListener('click', () => command('locate'));
    $('suctionSelect').addEventListener('change', () => command('suction', $('suctionSelect').value));

    $('newSchedBtn').addEventListener('click', () => openForm(null));
    $('sfCancel').addEventListener('click', closeForm);
    $('schedForm').addEventListener('submit', saveSchedule);
    $('tabCleans').addEventListener('click', () => showTab('cleans'));
    $('tabCommands').addEventListener('click', () => { showTab('commands'); loadHistory(); });

    selectVacuum();
    setInterval(refreshStatus, 3000);
    setInterval(() => { loadSchedules(); loadHistory(); }, 60000);
}

init();
