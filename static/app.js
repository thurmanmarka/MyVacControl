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

const fmtMinutes = (m) => fmtDuration(m * 60000);

// A "YYYY-MM-DD" date as a local day, e.g. "Mon, Oct 26"
function fmtDate(ymd, withWeekday = true) {
    return new Date(ymd + 'T00:00').toLocaleDateString([], {
        ...(withWeekday ? { weekday: 'short' } : {}), month: 'short', day: 'numeric',
    });
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
    $('driveBtn').classList.toggle('hidden', !v.can_drive);
    $('driveWrap').classList.add('hidden');
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
    $('stState').textContent = text + (st.error_code ? ' (fault)' : '');
    $('stState').className = 'v ' + (st.error_code ? 'bad' : cls);
    $('faultNote').textContent = st.faults ? '⚠️ ' + st.faults.join(' · ') : '';
    $('faultNote').classList.toggle('hidden', !st.faults);

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
    ['homeBtn', 'locateBtn', 'suctionSelect', 'driveBtn'].forEach((id) => { $(id).disabled = !st.connected; });
    document.querySelectorAll('#dpad button').forEach((b) => { b.disabled = !st.connected; });
    if (!st.connected) driveEnd();
    setPlay(st);

    // A clean just started or ended: show it in the history and totals
    if (lastPhase && (lastPhase === 'cleaning') !== (st.phase === 'cleaning') && hasDB) {
        loadHistory();
        loadStats();
        loadParts();
    }
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

// ---- Manual driving ----

let driveDir = null;
let driveRepeat = null;
// Drive commands go one at a time, so a stop can't overtake the press before it
let driveQueue = Promise.resolve();

function sendDrive(dir) {
    const v = current();
    driveQueue = driveQueue
        .then(() => api(`/api/vacuums/${v.id}/command`, { method: 'POST', body: JSON.stringify({ action: 'drive', value: dir }) }))
        .catch((e) => setMsg($('ctlMsg'), e.message, false));
}

function driveStart(dir) {
    if (dir === driveDir) return;
    driveEnd(false);
    if (dir === 'stop') {
        sendDrive('stop');
        return;
    }
    driveDir = dir;
    document.querySelector(`#dpad [data-dir="${dir}"]`).classList.add('held');
    sendDrive(dir);
    // The server stops the vacuum if these repeats stop coming
    driveRepeat = setInterval(() => sendDrive(dir), 1000);
}

function driveEnd(sendStop = true) {
    if (!driveDir) return;
    clearInterval(driveRepeat);
    driveDir = null;
    document.querySelectorAll('#dpad .held').forEach((b) => b.classList.remove('held'));
    if (sendStop) sendDrive('stop');
}

const KEY_DIR = { ArrowUp: 'forward', ArrowDown: 'backward', ArrowLeft: 'left', ArrowRight: 'right' };

function setupDrive() {
    $('driveBtn').addEventListener('click', () => {
        const show = $('driveWrap').classList.contains('hidden');
        $('driveWrap').classList.toggle('hidden', !show);
        if (!show) driveEnd();
    });
    document.querySelectorAll('#dpad button').forEach((b) => {
        b.addEventListener('pointerdown', (ev) => {
            if (b.disabled) return;
            ev.preventDefault();
            b.setPointerCapture(ev.pointerId);
            driveStart(b.dataset.dir);
        });
        ['pointerup', 'pointercancel', 'lostpointercapture'].forEach((t) => b.addEventListener(t, () => driveEnd()));
        b.addEventListener('contextmenu', (ev) => ev.preventDefault()); // long press on phones
    });
    // Arrow keys drive too, while the pad is open and nothing else has the keys
    document.addEventListener('keydown', (ev) => {
        const dir = KEY_DIR[ev.key];
        if (!dir || ev.repeat || $('driveWrap').classList.contains('hidden') || $('driveBtn').disabled) return;
        if (['INPUT', 'SELECT', 'TEXTAREA'].includes(document.activeElement.tagName)) return;
        ev.preventDefault();
        driveStart(dir);
    });
    document.addEventListener('keyup', (ev) => { if (KEY_DIR[ev.key] === driveDir) driveEnd(); });
    window.addEventListener('blur', () => driveEnd());
    document.addEventListener('visibilitychange', () => { if (document.hidden) driveEnd(); });
}

// ---- Schedules ----

let today = '';

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
    $('sfRepeat').value = sc && sc.date ? 'once' : 'weekly';
    $('sfDate').value = (sc && sc.date) || today;
    $('sfDate').min = today;
    showRepeat();
    buildDayBoxes(sc && sc.days ? sc.days : 62);
    $('sfSave').textContent = sc ? 'Save changes' : 'Add schedule';
    $('schedForm').classList.remove('hidden');
    $('sfName').focus();
}

function closeForm() {
    editingId = null;
    $('schedForm').classList.add('hidden');
}

function showRepeat() {
    const once = $('sfRepeat').value === 'once';
    $('sfDays').classList.toggle('hidden', once);
    $('sfDateWrap').classList.toggle('hidden', !once);
    $('sfDate').required = once;
}

function formSchedule(enabled = true) {
    const once = $('sfRepeat').value === 'once';
    let days = 0;
    $('sfDays').querySelectorAll('input:checked').forEach((cb) => { days |= 1 << Number(cb.value); });
    return {
        name: $('sfName').value.trim(),
        at: $('sfAt').value,
        mode: $('sfMode').value,
        suction: $('sfSuction').value,
        days: once ? 0 : days,
        date: once ? $('sfDate').value : '',
        enabled,
    };
}

function fmtWhenSched(sc) {
    if (!sc.date) return `${fmtDays(sc.days)} at ${sc.at}`;
    return `Once, ${fmtDate(sc.date)} at ${sc.at}`;
}

// A one-off clean whose time has gone by
function schedDone(sc) {
    if (!sc.date) return false;
    return sc.date < today || (sc.date === today && Boolean(sc.last_run));
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
    today = data.today;
    $('tzNote').textContent = `Times are the Pi's local time (${data.timezone})`;
    showQuiet(data.quiet);
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
        const when = cell(fmtWhenSched(sc), 'nowrap');
        const tag = schedDone(sc) ? 'done' : sc.in_quiet ? 'skipped: quiet hours' : '';
        if (tag) {
            const t = document.createElement('div');
            t.className = 'tag';
            t.textContent = tag;
            when.appendChild(t);
        }
        tr.append(
            cell(sc.name),
            when,
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

// ---- Quiet hours ----

function showQuiet(q) {
    const on = Boolean(q && q.from);
    if (![$('qFrom'), $('qTo')].includes(document.activeElement)) {
        $('qFrom').value = on ? q.from : '';
        $('qTo').value = on ? q.to : '';
    }
    $('qOff').classList.toggle('hidden', !on);
    $('qNote').textContent = on
        ? `Schedules skip ${q.from} to ${q.to}. Starting a clean by hand still works.`
        : "Schedules don't start a clean during quiet hours.";
}

async function saveQuiet(from, to) {
    const v = current();
    try {
        await api(`/api/vacuums/${v.id}/quiet`, { method: 'PUT', body: JSON.stringify({ from, to }) });
        setMsg($('schedMsg'), from ? 'Quiet hours saved.' : 'Quiet hours off.', true);
    } catch (e) {
        setMsg($('schedMsg'), e.message, false);
    }
    loadSchedules();
}

// ---- Statistics ----

// A round top for the chart's scale, so its gridline label reads cleanly
function niceTop(minutes) {
    const steps = [30, 60, 90, 120, 180, 240, 300, 360, 480, 600, 720, 900, 1200];
    return steps.find((s) => s >= minutes) || Math.ceil(minutes / 300) * 300;
}

function drawWeeks(weeks) {
    const chart = $('weekChart');
    const axis = $('weekAxis');
    chart.replaceChildren();
    axis.replaceChildren();
    const top = niceTop(Math.max(1, ...weeks.map((w) => w.minutes)));

    const grid = document.createElement('div');
    grid.className = 'grid';
    const gl = document.createElement('span');
    gl.textContent = fmtMinutes(top);
    grid.appendChild(gl);
    const tip = document.createElement('div');
    tip.className = 'tip hidden';
    chart.append(grid, tip);

    weeks.forEach((w, i) => {
        const col = document.createElement('div');
        col.className = 'col';
        col.tabIndex = 0;
        const bar = document.createElement('div');
        bar.className = 'bar';
        bar.style.height = `${(w.minutes / top) * 100}%`;
        col.appendChild(bar);

        const last = i === weeks.length - 1;
        const what = w.cleans
            ? `${w.cleans} clean${w.cleans > 1 ? 's' : ''}, ${fmtMinutes(w.minutes)}` + (w.area ? `, ${w.area} m²` : '')
                + (w.faults ? `, ${w.faults} with faults` : '')
            : 'no cleans';
        const text = `${last ? 'This week' : 'Week of ' + fmtDate(w.start, false)}: ${what}`;
        col.setAttribute('aria-label', text);
        const show = () => {
            tip.textContent = text;
            tip.classList.remove('hidden');
            const x = col.offsetLeft + col.offsetWidth / 2;
            // Kept inside the chart at either end
            const half = tip.offsetWidth / 2;
            tip.style.left = `${Math.min(Math.max(x, half), chart.clientWidth - half)}px`;
            tip.style.top = `${col.offsetHeight - bar.offsetHeight - 6}px`;
        };
        const hide = () => tip.classList.add('hidden');
        col.addEventListener('mouseenter', show);
        col.addEventListener('focus', show);
        col.addEventListener('mouseleave', hide);
        col.addEventListener('blur', hide);
        chart.appendChild(col);

        // Every other week labelled, counting back from this one
        const lab = document.createElement('span');
        lab.textContent = (weeks.length - 1 - i) % 2 === 0 ? (last ? 'This week' : fmtDate(w.start, false)) : '';
        axis.appendChild(lab);
    });
}

async function loadStats() {
    const v = current();
    if (!v || !hasDB) return;
    let st;
    try {
        st = await api(`/api/vacuums/${v.id}/stats`);
    } catch (_) {
        return;
    }
    const m = st.last_30_days;
    const a = st.all_time;
    $('s30Cleans').textContent = m.cleans + (m.faults ? ` · ${m.faults} with faults` : '');
    $('s30Time').textContent = fmtMinutes(m.minutes);
    $('s30Area').textContent = `${m.area} m²`;
    $('sAvg').textContent = a.cleans ? fmtMinutes(Math.round(a.minutes / a.cleans)) : '—';
    $('sAll').textContent = `${a.cleans} cleans · ${fmtMinutes(a.minutes)}`;
    $('statsSince').textContent = st.since
        ? `since ${new Date(st.since).toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' })}`
        : '';
    drawWeeks(st.weeks);
}

// ---- Maintenance ----

async function resetPart(p) {
    if (!confirm(`Mark the ${p.name.toLowerCase()} as replaced today? Its hours start again from zero.`)) return;
    try {
        await api(`/api/vacuums/${current().id}/parts/${p.id}/reset`, { method: 'POST' });
        setMsg($('partMsg'), `${p.name} marked as replaced.`, true);
    } catch (e) {
        setMsg($('partMsg'), e.message, false);
    }
    loadParts();
}

async function loadParts() {
    const v = current();
    if (!v || !hasDB) return;
    let data;
    try {
        data = await api(`/api/vacuums/${v.id}/parts`);
    } catch (e) {
        setMsg($('partMsg'), e.message, false);
        return;
    }
    const rows = $('partRows');
    rows.replaceChildren();
    for (const p of data.parts) {
        const tr = document.createElement('tr');
        const pct = Math.round(p.left * 100);
        const barTd = document.createElement('td');
        const bar = document.createElement('div');
        bar.className = 'part-bar';
        const fill = document.createElement('div');
        fill.style.width = `${pct}%`;
        fill.className = p.left < 0.1 ? 'bad' : p.left < 0.25 ? 'warn' : '';
        bar.appendChild(fill);
        barTd.appendChild(bar);
        const note = p.left <= 0 ? ' · replace now' : p.left < 0.1 ? ' · replace soon' : '';

        const replaced = p.reset_at ? `${fmtWhen(p.reset_at)} by ${p.reset_by}` : 'Not recorded';
        const act = document.createElement('td');
        const btn = document.createElement('button');
        btn.className = 'btn secondary small';
        btn.textContent = 'Replaced';
        btn.addEventListener('click', () => resetPart(p));
        act.appendChild(btn);

        tr.append(
            cell(p.name),
            cell(`${Math.round(p.used_hours)} h of ${p.hours} h`, 'nowrap'),
            barTd,
            cell(`${pct}%${note}`, 'nowrap' + (p.left < 0.1 ? ' result-bad' : '')),
            cell(replaced, 'nowrap'),
            act,
        );
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
        const end = (c.end_status || '—') + (c.faults ? ` · ${c.faults.join(', ')}` : '');
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
    driveEnd();
    lastPhase = '';
    buildControls();
    refreshStatus();
    loadSchedules();
    loadHistory();
    loadStats();
    loadParts();
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
    $('statsPanel').classList.toggle('hidden', !hasDB);
    $('partsPanel').classList.toggle('hidden', !hasDB);
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
    setupDrive();

    $('newSchedBtn').addEventListener('click', () => openForm(null));
    $('sfRepeat').addEventListener('change', showRepeat);
    $('quietForm').addEventListener('submit', (ev) => {
        ev.preventDefault();
        saveQuiet($('qFrom').value, $('qTo').value);
    });
    $('qOff').addEventListener('click', () => saveQuiet('', ''));
    $('sfCancel').addEventListener('click', closeForm);
    $('schedForm').addEventListener('submit', saveSchedule);
    $('tabCleans').addEventListener('click', () => showTab('cleans'));
    $('tabCommands').addEventListener('click', () => { showTab('commands'); loadHistory(); });

    selectVacuum();
    setInterval(refreshStatus, 3000);
    setInterval(() => { loadSchedules(); loadHistory(); loadStats(); loadParts(); }, 60000);
}

init();
