# Raspberry Pi 4 Setup

## Hardware
- Raspberry Pi 4
- SD Card: 128 GB SDXC (Class 10)

## OS Image
- **Raspberry Pi OS Lite (64-bit)** — Trixie (Debian 13)
- Kernel: 6.12
- Release: 2025-12-04
- Download: `https://downloads.raspberrypi.com/raspios_lite_arm64/images/raspios_lite_arm64-2025-12-04/2025-12-04-raspios-trixie-arm64-lite.img.xz`

## Configuration (cloud-init on boot partition)

### network-config
```yaml
network:
  version: 2
  wifis:
    wlan0:
      dhcp4: true
      optional: false
      access-points:
        "HOME146":
          password: "lol770905"
      regulatory-domain: RU
```

### user-data
```yaml
#cloud-config
hostname: raspberrypi
manage_etc_hosts: true
ssh_pwauth: true

users:
- name: pi
  groups: users,adm,dialout,audio,netdev,video,plugdev,cdrom,games,input,gpio,spi,i2c,render,sudo
  shell: /bin/bash
  lock_passwd: false
  passwd: <sha256-hash-of-raspberry>
  sudo: ALL=(ALL) NOPASSWD:ALL

packages:
- avahi-daemon

runcmd:
- systemctl enable ssh
- systemctl start ssh
- systemctl enable avahi-daemon
- systemctl start avahi-daemon
```

### meta-data
- `dsmode: local`
- `instance_id` — менять при каждом обновлении конфигов, чтобы cloud-init перечитал

## Credentials
- User: `pi`
- Password: `raspberry`
- SSH: enabled

## Network
- WiFi SSID: HOME146
- Home network: 192.168.0.x
- Pi MAC prefix: DC:A6:32 (Raspberry Pi Foundation)

## TODO
- [ ] Настроить reverse SSH tunnel для удалённого доступа (нужен VPS со статикой)
- [ ] Рассмотреть альтернативы: Tailscale, Cloudflare Tunnel, WireGuard
