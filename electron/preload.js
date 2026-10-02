const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('electronAPI', {
  minimize: () => ipcRenderer.send('window-minimize'),
  maximize: () => ipcRenderer.send('window-maximize'),
  close: () => ipcRenderer.send('window-close'),
  getServiceStatus: () => ipcRenderer.invoke('service-status'),
  getMachineInfo: () => ipcRenderer.invoke('machine-info'),
  getPinStatus: () => ipcRenderer.invoke('pin-status'),
  setupPin: pin => ipcRenderer.invoke('pin-setup', pin),
  skipPinSetup: () => ipcRenderer.invoke('pin-skip-setup'),
  verifyPin: pin => ipcRenderer.invoke('pin-verify', pin),
  changePin: (currentPin, nextPin) => ipcRenderer.invoke('pin-change', currentPin, nextPin),
  disablePin: currentPin => ipcRenderer.invoke('pin-disable', currentPin),
  controlService: action => ipcRenderer.invoke('service-control', action)
});
