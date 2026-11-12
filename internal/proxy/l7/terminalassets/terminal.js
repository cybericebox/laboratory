// The console page. It speaks the frame protocol of internal/deviceaccess/wire over a WebSocket to its own host. It sets no title, writes
// nothing to the clipboard (the OSC 52 addon is not loaded), opens only http(s) links and only on a click, and acknowledges the output
// it has drawn so the proxy can hold the shell back when this page falls behind.
(() => {
  'use strict';
  const app = document.getElementById('app');
  const T = JSON.parse(document.getElementById('i18n').textContent);
  const TYPE = { DATA: 1, RESIZE: 2, CLOSE: 3, ACK: 4 };
  const MAX_FRAME = 32 * 1024;
  const $ = (id) => document.getElementById(id);
  const statusEl = $('status');

  // One rule for every link, the ones found in the text and the ones a program declares with OSC 8: only http(s), only on a click, in a
  // new context without the opener or the referrer.
  function openLink(event, uri) {
    if (/^https?:\/\//i.test(uri)) {
      window.open(uri, '_blank', 'noopener,noreferrer');
    }
  }

  const term = new Terminal({
    linkHandler: { activate: openLink },
    cursorBlink: true,
    scrollback: 5000,
    fontSize: 14,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    theme: { background: '#0b0f14' },
    allowProposedApi: false,
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.loadAddon(new WebLinksAddon.WebLinksAddon(openLink));
  term.open($('term'));
  fit.fit();

  let ws = null;
  let ready = false;
  let ended = false;
  let reason = '';
  let owed = 0;
  let ackTimer = 0;
  const enc = new TextEncoder();
  const dec = new TextDecoder();

  function setStatus(text, cls) {
    statusEl.textContent = text;
    statusEl.className = cls || '';
  }

  function frame(type, payload) {
    const out = new Uint8Array(payload.length + 1);
    out[0] = type;
    out.set(payload, 1);
    return out;
  }

  function send(type, payload) {
    if (ws && ws.readyState === WebSocket.OPEN && ready) {
      ws.send(frame(type, payload));
    }
  }

  function sendInput(bytes) {
    for (let i = 0; i < bytes.length; i += MAX_FRAME) {
      send(TYPE.DATA, bytes.subarray(i, Math.min(i + MAX_FRAME, bytes.length)));
    }
  }

  function sendSize() {
    send(TYPE.RESIZE, new Uint8Array([term.cols >> 8, term.cols & 255, term.rows >> 8, term.rows & 255]));
  }

  function flushAck() {
    clearTimeout(ackTimer);
    ackTimer = 0;
    if (owed > 0) {
      const n = owed;
      owed = 0;
      send(TYPE.ACK, new Uint8Array([n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255]));
    }
  }

  function drawn(n) {
    owed += n;
    if (owed >= 16384) {
      flushAck();
    } else if (!ackTimer) {
      ackTimer = setTimeout(flushAck, 30);
    }
  }

  function finish(code) {
    if (ended) {
      return;
    }
    ended = true;
    ready = false;
    setStatus(T.ended, 'bad');
    $('over-text').textContent = T.reasons[code] || T.reasons.error;
    $('over').classList.add('show');
  }

  function connect() {
    const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
    setStatus(T.connecting, '');
    ws = new WebSocket(scheme + '//' + location.host + '/ws');
    ws.binaryType = 'arraybuffer';
    ws.onopen = () => ws.send(JSON.stringify({ cols: term.cols, rows: term.rows }));
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        let m;
        try { m = JSON.parse(ev.data); } catch (e) { return; }
        if (m.type === 'ready') {
          ready = true;
          setStatus(T.connected, 'ok');
          term.focus();
        } else if (m.type === 'error') {
          finish(m.code);
        }
        return;
      }
      const b = new Uint8Array(ev.data);
      if (b.length === 0) {
        return;
      }
      if (b[0] === TYPE.DATA) {
        const data = b.subarray(1);
        term.write(data, () => drawn(data.length));
      } else if (b[0] === TYPE.CLOSE) {
        reason = dec.decode(b.subarray(1));
      }
    };
    ws.onclose = () => finish(reason || 'error');
    ws.onerror = () => {};
  }

  term.onData((d) => sendInput(enc.encode(d)));
  term.onBinary((d) => sendInput(Uint8Array.from(d, (c) => c.charCodeAt(0) & 255)));
  term.onResize(sendSize);
  let resizeTimer = 0;
  window.addEventListener('resize', () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => fit.fit(), 100);
  });
  $('again').addEventListener('click', () => location.reload());

  connect();
})();
