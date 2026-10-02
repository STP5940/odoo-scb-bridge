const { app, BrowserWindow, ipcMain } = require('electron');
const { execFile } = require('child_process');
const { promisify } = require('util');
const path = require('path');

const execFileAsync = promisify(execFile);
const serviceName = 'OdooSCBBridge';
let mainWindow;

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

  mainWindow.loadFile(path.join(rootDir, 'internal', 'ui', 'index.html'));
  mainWindow.on('closed', () => { mainWindow = null; });
}

app.whenReady().then(createWindow);

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') {
    app.quit();
  }
});
