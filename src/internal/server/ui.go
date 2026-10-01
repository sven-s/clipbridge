package server

const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Clipbridge</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:monospace;background:#111;color:#eee;min-height:100vh;padding:20px}
h1{color:#4af;font-size:1.2em;margin-bottom:4px}
.sub{color:#666;font-size:.85em;margin-bottom:20px}
.card{background:#1a1a1a;border:1px solid #333;border-radius:6px;padding:16px;margin-bottom:16px}
.card h2{font-size:.9em;color:#888;text-transform:uppercase;letter-spacing:.1em;margin-bottom:12px}
textarea{width:100%;background:#111;color:#eee;border:1px solid #444;border-radius:4px;padding:8px;font-family:monospace;font-size:.9em;resize:vertical;min-height:80px}
textarea:focus{outline:none;border-color:#4af}
.row{display:flex;gap:8px;margin-top:8px;flex-wrap:wrap}
button{background:#4af;color:#000;border:none;border-radius:4px;padding:7px 14px;font-family:monospace;font-size:.9em;cursor:pointer;font-weight:bold}
button:hover{background:#6cf}
button:disabled{background:#333;color:#666;cursor:default}
button.sec{background:#333;color:#eee}
button.sec:hover{background:#444}
button.danger{background:#a44;color:#fff}
.file-drop{border:2px dashed #444;border-radius:6px;padding:20px;text-align:center;color:#666;cursor:pointer;transition:border-color .2s}
.file-drop:hover,.file-drop.over{border-color:#4af;color:#4af}
.file-drop input{display:none}
.incoming{padding:12px;background:#111;border-radius:4px;border:1px solid #333}
.incoming .filename{color:#4af;font-weight:bold}
.incoming .meta{color:#666;font-size:.85em;margin:4px 0 10px}
.incoming .textval{background:#0a0a0a;border:1px solid #333;border-radius:4px;padding:8px;max-height:120px;overflow-y:auto;word-break:break-all;margin-bottom:8px;font-size:.9em}
.status{font-size:.8em;padding:4px 8px;border-radius:3px;display:inline-block}
.status.ok{color:#4a4;background:#0a1a0a}
.status.err{color:#a44;background:#1a0a0a}
.status.wait{color:#a84;background:#1a1400}
progress{width:100%;height:6px;border-radius:3px;margin-top:8px;accent-color:#4af}
#setup{max-width:420px;margin:60px auto}
#setup input{width:100%;background:#111;color:#eee;border:1px solid #444;border-radius:4px;padding:8px;font-family:monospace;font-size:.9em;margin-top:6px;margin-bottom:12px}
#setup input:focus{outline:none;border-color:#4af}
#app{max-width:540px;margin:0 auto}
.topbar{display:flex;justify-content:space-between;align-items:center;margin-bottom:20px}
.nothing{color:#555;font-style:italic;font-size:.9em}
</style>
</head>
<body>

<div id="setup" style="display:none">
  <h1>Clipbridge Setup</h1>
  <p class="sub" style="margin-bottom:20px">First-time configuration</p>
  <div class="card">
    <label>Secret key<br>
      <input type="password" id="inp-secret" placeholder="Paste secret from Mac menu bar">
    </label>
    <label>Your machine name<br>
      <input type="text" id="inp-name" placeholder="e.g. SOLAR-PC">
    </label>
    <button onclick="saveSetup()">Save &amp; Connect</button>
  </div>
</div>

<div id="app" style="display:none">
  <div class="topbar">
    <div>
      <h1>Clipbridge</h1>
      <div class="sub" id="machine-label"></div>
    </div>
    <div>
      <span id="conn-status" class="status wait">Connecting…</span>
      &nbsp;
      <button class="sec" onclick="showSettings()" style="font-size:.8em;padding:4px 8px">Settings</button>
    </div>
  </div>

  <div class="card">
    <h2>Send to Mac</h2>
    <textarea id="send-text" placeholder="Type or paste text here…"></textarea>
    <div class="row">
      <button onclick="pasteAndSend()">Paste &amp; Send</button>
      <button onclick="sendText()">Send Text</button>
    </div>
  </div>

  <div class="card">
    <h2>Send File to Mac</h2>
    <div class="file-drop" id="drop-zone" onclick="document.getElementById('file-input').click()"
         ondragover="event.preventDefault();this.classList.add('over')"
         ondragleave="this.classList.remove('over')"
         ondrop="handleDrop(event)">
      <input type="file" id="file-input" multiple onchange="uploadFiles(Array.from(this.files))">
      Click or drag files here<br><small style="color:#666">Multiple files auto-zip on the way</small>
    </div>
    <progress id="upload-progress" value="0" max="100" style="display:none"></progress>
    <div id="upload-status" style="margin-top:6px;font-size:.85em;color:#888"></div>
  </div>

  <div class="card">
    <h2>From Mac</h2>
    <div id="incoming-area">
      <div class="nothing">Nothing waiting</div>
    </div>
  </div>
</div>

<script>
let secret = '', name = '', pollTimer = null;
let currentIncomingKey = ''; // tracks what's currently rendered to avoid wiping status

function init() {
  secret = localStorage.getItem('cs_secret') || '';
  name   = localStorage.getItem('cs_name')   || '';
  const params = new URLSearchParams(location.search);
  if (params.get('secret')) {
    secret = params.get('secret');
    localStorage.setItem('cs_secret', secret);
  }
  if (!secret || !name) {
    document.getElementById('inp-secret').value = secret;
    document.getElementById('inp-name').value = name;
    document.getElementById('setup').style.display = '';
  } else {
    startApp();
  }
}

function saveSetup() {
  secret = document.getElementById('inp-secret').value.trim();
  name   = document.getElementById('inp-name').value.trim();
  if (!secret || !name) { alert('Both fields required'); return; }
  localStorage.setItem('cs_secret', secret);
  localStorage.setItem('cs_name', name);
  document.getElementById('setup').style.display = 'none';
  startApp();
}

function showSettings() {
  document.getElementById('inp-secret').value = secret;
  document.getElementById('inp-name').value = name;
  document.getElementById('app').style.display = 'none';
  document.getElementById('setup').style.display = '';
  clearInterval(pollTimer);
}

function startApp() {
  document.getElementById('setup').style.display = 'none';
  document.getElementById('app').style.display = '';
  document.getElementById('machine-label').textContent = 'Machine: ' + name;
  register();
  setInterval(register, 30000);
  pollTimer = setInterval(pollIncoming, 3000);
  pollIncoming();

  // Background tabs get their timers throttled or frozen outright, and an RDP
  // session that is minimised stops firing them completely. Re-register the
  // moment the tab (or the network) comes back so the user never has to hit
  // refresh to reappear on the Mac.
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) { register(); pollIncoming(); }
  });
  window.addEventListener('focus', register);
  window.addEventListener('online', register);
  window.addEventListener('pageshow', register);
}

function headers(extra) {
  // X-Machine-Name on every request: the server treats any authenticated call
  // as proof of life, so a throttled heartbeat alone can't drop us off the
  // Mac's menu.
  return Object.assign({'Authorization': 'Bearer ' + secret, 'X-Machine-Name': name}, extra || {});
}

async function register() {
  try {
    const r = await fetch('/register', {
      method: 'POST',
      headers: headers({'Content-Type': 'application/json'}),
      body: JSON.stringify({name})
    });
    setStatus(r.ok ? 'ok' : 'err', r.ok ? 'Connected' : 'Auth error');
  } catch(e) {
    setStatus('err', 'Disconnected');
  }
}

function setStatus(type, text) {
  const el = document.getElementById('conn-status');
  el.className = 'status ' + type;
  el.textContent = text;
}

async function pasteAndSend() {
  try {
    const text = await navigator.clipboard.readText();
    document.getElementById('send-text').value = text;
    await sendText();
  } catch(e) {
    alert('Clipboard read blocked — paste manually then click Send Text');
  }
}

async function sendText() {
  const text = document.getElementById('send-text').value;
  if (!text.trim()) { alert('Nothing to send'); return; }
  try {
    const r = await fetch('/send/from-' + name, {
      method: 'POST',
      headers: headers({'Content-Type': 'text/plain; charset=utf-8'}),
      body: text
    });
    if (r.ok) {
      document.getElementById('send-text').value = '';
      setStatus('ok', 'Text sent ✓');
      setTimeout(() => setStatus('ok', 'Connected'), 2000);
    }
  } catch(e) { setStatus('err', 'Send failed'); }
}

function handleDrop(e) {
  e.preventDefault();
  document.getElementById('drop-zone').classList.remove('over');
  const files = Array.from(e.dataTransfer.files);
  if (files.length) uploadFiles(files);
}

function fmtBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n/1024).toFixed(1) + ' KB';
  if (n < 1073741824) return (n/1048576).toFixed(1) + ' MB';
  return (n/1073741824).toFixed(2) + ' GB';
}

function fmtTime(s) {
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s/60) + 'm ' + (s%60) + 's';
  return Math.floor(s/3600) + 'h ' + Math.floor((s%3600)/60) + 'm';
}

const UPLOAD_RETRIES = 3;

function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

// One chunk, as a raw body. XHR rather than fetch because only XHR reports
// upload progress, which is the whole point behind a proxy that stalls for
// minutes at a time.
function postChunk(url, blob, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', url);
    xhr.setRequestHeader('Authorization', 'Bearer ' + secret);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.upload.onprogress = e => { if (e.lengthComputable) onProgress(e.loaded); };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) resolve();
      else reject(new Error('HTTP ' + xhr.status + (xhr.responseText ? ': ' + xhr.responseText.trim() : '')));
    };
    xhr.onerror = () => reject(new Error('connection dropped'));
    xhr.ontimeout = () => reject(new Error('timed out'));
    xhr.send(blob);
  });
}

// Retrying a chunk is safe: the server addresses writes by byte offset, so a
// resent chunk overwrites exactly the range it wrote before.
async function withRetry(fn, onRetry) {
  let lastErr;
  for (let attempt = 1; attempt <= UPLOAD_RETRIES; attempt++) {
    try { return await fn(); }
    catch (e) {
      lastErr = e;
      if (attempt < UPLOAD_RETRIES) {
        if (onRetry) onRetry(attempt, e);
        await sleep(1000 * attempt);
      }
    }
  }
  throw lastErr;
}

async function uploadFiles(files) {
  if (!files || files.length === 0) return;
  const prog = document.getElementById('upload-progress');
  const stat = document.getElementById('upload-status');
  prog.style.display = '';
  prog.value = 0;

  let totalSize = 0;
  for (const f of files) totalSize += f.size;

  const isMulti = files.length > 1;
  const label = isMulti
    ? files.length + ' files (' + fmtBytes(totalSize) + ') — server will zip them'
    : files[0].name + ' (' + fmtBytes(files[0].size) + ')';
  stat.textContent = 'Starting upload of ' + label + '…';

  const startTime = Date.now();
  const render = loaded => {
    const elapsed = (Date.now() - startTime) / 1000;
    const speed = elapsed > 0 ? loaded / elapsed : 0;
    const eta = speed > 0 ? Math.round((totalSize - loaded) / speed) : 0;
    const pct = totalSize > 0 ? Math.round(loaded * 100 / totalSize) : 100;
    prog.value = pct;
    stat.textContent =
      pct + '% — ' + fmtBytes(loaded) + ' / ' + fmtBytes(totalSize) +
      ' — ' + fmtBytes(speed) + '/s — ETA ' + fmtTime(eta);
  };

  let uploadId = null;
  try {
    let zipName = '';
    if (isMulti) {
      const ts = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
      zipName = 'clipbridge-' + files.length + '-files-' + ts + '.zip';
    }
    const initRes = await fetch('/upload/init', {
      method: 'POST',
      headers: headers({'Content-Type': 'application/json'}),
      body: JSON.stringify({
        dir: 'from-' + name,
        files: files.map(f => ({name: f.name, size: f.size})),
        zipName: zipName,
      }),
    });
    if (!initRes.ok) throw new Error('init failed (' + initRes.status + ')');
    const init = await initRes.json();
    uploadId = init.uploadId;
    const chunkSize = init.chunkSize;

    // Committed bytes. In-flight progress is added on top so the bar keeps
    // moving inside a single chunk without double-counting once it lands.
    let sent = 0;
    for (let i = 0; i < files.length; i++) {
      const f = files[i];
      const offsets = [];
      for (let off = 0; off < f.size; off += chunkSize) offsets.push(off);
      // A zero-byte file still needs its part created, or completion sees a gap.
      if (offsets.length === 0) offsets.push(0);

      for (const off of offsets) {
        const blob = f.slice(off, Math.min(off + chunkSize, f.size));
        const url = '/upload/chunk/' + uploadId + '/' + i + '?offset=' + off;
        await withRetry(
          () => postChunk(url, blob, loaded => render(sent + loaded)),
          (attempt, err) => {
            stat.textContent = 'Chunk failed (' + err.message + ') — retry ' +
              (attempt + 1) + '/' + UPLOAD_RETRIES + '…';
          }
        );
        sent += blob.size;
        render(sent);
      }
    }

    const done = await fetch('/upload/complete/' + uploadId, {method: 'POST', headers: headers()});
    if (!done.ok) throw new Error((await done.text()).trim() || ('complete failed (' + done.status + ')'));
    uploadId = null;

    prog.style.display = 'none';
    const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
    stat.textContent = (isMulti ? files.length + ' files' : files[0].name) +
      ' sent in ' + elapsed + 's ✓';
    setStatus('ok', 'File sent ✓');
  } catch (e) {
    prog.style.display = 'none';
    stat.textContent = 'Upload failed — ' + e.message;
    setStatus('err', 'Upload failed');
    if (uploadId) {
      // Best effort: drop the staged bytes rather than leave them for the sweep.
      fetch('/upload/abort/' + uploadId, {method: 'DELETE', headers: headers()}).catch(() => {});
    }
  }
}

async function pollIncoming() {
  const dir = 'to-' + name;
  try {
    const r = await fetch('/poll/' + dir, {headers: headers()});
    if (r.status === 404) { showNothing(); return; }
    if (!r.ok) return;
    const meta = await r.json();
    showIncoming(meta, dir);
  } catch(e) {}
}

function showNothing() {
  if (currentIncomingKey === 'none') return;
  currentIncomingKey = 'none';
  document.getElementById('incoming-area').innerHTML =
    '<div class="nothing">Nothing waiting</div>';
}

function showIncoming(meta, dir) {
  const key = meta.type + '|' + (meta.filename || '') + '|' + meta.size;
  if (key === currentIncomingKey) return; // already rendered, don't wipe status
  currentIncomingKey = key;
  const area = document.getElementById('incoming-area');
  if (meta.type === 'text') {
    area.innerHTML = '<div class="incoming">' +
      '<div class="meta">Text · ' + meta.size + ' bytes</div>' +
      '<div class="textval" id="text-preview">Loading…</div>' +
      '<div class="row">' +
      '<button onclick="copyIncoming()">Copy to Clipboard</button>' +
      '<button class="sec danger" onclick="clearIncoming()">Dismiss</button>' +
      '</div></div>';
    fetch('/receive/' + dir, {headers: headers()}).then(r => r.text()).then(t => {
      document.getElementById('text-preview').textContent = t;
      window._pendingText = t;
    });
  } else {
    const mb = (meta.size / 1048576).toFixed(1);
    area.innerHTML = '<div class="incoming">' +
      '<div class="filename">📄 ' + meta.filename + '</div>' +
      '<div class="meta">' + mb + ' MB</div>' +
      '<div class="row">' +
      '<button id="dl-btn" onclick="downloadIncoming(\'' + dir + '\',\'' + meta.filename + '\')">Download</button>' +
      '<button class="sec danger" onclick="clearIncoming()">Dismiss</button>' +
      '</div>' +
      '<div id="dl-status" style="margin-top:14px;font-size:1em;color:#4af;line-height:1.5"></div>' +
      '</div>';
  }
}

async function copyIncoming() {
  const dir = 'to-' + name;
  try {
    const text = window._pendingText ||
      await fetch('/receive/' + dir, {headers: headers()}).then(r => r.text());
    await navigator.clipboard.writeText(text);
    await fetch('/clear/' + dir, {method: 'DELETE', headers: headers()});
    showNothing();
    setStatus('ok', 'Copied ✓');
    setTimeout(() => setStatus('ok', 'Connected'), 2000);
  } catch(e) {
    alert('Clipboard write blocked — copy the text manually from the box above');
  }
}

function downloadIncoming(dir, filename) {
  const stat = document.getElementById('dl-status');
  stat.innerHTML =
    '⬇ <b>Download started</b> — check your browser\'s Downloads folder.<br>' +
    '<br>' +
    '⚠ Corporate proxies (Zscaler etc.) often scan the entire file first<br>' +
    'and deliver it in one burst at the end. The browser will show<br>' +
    '<b>0 B/s</b> for several minutes — that\'s normal, just wait.<br>' +
    '<br>' +
    'The file stays on the server until you click <b>Dismiss</b>,<br>' +
    'so you can safely retry if it actually fails.';

  const a = document.createElement('a');
  a.href = '/receive/' + dir + '?auth=' + encodeURIComponent(secret) + '&machine=' + encodeURIComponent(name);
  a.download = filename;
  a.click();
}

async function clearIncoming() {
  const dir = 'to-' + name;
  await fetch('/clear/' + dir, {method: 'DELETE', headers: headers()});
  showNothing();
}

init();
</script>
</body>
</html>`
