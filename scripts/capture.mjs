// Capture the real browser UI with sample API responses. Chromium's debugging
// pipe needs no open port, and the sample page never reaches the network.
import assert from 'node:assert/strict';
import {spawn, execFileSync} from 'node:child_process';
import {copyFile, mkdtemp, readFile, rm, writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';

const [version, option] = process.argv.slice(2);
const docs = new URL('../docs/', import.meta.url);
const template = await readFile(new URL('../internal/server/index.html', import.meta.url), 'utf8');
const origin = 'http://loka.test:45873';
const name = 'Studio Desktop';
const laptop = {id: 'laptop', name: 'Studio Laptop', host: '192.0.2.21', port: 45873};
const pc = {id: 'pc', name: 'Living Room PC', host: '192.0.2.22', port: 45873};
const state = {name, peers: [laptop, pc], addresses: ['192.0.2.20'], transfers: []};
const desktop = {width: 1600, height: 840, deviceScaleFactor: 0.75, mobile: false};

const sampleFiles = ['itinerary.pdf', 'weekend-photos.zip'];
const samplePaths = sampleFiles.map(file => `/home/alex/Documents/${file}`);
const fileTransfer = {id: 'demo', name: laptop.name, direction: 'outgoing', items: sampleFiles};
const previewTransfers = [
  {id: 'text', kind: 'text', direction: 'outgoing', status: 'complete', progress: 100,
    name: pc.name, preview: 'Meet at the station at 10.', size: 26},
  {id: 'notes', direction: 'outgoing', status: 'complete', progress: 100,
    name: laptop.name, items: ['meeting-notes.pdf'], speed_mbps: 312.4},
  {id: 'video', direction: 'incoming', status: 'receiving', progress: 62,
    name: pc.name, items: ['weekend-video.mp4'], speed_mbps: 87.4},
  {id: 'link', direction: 'outgoing', status: 'shared', items: ['itinerary.pdf'], token: 'example-download'},
];

function response({url, method, postData}) {
  const {pathname} = new URL(url);
  if (pathname === '/') {
    const values = {
      MODE: 'desktop', NAME: JSON.stringify(name), VERSION: version,
      KEY: 'example', RECEIVE_TOKEN: 'example-receive', CA_FINGERPRINT: '00'.repeat(32), VOUCHES: true,
    };
    const html = template.replace(/__LOKA_([A-Z_]+)__/g, (_, key) => {
      assert.ok(Object.hasOwn(values, key), `Missing page value: ${key}`);
      return values[key];
    });
    return [200, 'text/html; charset=utf-8', html];
  }
  if (pathname === '/api/state') return [200, 'application/json', JSON.stringify(state)];
  if (pathname === '/api/send' && method === 'POST') {
    const {id, paths} = JSON.parse(postData);
    assert.equal(id, laptop.id);
    assert.deepEqual(paths, samplePaths);
    state.transfers = [{...fileTransfer, status: 'waiting', progress: 0, code: '472 918', new_device: true}];
    return [200, 'application/json', JSON.stringify({id: fileTransfer.id})];
  }
  return [404, 'text/plain', ''];
}

function launchChromium(directory, env) {
  const process = spawn('chromium', [
    '--headless', '--remote-debugging-pipe', '--disable-background-networking',
    '--disable-component-update', '--no-first-run', '--no-default-browser-check',
    `--user-data-dir=${join(directory, 'profile')}`, 'about:blank',
  ], {cwd: directory, env, stdio: ['ignore', 'ignore', 'pipe', 'pipe', 'pipe']});
  const [input, output] = process.stdio.slice(3);
  const commands = new Map();
  const events = new Map();
  let nextID = 0;
  let sessionId;
  let buffer = '';
  let diagnostics = '';

  const closed = new Promise(resolve => process.once('close', () => {
    clearTimeout(timeout);
    fail(new Error(`Chromium exited. ${diagnostics.trim()}`));
    resolve();
  }));
  const timeout = setTimeout(() => {
    fail(new Error(`Chromium capture timed out. ${diagnostics.trim()}`));
    process.kill();
  }, 60_000);
  function fail(error) {
    for (const {reject} of [...commands.values(), ...events.values()]) reject(error);
    commands.clear();
    events.clear();
  }
  process.on('error', fail);
  // A crashed browser can reset the pipe before it prints the reason.
  for (const stream of [input, output]) stream.on('error', () => {});
  process.stderr.setEncoding('utf8').on('data', chunk => {
    diagnostics = (diagnostics + chunk).slice(-3000);
  });

  function send(method, params = {}) {
    const id = ++nextID;
    return new Promise((resolve, reject) => {
      commands.set(id, {resolve, reject});
      input.write(JSON.stringify({id, method, params, sessionId}) + '\0');
    });
  }

  output.setEncoding('utf8').on('data', chunk => {
    const messages = (buffer + chunk).split('\0');
    buffer = messages.pop();
    for (const message of messages) {
      const {id, method, params, result, error} = JSON.parse(message);
      if (id) {
        const {resolve, reject} = commands.get(id) ?? {};
        commands.delete(id);
        if (error) reject?.(new Error(error.message));
        else resolve?.(result);
      } else if (method === 'Fetch.requestPaused') {
        Promise.resolve().then(() => {
          const [responseCode, type, body] = response(params.request);
          return send('Fetch.fulfillRequest', {
            requestId: params.requestId, responseCode, body: Buffer.from(body).toString('base64'),
            responseHeaders: [{name: 'Content-Type', value: type}, {name: 'Cache-Control', value: 'no-store'}],
          });
        }).catch(error => {
          fail(error);
          process.kill();
        });
      } else if (events.has(method)) {
        const {resolve} = events.get(method);
        events.delete(method);
        resolve(params);
      }
    }
  });

  return {
    send,
    event: method => new Promise((resolve, reject) => events.set(method, {resolve, reject})),
    async attach() {
      const {targetInfos} = await send('Target.getTargets');
      const {targetId} = targetInfos.find(target => target.type === 'page');
      ({sessionId} = await send('Target.attachToTarget', {targetId, flatten: true}));
    },
    async close() {
      process.kill();
      await closed;
    },
  };
}

async function capture(directory, env) {
  const browser = launchChromium(directory, env);
  const {send} = browser;
  const files = [];
  let navigation = 0;

  async function evaluate(expression) {
    const {result, exceptionDetails} = await send('Runtime.evaluate', {
      expression, awaitPromise: true, returnByValue: true,
    });
    if (exceptionDetails) throw new Error(result.description || exceptionDetails.text);
    return result.value;
  }

  async function open(theme, lang = 'en') {
    await send('Emulation.setDeviceMetricsOverride', desktop);
    const loaded = browser.event('Page.loadEventFired');
    const url = `${origin}/?theme=${theme}&lang=${lang}&view=${++navigation}`;
    await Promise.all([loaded, send('Page.navigate', {url})]);
    await evaluate('refresh()');
    assert.equal(await evaluate("document.querySelectorAll('#peers button').length"), 2);
    assert.deepEqual(await evaluate('[document.documentElement.lang, theme()]'), [lang, theme]);
  }

  async function screenshot(file) {
    const {data} = await send('Page.captureScreenshot', {format: 'png'});
    await writeFile(join(directory, file), Buffer.from(data, 'base64'));
  }

  async function fitPreview() {
    const height = await evaluate('document.documentElement.scrollHeight');
    if (height <= desktop.height) return;
    // Grow the viewport in 40 × 21 steps and keep the output at 1200 × 630.
    const steps = Math.ceil((height + 16) / 21);
    await send('Emulation.setDeviceMetricsOverride', {
      width: steps * 40, height: steps * 21, deviceScaleFactor: 30 / steps, mobile: false,
    });
    assert.ok(await evaluate('document.documentElement.scrollHeight <= innerHeight'), 'Desktop preview is clipped');
  }

  async function addPaths() {
    await evaluate(`for (const path of ${JSON.stringify(samplePaths)}) {
      $('paths').value = path;
      $('add-path').click();
    }`);
  }

  async function gif(theme, suffix) {
    state.transfers = [];
    await open(theme);
    const frames = [];
    async function frame(seconds) {
      const file = `frame-${frames.length}.png`;
      await screenshot(file);
      frames.push({file, seconds});
    }

    await frame(0.8);
    await addPaths();
    await frame(1.24);
    await evaluate(`(() => {
      const button = document.querySelector('#peers button');
      const {host, port, id} = button.dataset;
      return send(host, port, button, id);
    })()`);
    assert.match(await evaluate("document.querySelector('.transfer-code:not([hidden])')?.textContent"), /472 918/);
    await frame(2.4);
    for (const progress of [12, 35, 62, 88]) {
      state.transfers = [{...fileTransfer, status: 'sending', progress, speed_mbps: 312.4}];
      await evaluate('refresh()');
      await frame(0.36);
    }
    state.transfers = [{...fileTransfer, status: 'complete', progress: 100, speed_mbps: 312.4}];
    await evaluate('refresh()');
    assert.equal(await evaluate("$('selected').childElementCount"), 0);
    await frame(2.4);

    const playlist = frames.map(({file, seconds}) => `file '${file}'\nduration ${seconds}`);
    // Repeat the last frame so ffmpeg gives it the requested duration.
    playlist.push(`file '${frames.at(-1).file}'`);
    await writeFile(join(directory, 'frames.txt'), playlist.join('\n') + '\n');
    const file = `demo${suffix}.gif`;
    execFileSync('ffmpeg', [
      '-loglevel', 'error', '-f', 'concat', '-safe', '0', '-i', 'frames.txt',
      '-filter_complex_threads', '1', '-filter_complex',
      'split[a][b];[a]palettegen[p];[b][p]paletteuse=dither=none:diff_mode=rectangle',
      '-fps_mode', 'vfr', '-loop', '0', file,
    ], {cwd: directory, env, stdio: 'inherit'});
    files.push(file);
  }

  try {
    await browser.attach();
    await send('Page.enable');
    await send('Fetch.enable', {patterns: [{urlPattern: `${origin}/*`}]});
    await send('Emulation.setEmulatedMedia', {features: [{name: 'prefers-reduced-motion', value: 'reduce'}]});
    await send('Page.addScriptToEvaluateOnNewDocument', {source: `
      if (location.origin === ${JSON.stringify(origin)}) {
        const params = new URLSearchParams(location.search);
        localStorage.setItem('loka-theme', params.get('theme'));
        localStorage.setItem('loka-language', params.get('lang'));
      }
    `});

    for (const theme of ['dark', 'light']) {
      const suffix = theme === 'dark' ? '' : '-light';
      if (option === '--gif') await gif(theme, suffix);
      state.transfers = previewTransfers;
      for (const lang of ['en', 'uk']) {
        await open(theme, lang);
        await addPaths();
        await fitPreview();
        const file = `loka${lang === 'uk' ? '-uk' : ''}${suffix}.png`;
        await screenshot(file);
        files.push(file);
      }
    }
    return files;
  } finally {
    await browser.close();
  }
}

const directory = await mkdtemp(join(tmpdir(), 'loka-capture-'));
const env = {
  PATH: process.env.PATH, LANG: 'C.UTF-8',
  XDG_CONFIG_HOME: directory, XDG_CACHE_HOME: directory, XDG_DATA_HOME: directory,
};
try {
  for (const file of await capture(directory, env)) {
    await copyFile(join(directory, file), new URL(file, docs));
    console.log(`docs/${file}`);
  }
} finally {
  await rm(directory, {recursive: true, force: true});
}
