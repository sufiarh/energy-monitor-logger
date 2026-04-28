# Changelog

All notable changes to this project are documented here.

---

## [1.0.0] — 2026-04-20

### Added
- PZEM-016 polling via Modbus RTU at 1-second interval
- Web dashboard with cookie-based session authentication (24-hour TTL)
- Power quality detection: under/over voltage and frequency alarms
- Full-resolution data logging to `history.jsonl` (1 second)
- Periodic snapshot logging to `history.csv` (every 10 minutes when NORMAL, immediately on abnormal events)
- System event log to `events.jsonl` (connect, disconnect, alarms, config updates)
- Daily energy tracking with automatic midnight rollover, persisted to `energy_state.json`
- Daily electricity cost estimate based on configurable tariff
- REST API endpoints for export, energy reset, CSV reset, and live config
- Live tariff and rated-power configuration via the dashboard UI
- Engineer Mode: detailed technical metrics view
- Cloudflare Tunnel support for secure public access
- Auto-reconnect on sensor disconnection (triggers after 3 consecutive errors)
- systemd service file for autostart on boot
