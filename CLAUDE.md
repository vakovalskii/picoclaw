# PicoClaw

## Raspberry Pi 4 (4GB RAM, 4 cores)
- **OS:** Raspberry Pi OS Lite 64-bit (Trixie/Debian 13), kernel 6.12
- **SD Card:** 128 GB SDXC (118 GB usable)
- **IP (local):** 192.168.0.237
- **Hostname:** raspberrypi
- **User:** pi / raspberry
- **SSH:** `ssh pi@192.168.0.237` (ed25519 + RSA keys deployed)
- **WiFi:** HOME146 / lol770905
- **MAC:** DC:A6:32:40:AA:04

## Setup Notes
- Image: cloud-init based (Trixie). Files on boot partition: `user-data`, `network-config`, `meta-data`
- WiFi must be configured via `write_files` (NetworkManager .nmconnection), NOT via `network-config` (netplan) — netplan WiFi does not work on this image
- Change `instance_id` in `meta-data` to force cloud-init re-run
- Full setup docs: `docs-init/raspberry-pi-setup.md`

## TODO
- [ ] Set up persistent remote access (reverse SSH tunnel / Tailscale / Cloudflare Tunnel)
