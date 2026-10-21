import { createServer } from 'node:http';
import { spawn, execFile } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { randomUUID } from 'node:crypto';
import os from 'node:os';

const root = dirname(fileURLToPath(import.meta.url));
const catalog = JSON.parse(await readFile(join(root, 'catalog.json'), 'utf8'));
const appsById = new Map(catalog.apps.map((app) => [app.id, app]));
const DRY = process.argv.includes('--dry');
const PORT = Number(process.env.PORT || 8765);
const TAIL_LIMIT = 9000;

const wingetCandidates = [
  process.env.OFFICEHUB_WINGET,
  join(os.homedir(), 'AppData', 'Local', 'Microsoft', 'WindowsApps', 'winget.exe'),
  'winget',
].filter(Boolean);
const WINGET = wingetCandidates.find((c) => c === 'winget' || existsSync(c)) || 'winget';

function runCaptured(args) {
  return new Promise((resolve, reject) => {
    execFile(WINGET, args, { windowsHide: true, maxBuffer: 64 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err && !stdout) reject(new Error(String(stderr || err.message).slice(0, 400)));
      else resolve(`${stdout}\n${stderr}`);
    });
  });
}

let scan = { at: 0, installed: {}, error: null, running: null };

async function rescan() {
  if (scan.running) return scan.running;
  scan.running = (async () => {
    try {
      const out = DRY
        ? ''
        : await runCaptured(['list', '--accept-source-agreements', '--disable-interactivity']);
      const installed = {};
      for (const app of catalog.apps) {
        const re = new RegExp(`(?:^|\\s)${app.id.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(?:\\s|$)`);
        for (const line of out.split(/\r?\n/)) {
          if (re.test(line)) {
            const cells = line.trim().split(/\s{2,}/);
            installed[app.id] = cells[2] || '';
            break;
          }
        }
      }
      scan = { at: Date.now(), installed, error: null, running: null };
    } catch (error) {
      scan = { at: Date.now(), installed: scan.installed, error: error.message, running: null };
    }
    return scan;
  })();
  return scan.running;
}

const jobs = new Map();

function installArgs(appId, { mode, silent }) {
  const verb = mode === 'upgrade' ? 'upgrade' : 'install';
  const args = [
    verb,
    '-e',
    '--id',
    appId,
    '--source',
    'winget',
    '--accept-source-agreements',
    '--accept-package-agreements',
    '--disable-interactivity',
  ];
  if (silent) args.push('--silent');
  return args;
}

function startJob(appId, mode, silent) {
  const jobId = randomUUID();
  const args = installArgs(appId, { mode, silent });
  const job = {
    id: jobId,
    appId,
    appName: appsById.get(appId).name,
    mode,
    command: `"${WINGET}" ${args.join(' ')}`,
    status: 'running',
    exitCode: null,
    tail: '',
    startedAt: Date.now(),
    endedAt: null,
    child: null,
  };
  jobs.set(jobId, job);
  for (const previous of jobs.values()) {
    if (jobs.size > 40 && previous.status !== 'running' && previous.id !== jobId) jobs.delete(previous.id);
  }

  if (DRY) {
    job.status = 'dry-run';
    job.endedAt = Date.now();
    job.tail = `Режим --dry: команда подготовлена, но не запускалась.\n${job.command}`;
    return job;
  }

  const child = spawn(WINGET, args, { windowsHide: true, env: process.env });
  job.child = child;
  const push = (chunk) => {
    job.tail = (job.tail + chunk.toString('utf8')).slice(-TAIL_LIMIT);
  };
  child.stdout.on('data', push);
  child.stderr.on('data', push);
  child.on('error', (error) => {
    job.status = 'failed';
    job.exitCode = -1;
    job.endedAt = Date.now();
    push(`\nНе удалось запустить winget: ${error.message}`);
  });
  child.on('close', (code) => {
    job.child = null;
    job.exitCode = code;
    job.endedAt = Date.now();
    if (job.status === 'canceled') return;
    job.status = code === 0 ? 'done' : 'failed';
    if (code === 0) rescan().catch(() => {});
  });
  return job;
}

function publicJob(job) {
  const { child, ...rest } = job;
  return rest;
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    let raw = '';
    req.on('data', (chunk) => {
      raw += chunk;
      if (raw.length > 64 * 1024) reject(new Error('body too large'));
    });
    req.on('end', () => {
      if (!raw) return resolve({});
      try {
        resolve(JSON.parse(raw));
      } catch {
        reject(new Error('invalid json'));
      }
    });
  });
}

const json = (res, code, data) => {
  res.writeHead(code, { 'content-type': 'application/json; charset=utf-8' });
  res.end(JSON.stringify(data));
};

const server = createServer(async (req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${PORT}`);
  try {
    if (url.pathname === '/' || url.pathname === '/index.html') {
      res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
      res.end(await readFile(join(root, 'public', 'index.html')));
      return;
    }

    if (url.pathname === '/api/catalog') {
      json(res, 200, {
        categories: catalog.categories,
        apps: catalog.apps,
        installed: scan.installed,
        scannedAt: scan.at,
        scanError: scan.error,
        winget: WINGET,
        dry: DRY,
      });
      return;
    }

    if (url.pathname === '/api/jobs') {
      json(res, 200, [...jobs.values()].map(publicJob).sort((a, b) => b.startedAt - a.startedAt));
      return;
    }

    if (req.method === 'POST' && url.pathname === '/api/scan') {
      await rescan();
      json(res, 200, { installed: scan.installed, scannedAt: scan.at, error: scan.error });
      return;
    }

    if (req.method === 'POST' && (url.pathname === '/api/install' || url.pathname === '/api/upgrade')) {
      const body = await readBody(req);
      const app = appsById.get(body.id);
      if (!app) {
        json(res, 400, { error: 'id отсутствует в каталоге — установка запрещена' });
        return;
      }
      const job = startJob(app.id, url.pathname === '/api/upgrade' ? 'upgrade' : 'install', !!body.silent);
      json(res, 202, publicJob(job));
      return;
    }

    if (req.method === 'POST' && url.pathname === '/api/cancel') {
      const body = await readBody(req);
      const job = jobs.get(body.jobId);
      if (!job || !job.child) {
        json(res, 400, { error: 'задача не найдена или уже завершена' });
        return;
      }
      job.status = 'canceled';
      job.child.kill();
      json(res, 200, publicJob(job));
      return;
    }

    json(res, 404, { error: 'not found' });
  } catch (error) {
    json(res, 500, { error: error.message });
  }
});

server.listen(PORT, '127.0.0.1', () => {
  const link = `http://127.0.0.1:${PORT}`;
  console.log(`Office Hub: ${link}${DRY ? '  (dry-run: winget не запускается)' : ''}`);
  console.log(`winget: ${WINGET}`);
  rescan().catch(() => {});
  if (!process.env.OFFICEHUB_NO_OPEN) {
    execFile('cmd.exe', ['/c', 'start', 'OfficeHub', link], () => {});
  }
});
