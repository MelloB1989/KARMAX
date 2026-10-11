# Fleet console on PC2 (`packaging/console/`)

Turns PC2 into the console's host: a virtual display, Chrome in kiosk mode on the panel,
`fleet-console` as a user service, and RustDesk sharing that display. It assumes
Ubuntu/Debian (apt, GRUB, initramfs-tools) and Xorg, as the spec requires.

| File | What it is |
|---|---|
| `install.sh` | idempotent installer, run as the kiosk user; root steps go through `sudo` |
| `console.env.example` | every value it uses (connector, mode, user, versions, URLs); copy to `console.env` |
| `fleet-console.service` | user unit: the console, `Restart=always`, lingering enabled |
| `fleet-kiosk.service` | system unit: autologin on tty1, `xinit`, `kiosk-session.sh` |
| `kiosk-session.sh` | inside X: no blanking, no cursor, Chrome `--kiosk` on the login URL, restarted on exit |
| `edid/make-edid.py` | writes a valid EDID 1.3 block for one fixed mode |
| `test.sh` | the tests below; needs no root |

## Run it

```
cp console.env.example console.env     # edit: CONNECTOR, KIOSK_USER, paths, RUSTDESK_URL/SHA256
./install.sh --dry-run                 # prints every command; changes nothing
./install.sh                           # as the kiosk user; sudo asks where it must
./install.sh --only token              # one step; steps: check edid cmdline packages units rustdesk token
```

The steps: `check` (connector, render node, `video`/`render` groups), `edid`, `cmdline`,
`packages`, `units`, `rustdesk`, `token`. Running it again changes nothing that is already
right. It prints no secret: the client token is written `0600` into `CONSOLE_DIR` and read
by `kiosk-session.sh` at the moment Chrome starts. `RUSTDESK_SHA256` must match the `.deb`,
or the install stops.

Before the first real run, put the client's `console.yaml` (from `fleetctl render`) in
`CONSOLE_DIR` and the `fleet-console` binary at `CONSOLE_BIN`.

## The reboot

The virtual output needs kernel parameters, so the display appears only after one reboot.
`install.sh` adds this to `GRUB_CMDLINE_LINUX_DEFAULT` (once):

```
video=<CONNECTOR>:1920x1080@60e drm.edid_firmware=<CONNECTOR>:edid/fleet-1920x1080.bin
```

(`drm_kms_helper.edid_firmware` on a kernel that only has that name; `EDID_PARAM` overrides.)
The first copy of the original is kept as `/etc/default/grub.fleet-console.bak` and is never
overwritten. SSH works with or without the new parameters.

### Undo

From a shell (SSH is enough):

```
sudo cp -p /etc/default/grub.fleet-console.bak /etc/default/grub && sudo update-grub && sudo update-initramfs -u
```

If the machine will not show anything after the reboot: at the GRUB menu press `e`, delete
the two added parameters from the `linux` line, and boot with `Ctrl+x`. Then run the line above.

### If the virtual output does not come up

Some GPU/connector pairs ignore a forced EDID. The fallback is an HDMI **dummy plug** in the
connector named by `CONNECTOR`; the rest of the setup is unchanged. Undo the kernel
parameters as above if you go this way.

## RustDesk

The `.deb` is pinned (`RUSTDESK_URL`, `RUSTDESK_VERSION`, `RUSTDESK_SHA256`) and its service is
pointed at display `:0` with the kiosk user's `~/.Xauthority`. The operator sets the unattended
password by hand, and `install.sh` never sees it:

```
sudo rustdesk --password <a password you choose>
```

`RUSTDESK_MODE=public` uses RustDesk's public rendezvous servers. `RUSTDESK_MODE=direct` sets
`direct-server Y` and the direct access port, and points `custom-rendezvous-server`, `relay-server`
and `api-server` at 127.0.0.1 and turns off LAN discovery. `direct-server Y` alone would keep the
public registration. RustDesk has no documented off switch, so this is the maintainers' workaround:
exposed is TCP `RUSTDESK_DIRECT_PORT` (password-protected); not used are the public servers and
connecting by ID. It was not verified against a live RustDesk 1.4.9, so after installing run
`ss -tnp | grep rustdesk` while idle and confirm no connection to a public address, and check from
the Mac that a direct connection works. If a public connection appears, do not rely on direct-only.

## Tests

```
packaging/console/test.sh
```

The dry run is asserted to contain the GRUB line, the undo line, the EDID, Chrome and RustDesk
(with its checksum), both units and the token step, and to change nothing. `grub_apply` is
checked to be idempotent on a copy of a GRUB file. `edid/test_make_edid.py` checks the EDID's
length, header, version, checksum and preferred timing. `shellcheck` runs if it is installed;
it was not on the machine this was written on, so run `test.sh` once where it is.
