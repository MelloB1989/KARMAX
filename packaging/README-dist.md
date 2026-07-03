# KARMAX

You've downloaded a KARMAX release. To install and run it as a background
service:

**Linux / macOS**
```bash
./install.sh
```

**Windows** (from this folder)
```powershell
powershell -ExecutionPolicy Bypass -File install.ps1
```

The installer places the `karmax` binary in `~/.local/bin` (Linux/macOS) or
`%LOCALAPPDATA%\KARMAX` (Windows), seeds config in `~/.karmax`, and registers a
background service that starts on login and restarts automatically if it ever
stops:

- **Linux** — a systemd `--user` service (`Restart=always`, lingering so it
  survives logout).
- **macOS** — a launchd LaunchAgent (`KeepAlive`, relaunches instantly).
- **Windows** — a hidden Scheduled Task with a supervisor loop.

Then configure it: edit `~/.karmax/karmax.yaml` and set your provider
credentials in `~/.karmax/.env`. See
https://github.com/MelloB1989/KARMAX for details.
