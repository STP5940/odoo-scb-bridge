const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('electronAPI', {
  minimize: () => ipcRenderer.send('window-minimize'),
  maximize: () => ipcRenderer.send('window-maximize'),
  close: () => ipcRenderer.send('window-close'),
  getServiceStatus: () => ipcRenderer.invoke('service-status'),
  getMachineInfo: () => ipcRenderer.invoke('machine-info'),
  controlService: action => ipcRenderer.invoke('service-control', action)
});
