#!/usr/bin/env node

const http = require('http');
const { execSync, spawn } = require('child_process');
const { createRequire } = require('module');

const BUFFER_SIZE = 100;
const args = process.argv.slice(2);

// parse flags
const isPublic = args.includes('--public');
const portIdx = args.indexOf('--port');
const PORT = portIdx !== -1 ? parseInt(args[portIdx + 1], 10) : (process.env.PORT || 9999);

// remove flags to get the command
const cmdArgs = args.filter((a, i) => {
  if (a === '--public') return false;
  if (a === '--port') return false;
  if (portIdx !== -1 && i === portIdx + 1) return false;
  return true;
});

// detect mode: pipe (stdin) or command
const isPipe = cmdArgs.length === 0;

if (!isPipe && cmdArgs.length === 0) {
  process.stderr.write('Usage:\n');
  process.stderr.write('  piper <command>              # run command and stream\n');
  process.stderr.write('  piper --public <command>     # stream publicly via Tailscale Funnel\n');
  process.stderr.write('  command | piper              # pipe mode (may buffer)\n');
  process.exit(1);
}

const clients = new Set();
const buffer = [];
let funnelProcess = null;
let childProcess = null;

function getTailscaleIp() {
  try {
    return execSync('tailscale ip -4', { encoding: 'utf8' }).trim();
  } catch {
    return null;
  }
}

function getFunnelUrl() {
  try {
    const status = execSync('tailscale status --json', { encoding: 'utf8' });
    const { Self } = JSON.parse(status);
    const hostname = Self.DNSName.replace(/\.$/, '');
    return `https://${hostname}`;
  } catch {
    return null;
  }
}

function startFunnel() {
  const url = getFunnelUrl();
  if (!url) {
    process.stderr.write('  Error: Could not get Tailscale hostname\n');
    return null;
  }

  funnelProcess = spawn('tailscale', ['funnel', String(PORT)], {
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  funnelProcess.on('error', (err) => {
    process.stderr.write(`  Funnel error: ${err.message}\n`);
  });

  return url;
}

function broadcast(chunk) {
  const lines = chunk.split('\n');
  for (const line of lines) {
    if (line || lines.length === 1) {
      buffer.push(line + '\n');
      if (buffer.length > BUFFER_SIZE) buffer.shift();
    }
  }

  for (const client of clients) {
    client.write(chunk);
  }

  process.stdout.write(chunk);
}

function cleanup() {
  if (funnelProcess) {
    funnelProcess.kill();
    try { execSync(`tailscale funnel --https=${PORT} off 2>/dev/null`); } catch {}
  }
  if (childProcess) {
    childProcess.kill();
  }
}

function shutdown() {
  for (const client of clients) {
    client.end();
  }
  cleanup();
  server.close();
  process.exit(0);
}

process.on('SIGINT', () => { cleanup(); process.exit(0); });
process.on('SIGTERM', () => { cleanup(); process.exit(0); });

const server = http.createServer((req, res) => {
  if (req.url === '/health') {
    res.writeHead(200);
    return res.end('ok');
  }

  res.writeHead(200, {
    'Content-Type': 'text/plain; charset=utf-8',
    'Cache-Control': 'no-cache',
    'Transfer-Encoding': 'chunked',
  });

  if (buffer.length > 0) {
    res.write(buffer.join(''));
  }

  clients.add(res);
  req.on('close', () => clients.delete(res));
});

function startSource() {
  if (isPipe) {
    // pipe mode: read from stdin
    process.stdin.setEncoding('utf8');
    process.stdin.on('data', broadcast);
    process.stdin.on('end', shutdown);
  } else {
    // command mode: run with script(1) to force PTY (no buffering)
    const cmd = cmdArgs.join(' ');

    // macOS: script -q /dev/null command
    // Linux: script -qfc command /dev/null
    const isMac = process.platform === 'darwin';
    const scriptArgs = isMac
      ? ['-q', '/dev/null', '/bin/zsh', '-c', cmd]
      : ['-qfc', cmd, '/dev/null'];

    childProcess = spawn('script', scriptArgs, {
      stdio: ['ignore', 'pipe', 'pipe'],
      env: { ...process.env, TERM: 'xterm-256color' },
    });

    childProcess.stdout.setEncoding('utf8');
    childProcess.stdout.on('data', broadcast);

    childProcess.stderr.setEncoding('utf8');
    childProcess.stderr.on('data', broadcast);

    childProcess.on('close', (code) => {
      process.stderr.write(`\n  Command exited (code ${code})\n`);
      shutdown();
    });
  }
}

server.listen(PORT, '0.0.0.0', () => {
  const tsIp = getTailscaleIp();
  const localUrl = `http://localhost:${PORT}`;
  const tsUrl = tsIp ? `http://${tsIp}:${PORT}` : null;

  process.stderr.write('\n');
  process.stderr.write(`  Stream ready!\n`);
  process.stderr.write(`  Local:     ${localUrl}\n`);
  if (tsUrl) {
    process.stderr.write(`  Tailscale: ${tsUrl}\n`);
  }

  if (isPublic) {
    const publicUrl = startFunnel();
    if (publicUrl) {
      process.stderr.write(`  Public:    ${publicUrl}\n`);
      process.stderr.write(`\n  Viewers:   curl -N ${publicUrl}\n`);
    }
  } else {
    process.stderr.write(`\n  Viewers:   curl -N ${tsUrl || localUrl}\n`);
  }

  process.stderr.write('\n');

  startSource();
});
