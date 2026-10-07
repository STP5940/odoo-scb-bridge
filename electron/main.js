const { app, BrowserWindow, ipcMain, safeStorage, shell } = require('electron');
const { execFile } = require('child_process');
const os = require('os');
const { promisify } = require('util');
const path = require('path');
const fs = require('fs');
const crypto = require('crypto');
const http = require('http');

const execFileAsync = promisify(execFile);
const serviceName = 'OdooSCBBridge';
let mainWindow;
let pinFailures = 0;
let pinLockedUntil = 0;
let pinAuditEvents = [];
let pinSecurityStateLoaded = false;
let pinAuditFlushPromise = null;

function pinRecordPath() {
  return path.join(app.getPath('userData'), 'app-pin.dat');
}

function pinSetupSkippedPath() {
  return path.join(app.getPath('userData'), 'pin-setup-skipped');
}

function pinSecurityStatePath() {
  return path.join(app.getPath('userData'), 'pin-security.dat');
}

function loadPinSecurityState() {
  if (pinSecurityStateLoaded) return;
  const file = pinSecurityStatePath();
  pinFailures = 0;
  pinLockedUntil = 0;
  pinAuditEvents = [];
  if (fs.existsSync(file)) {
    try {
      if (safeStorage.isEncryptionAvailable()) {
        const encrypted = fs.readFileSync(file, 'utf8').trim();
        if (encrypted) {
          const decrypted = safeStorage.decryptString(Buffer.from(encrypted, 'base64'));
          if (decrypted) {
            const state = JSON.parse(decrypted);
            if (Number.isInteger(state.failures) && state.failures >= 0 && state.failures <= 4) {
              pinFailures = state.failures;
            }
            if (Number.isFinite(state.lockedUntil)) {
              pinLockedUntil = state.lockedUntil;
            }
            if (Array.isArray(state.pendingLockoutAudits)) {
              pinAuditEvents = state.pendingLockoutAudits.filter(event => event && typeof event.timestamp === 'string');
            }
          }
        }
      }
    } catch (e) {
      console.error('Error loading PIN security state, resetting:', e);
      try { fs.rmSync(file, { force: true }); } catch (_) {}
    }
  }
  pinSecurityStateLoaded = true;
}

function savePinSecurityState() {
  if (!safeStorage.isEncryptionAvailable()) throw new Error('Secure PIN storage is unavailable');
  const file = pinSecurityStatePath();
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const temporaryFile = file + '.tmp';
  const state = { failures: pinFailures, lockedUntil: pinLockedUntil, pendingLockoutAudits: pinAuditEvents };
  fs.writeFileSync(temporaryFile, safeStorage.encryptString(JSON.stringify(state)).toString('base64'), { mode: 0o600 });
  fs.renameSync(temporaryFile, file);
}

function sendPinLockoutAudit(event) {
  return new Promise((resolve, reject) => {
    const body = Buffer.from(JSON.stringify(event));
    const request = http.request({
      hostname: '127.0.0.1', port: 9527, path: '/api/security/pin-lockout', method: 'POST',
      timeout: 3000, headers: { 'Content-Type': 'application/json', 'Content-Length': body.length }
    }, response => {
      response.resume();
      response.on('end', () => response.statusCode >= 200 && response.statusCode < 300
        ? resolve() : reject(new Error(`PIN audit API returned ${response.statusCode}`)));
    });
    request.on('timeout', () => request.destroy(new Error('PIN audit request timed out')));
    request.on('error', reject);
    request.end(body);
  });
}

function flushPinLockoutAudits() {
  if (pinAuditFlushPromise) return pinAuditFlushPromise;
  pinAuditFlushPromise = (async () => {
    loadPinSecurityState();
    while (pinAuditEvents.length) {
      const event = pinAuditEvents[0];
      await sendPinLockoutAudit(event);
      if (pinAuditEvents[0] === event) pinAuditEvents.shift();
      savePinSecurityState();
    }
  })().catch(() => {}).finally(() => { pinAuditFlushPromise = null; });
  return pinAuditFlushPromise;
}

function readPinRecord() {
  const file = pinRecordPath();
  if (!fs.existsSync(file)) return null;
  if (!safeStorage.isEncryptionAvailable()) throw new Error('Secure PIN storage is unavailable');
  try {
    const encrypted = fs.readFileSync(file, 'utf8').trim();
    if (!encrypted) return null;
    const decrypted = safeStorage.decryptString(Buffer.from(encrypted, 'base64'));
    if (!decrypted) return null;
    const record = JSON.parse(decrypted);
    if (!record || !record.salt || !record.verifier) return null;
    return record;
  } catch (e) {
    console.error('Error reading PIN record:', e);
    return null;
  }
}

function validatePin(pin) {
  return typeof pin === 'string' && /^\d{6}$/.test(pin);
}

function savePin(pin) {
  if (!validatePin(pin)) throw new Error('PIN must contain exactly 6 digits');
  if (!safeStorage.isEncryptionAvailable()) throw new Error('Secure PIN storage is unavailable');
  const salt = crypto.randomBytes(16);
  const record = {
    salt: salt.toString('hex'),
    verifier: crypto.scryptSync(pin, salt, 64).toString('hex')
  };
  const file = pinRecordPath();
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const temporaryFile = file + '.tmp';
  fs.writeFileSync(temporaryFile, safeStorage.encryptString(JSON.stringify(record)).toString('base64'), { mode: 0o600 });
  fs.renameSync(temporaryFile, file);
}

function verifyPin(pin) {
  loadPinSecurityState();
  if (Date.now() < pinLockedUntil) {
    const seconds = Math.ceil((pinLockedUntil - Date.now()) / 1000);
    return { ok: false, locked: true, seconds };
  }
  const record = readPinRecord();
  if (!record || !validatePin(pin)) return { ok: false };
  const expected = Buffer.from(record.verifier, 'hex');
  const actual = crypto.scryptSync(pin, Buffer.from(record.salt, 'hex'), expected.length);
  if (expected.length === actual.length && crypto.timingSafeEqual(expected, actual)) {
    pinFailures = 0;
    pinLockedUntil = 0;
    savePinSecurityState();
    return { ok: true };
  }
  pinFailures++;
  if (pinFailures >= 5) {
    pinFailures = 0;
    pinLockedUntil = Date.now() + 30_000;
    pinAuditEvents.push({ timestamp: new Date().toISOString(), attempts: 5, lockout_seconds: 30 });
    savePinSecurityState();
    flushPinLockoutAudits();
    return { ok: false, locked: true, seconds: 30 };
  }
  savePinSecurityState();
  return { ok: false, remaining: 5 - pinFailures };
}

ipcMain.handle('pin-status', () => {
  try {
    loadPinSecurityState();
    const configured = Boolean(readPinRecord());
    return {
      configured,
      skipped: !configured && fs.existsSync(pinSetupSkippedPath()),
      lockoutSeconds: Math.max(0, Math.ceil((pinLockedUntil - Date.now()) / 1000))
    };
  } catch (e) {
    console.error('Error in pin-status handler:', e);
    return {
      configured: false,
      skipped: false,
      lockoutSeconds: 0
    };
  }
});
ipcMain.handle('open-folder', async (_event, folderPath) => {
  if (typeof folderPath !== 'string' || !folderPath.trim()) throw new Error('Folder path is required');
  // Relative data paths are rooted beside the installed service/app binaries,
  // never at the Windows service working directory (often System32).
  const installDir = path.dirname(app.getPath('exe'));
  const resolvedPath = path.isAbsolute(folderPath)
    ? path.normalize(folderPath)
    : path.resolve(installDir, folderPath);
  const stats = await fs.promises.stat(resolvedPath);
  if (!stats.isDirectory()) throw new Error('Path is not a folder');
  const error = await shell.openPath(resolvedPath);
  if (error) throw new Error(error);
  return { ok: true };
});
ipcMain.handle('pin-setup', (_event, pin) => {
  if (readPinRecord()) throw new Error('PIN is already configured');
  savePin(pin);
  fs.rmSync(pinSetupSkippedPath(), { force: true });
  return { ok: true };
});
ipcMain.handle('pin-skip-setup', () => {
  if (!readPinRecord()) {
    fs.mkdirSync(app.getPath('userData'), { recursive: true });
    fs.writeFileSync(pinSetupSkippedPath(), 'skipped');
  }
  return { ok: true };
});
ipcMain.handle('pin-verify', (_event, pin) => verifyPin(pin));
ipcMain.handle('pin-change', (_event, currentPin, nextPin) => {
  const result = verifyPin(currentPin);
  if (!result.ok) return result;
  savePin(nextPin);
  return { ok: true };
});
ipcMain.handle('pin-disable', (_event, currentPin) => {
  const result = verifyPin(currentPin);
  if (!result.ok) return result;
  fs.rmSync(pinRecordPath(), { force: true });
  fs.mkdirSync(app.getPath('userData'), { recursive: true });
  fs.writeFileSync(pinSetupSkippedPath(), 'skipped');
  return { ok: true };
});
ipcMain.handle('pin-audit-flush', () => flushPinLockoutAudits());

async function queryServiceState() {
  const { stdout } = await execFileAsync('sc.exe', ['query', serviceName], { windowsHide: true });
  const match = stdout.match(/STATE\s*:\s*\d+\s+(\w+)/i);
  if (!match) throw new Error('อ่านสถานะ Windows Service ไม่สำเร็จ');
  return match[1].toUpperCase();
}

ipcMain.handle('service-status', async () => {
  try {
    const state = await queryServiceState();
    return { available: true, state, running: state === 'RUNNING' };
  } catch (error) {
    return { available: false, state: 'UNKNOWN', running: false, error: error.message };
  }
});

ipcMain.handle('machine-info', () => {
  let profileName = process.env.USERNAME || process.env.USER || '';
  try { profileName = os.userInfo().username || profileName; } catch (_) {}
  return { computerName: os.hostname(), profileName };
});

ipcMain.handle('service-control', async (_event, action) => {
  if (action !== 'start' && action !== 'stop') throw new Error('คำสั่ง Service ไม่ถูกต้อง');
  const targetState = action === 'start' ? 'RUNNING' : 'STOPPED';
  const currentState = await queryServiceState();
  if (currentState !== targetState) {
    await execFileAsync('sc.exe', [action, serviceName], { windowsHide: true });
  }

  const deadline = Date.now() + 20000;
  let state = currentState;
  while (Date.now() < deadline) {
    await new Promise(resolve => setTimeout(resolve, 500));
    state = await queryServiceState();
    if (state === targetState) return { available: true, state, running: state === 'RUNNING' };
  }
  throw new Error(`หมดเวลารอ Windows Service เปลี่ยนเป็น ${targetState} (สถานะล่าสุด: ${state})`);
});

ipcMain.on('window-minimize', () => mainWindow?.minimize());
ipcMain.on('window-maximize', () => {
  if (!mainWindow) return;
  mainWindow.isMaximized() ? mainWindow.unmaximize() : mainWindow.maximize();
});
ipcMain.on('window-close', () => mainWindow?.close());

function createWindow() {
  const rootDir = path.resolve(__dirname, '..');

  mainWindow = new BrowserWindow({
    width: 1200,
    height: 800,
    minWidth: 980,
    minHeight: 650,
    frame: false, // Frameless modern desktop window
    titleBarStyle: 'hidden',
    backgroundColor: '#0f172a',
    icon: path.join(rootDir, 'icon', 'app.ico'),
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      nodeIntegration: false,
      contextIsolation: true
    }
  });

  mainWindow.maximize();
  mainWindow.loadFile(path.join(rootDir, 'internal', 'ui', 'index.html'));
  mainWindow.on('closed', () => { mainWindow = null; });
}

app.whenReady().then(createWindow);

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') {
    app.quit();
  }
});
