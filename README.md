# Odoo SCB Data Bridge System

ระบบโอนย้ายไฟล์อัตโนมัติสำหรับเชื่อมต่อข้อมูลระหว่าง Odoo ERP และ ธนาคารไทยพาณิชย์ (SCB) พัฒนาด้วยสถาปัตยกรรมระดับองค์กร:
- **Core Microservice (Go)**: ทำงานเป็น Windows Service เบื้องหลัง รองรับ Embedded SFTP Inbound Server, Cron Outbound Scheduler (FTPS/SFTP), Local REST API, และ SQLite Database สำหรับบันทึก Audit Logs และการตั้งค่า
- **Service Monitor GUI (Electron Desktop App)**: หน้าต่างควบคุมและติดตามสถานะแบบ Desktop Application สไตล์ Visual Studio Code / Microsoft Fluent UI ทันสมัย ไม่เปิดหน้าต่าง Command Prompt
- **Installer Wizard (Inno Setup)**: ตัวติดตั้งสำเร็จรูปพร้อมลงทะเบียน Windows Service และ Firewall Rules อัตโนมัติ

---

## สถาปัตยกรรมระบบ (Project Architecture)

```
odoo-scb-bridge/
├── cmd/
│   └── service/            # Core Windows Service entry point (Go)
├── electron/               # Electron Desktop App (Main & Preload Process)
├── internal/
│   ├── api/                # Local REST API Server (Port 9527) สำหรับ GUI
│   ├── database/           # SQLite Database Layer (Audit Logs, Accounts, Config)
│   ├── models/             # Data Entities & Transfer Objects
│   ├── outbound/           # Outbound File Transfer Handlers (SFTP / FTPS)
│   ├── scheduler/          # Cron Job Scheduler สำหรับรอบส่งไฟล์
│   ├── server/sftp/        # Embedded Inbound SFTP Server
│   ├── ui/                 # Modern GUI Web Assets (HTML/CSS/JS)
│   └── utils/              # Hash, Crypto, File, Logging Utilities
├── icon/                   # Application Assets (.ico, .png)
├── dist/                   # โฟลเดอร์เก็บโปรแกรม Binaries
│   ├── bridge_service.exe  # Core Windows Service
│   └── ServiceMonitor.exe  # Electron GUI Application
├── output/                 # โฟลเดอร์เก็บตัวติดตั้ง Setup Wizard
│   └── OdooSCBBridgeSetup.exe
├── build.bat               # สคริปต์คอมไพล์ระบบทั้งหมดในคำสั่งเดียว
├── installer.iss           # สคริปต์ Inno Setup สำหรับสร้าง Installer
└── package.json            # Electron packaging configuration
```

---

## การคอมไพล์และ Build โปรแกรม (Build Instructions)

เพียงรันไฟล์ [build.bat](file:///c:/Users/LENOVO/Documents/GitHub/odoo-scb-bridge/build.bat) ผ่าน Command Prompt หรือ Terminal:

```cmd
build.bat
```

ก่อนเริ่ม Build สคริปต์จะแสดงเมนูให้เลือก Build only (ค่าเริ่มต้น คงเลขเดิม), Update Version for Production หรือ Exit/Cancel หากเลือกอัปเดต จะให้เลือกระดับ Patch, Minor หรือ Major และเพิ่มเลขระดับนั้น 1 ตามหลัก Semantic Versioning

ขั้นตอนการทำงานของสคริปต์:
1. คอมไพล์ Go Microservice เป็น `dist\bridge_service.exe`
2. คอมไพล์และแพ็กเกจ Electron Desktop App เป็น `dist\ServiceMonitor.exe` (รวมทั้ง `dist_electron\win-unpacked`)
3. คอมไพล์ Inno Setup Wizard สร้างเป็น `output\OdooSCBBridgeSetup.exe`

---

## ขั้นตอนการติดตั้งใช้งาน (Installation & Deployment)

1. นำไฟล์ติดตั้งจากโฟลเดอร์ **`output\OdooSCBBridgeSetup.exe`** ไปติดตั้งลงบนเครื่องเป้าหมาย
2. รันตัวติดตั้งในฐานะ Administrator
3. ตัวติดตั้งจะทำการ:
   - สร้างโฟลเดอร์สำหรับรับ-ส่งไฟล์ (`data\inbound`, `data\outbound`, `data\temp`)
   - เปิดพอร์ต Firewall สำหรับ SFTP (พอร์ต 2222)
   - ติดตั้งและสั่งเริ่มทำงาน **OdooSCBBridge Windows Service** อัตโนมัติ
   - สร้าง Desktop Shortcut และเปิดหน้าจอ **Odoo SCB Bridge Monitor** ขึ้นมาพร้อมใช้งานทันที

---

## คุณสมบัติเด่น (Features)

- **Audit Logs ละเอียดทุกกิจกรรม**: บันทึก Log การเข้าสู่ระบบ SFTP, การรับไฟล์ Inbound พร้อม Hash (SHA-256), และผลการส่งไฟล์ Outbound ลง SQLite
- **SFTP Account Management**: เพิ่ม แก้ไข เปิด/ปิดการใช้งาน หรือลบบัญชี SFTP ได้แบบ Real-time ผ่านหน้า GUI
- **Inbound & Outbound Automation**: ตั้งรอบเวลาส่งไฟล์อัตโนมัติ พร้อมปุ่ม Manual Trigger ส่งไฟล์ทดสอบทันที
- **Enterprise Dark UI**: หน้ากากโปรแกรมแบบ Modern VS Code Theme ดูสบายตา ตอบสนองรวดเร็ว มี Notification Toast แจ้งเตือนสถานะ

## Offline Product Activation

This build uses a machine-bound, perpetual offline license. On an unlicensed installation, the monitor displays a request code containing the hashed device ID plus the computer and profile names. The license issuer signs a license for that device with Ed25519. The Windows service verifies the signature locally; the SFTP listener, scheduler, and data-management API remain disabled until verification succeeds.

### Customer activation

1. Install and open Odoo SCB Bridge Monitor.
2. Copy the **Activation Request Code** and send it to the license administrator through an approved channel.
3. Paste the returned **License Key** and choose **Activate**.

### Internal license issuer

- The Ed25519 signing key is kept in `.secrets/issuer-ed25519-private.pem`; it is ignored by Git and is not copied into the installer. The desktop app does not need this private key to start or run; it contains only the public verification key. Store the private key in an encrypted, access-controlled backup and restore it to the issuer machine after cloning the repository. The issuer build script stops with a clear message if the key is missing.
- Never replace a missing signing key with a random key while continuing to use existing app builds. A replacement key requires changing `publicKeyBase64` in `internal/license/license.go` to its matching public key, then rebuilding and redistributing the app. Previously issued licenses will not verify against the replacement public key. If no backup exists, treat this as a signing-key rotation and plan customer reactivation before distributing the new build.
- Build the internal-only issuer with `build-license-issuer.bat` and run `tools\OdooSCBBridgeLicenseIssuer.exe` from the repository root. Paste the customer's request code. Never give customers the issuer executable together with the private key.
- A license is valid only for the machine ID in its request code. Reinstallation on the same Windows machine normally preserves activation; hardware/OS changes may require re-issuance.

The installed application contains only the public verification key. Signed licenses cannot be edited or created without the private key. No software running entirely on a customer's computer can guarantee resistance to binary patching or an administrator using a debugger; strict tamper prevention requires a trusted online activation service and signed/code-integrity enforcement.
