#!/bin/sh
set -eu
requested_mode="${1:-${CRABBOX_DESKTOP_THEME:-}}"
user="${CRABBOX_DESKTOP_USER:-crabbox}"
home_dir="$(getent passwd "$user" | cut -d: -f6)"
if [ -z "$home_dir" ]; then
  home_dir="/home/$user"
fi
config_dir="$home_dir/.config"
mode="$requested_mode"
if [ -z "$mode" ] && [ -f "$config_dir/crabbox/desktop-theme" ]; then
  mode="$(cat "$config_dir/crabbox/desktop-theme" 2>/dev/null || true)"
fi
case "$mode" in
  light|dark) ;;
  *) mode=dark ;;
esac
if [ "$mode" = "light" ]; then
  gtk_theme=Adwaita
  gtk_prefer_dark_ini=0
  gsettings_scheme=prefer-light
  terminal_fg="#1f2937"
  terminal_bg="#f8fafc"
  labwc_title_bg="#f3f4f6"
  labwc_title_fg="#111827"
  labwc_inactive_title_bg="#e5e7eb"
  labwc_inactive_title_fg="#374151"
  labwc_border="#cbd5e1"
  terminal_menu_bg="#f3f4f6"
  terminal_menu_fg="#111827"
  terminal_menu_hover_bg="#e5e7eb"
  wallpaper_bg="#e7eef7"
  wallpaper_panel="#d6e7f2"
  wallpaper_accent="#0891b2"
  wallpaper_grid="#b9c7d7"
else
  gtk_theme=Adwaita-dark
  gtk_prefer_dark_ini=1
  gsettings_scheme=prefer-dark
  terminal_fg="#e5e7eb"
  terminal_bg="#000000"
  labwc_title_bg="#1f2329"
  labwc_title_fg="#e5e7eb"
  labwc_inactive_title_bg="#111827"
  labwc_inactive_title_fg="#9ca3af"
  labwc_border="#30363d"
  terminal_menu_bg="#2b2f36"
  terminal_menu_fg="#d1d5db"
  terminal_menu_hover_bg="#374151"
  wallpaper_bg="#0d1117"
  wallpaper_panel="#111827"
  wallpaper_accent="#22d3ee"
  wallpaper_grid="#1f2937"
fi
if [ "$(id -u)" -eq 0 ]; then
  install -d -m 0700 -o "$user" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0"
else
  mkdir -p "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"
  chmod 0700 "$config_dir" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"
fi
printf '%s\n' "$mode" > "$config_dir/crabbox/desktop-theme"
for gtk_dir in "$config_dir/gtk-3.0" "$config_dir/gtk-4.0"; do
  cat > "$gtk_dir/settings.ini" <<EOF
[Settings]
gtk-theme-name=$gtk_theme
gtk-icon-theme-name=Adwaita
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
done
cat > "$home_dir/.gtkrc-2.0" <<EOF
gtk-theme-name="$gtk_theme"
gtk-icon-theme-name="Adwaita"
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
if [ "$(id -u)" -eq 0 ]; then
  chown -R "$user" "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$home_dir/.gtkrc-2.0"
fi
if [ -f /var/lib/crabbox/desktop.env ]; then
  . /var/lib/crabbox/desktop.env
fi
display="${DISPLAY:-:0}"
runtime="${XDG_RUNTIME_DIR:-/tmp/crabbox-runtime-$(id -u "$user")}"
dbus_address="${DBUS_SESSION_BUS_ADDRESS:-}"
if [ -z "$dbus_address" ]; then
  labwc_pid="$(pgrep -u "$user" -n -x labwc 2>/dev/null || true)"
  if [ -n "$labwc_pid" ] && [ -r "/proc/$labwc_pid/environ" ]; then
    dbus_address="$(tr '\0' '\n' < "/proc/$labwc_pid/environ" | sed -n 's/^DBUS_SESSION_BUS_ADDRESS=//p' | head -n1)"
  fi
fi
set_gnome_terminal_theme() {
  profiles="$(gsettings get org.gnome.Terminal.ProfilesList list 2>/dev/null | tr -d "[],'" || true)"
  default_profile="$(gsettings get org.gnome.Terminal.ProfilesList default 2>/dev/null | tr -d "'" || true)"
  if [ -n "$default_profile" ] && ! printf ' %s ' "$profiles" | grep -q " $default_profile "; then
    profiles="$profiles $default_profile"
  fi
  for profile in $profiles; do
    [ -n "$profile" ] || continue
    profile_path="/org/gnome/terminal/legacy/profiles:/:$profile/"
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" use-theme-colors false >/dev/null 2>&1 || true
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" foreground-color "$terminal_fg" >/dev/null 2>&1 || true
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" background-color "$terminal_bg" >/dev/null 2>&1 || true
    gsettings set "org.gnome.Terminal.Legacy.Profile:$profile_path" use-transparent-background false >/dev/null 2>&1 || true
  done
}
set_gtk_chrome_theme() {
  cat > "$config_dir/gtk-3.0/gtk.css" <<EOF
menubar, .menubar {
  background-color: $terminal_menu_bg;
  color: $terminal_menu_fg;
}
menubar menuitem, menubar menuitem label {
  color: $terminal_menu_fg;
}
menubar menuitem:hover {
  background-color: $terminal_menu_hover_bg;
  color: $terminal_menu_fg;
}
EOF
}
set_labwc_theme() {
  mkdir -p "$config_dir/labwc"
  cat > "$config_dir/labwc/themerc-override" <<EOF
window.active.title.bg.color: $labwc_title_bg
window.active.label.text.color: $labwc_title_fg
window.inactive.title.bg.color: $labwc_inactive_title_bg
window.inactive.label.text.color: $labwc_inactive_title_fg
window.active.border.color: $labwc_border
window.inactive.border.color: $labwc_border
window.active.button.unpressed.image.color: $labwc_title_fg
window.inactive.button.unpressed.image.color: $labwc_inactive_title_fg
window.active.button.hover.image.color: $labwc_title_fg
window.inactive.button.hover.image.color: $labwc_inactive_title_fg
window.active.button.pressed.image.color: $labwc_title_fg
window.inactive.button.pressed.image.color: $labwc_inactive_title_fg
EOF
  if command -v labwc >/dev/null 2>&1; then
    labwc_pid="$(pgrep -u "$user" -n -x labwc 2>/dev/null || true)"
    if [ -n "$labwc_pid" ]; then
      LABWC_PID="$labwc_pid" XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" labwc --reconfigure >/dev/null 2>&1 || kill -HUP "$labwc_pid" >/dev/null 2>&1 || true
    fi
  fi
}
set_desktop_background() {
  wallpaper_file="$config_dir/crabbox/desktop-background-$mode.svg"
  cat > "$wallpaper_file" <<EOF
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1920 1080">
  <rect width="1920" height="1080" fill="$wallpaper_bg"/>
  <path d="M0 720 C360 620 520 760 860 650 C1210 540 1430 660 1920 520 L1920 1080 L0 1080 Z" fill="$wallpaper_panel"/>
  <g stroke="$wallpaper_grid" stroke-width="1" opacity="0.45">
    <path d="M0 180 H1920M0 360 H1920M0 540 H1920M0 720 H1920M0 900 H1920"/>
    <path d="M240 0 V1080M480 0 V1080M720 0 V1080M960 0 V1080M1200 0 V1080M1440 0 V1080M1680 0 V1080"/>
  </g>
  <path d="M220 740 C520 520 790 910 1090 670 S1510 520 1710 700" fill="none" stroke="$wallpaper_accent" stroke-width="18" stroke-linecap="round" opacity="0.8"/>
  <rect x="1320" y="180" width="360" height="170" rx="18" fill="$wallpaper_accent" opacity="0.12"/>
</svg>
EOF
  if command -v swaybg >/dev/null 2>&1; then
    pkill -u "$user" -x swaybg >/dev/null 2>&1 || true
    (
      if XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -i "$wallpaper_file" -m fill; then
        exit 0
      else
        status=$?
      fi
      [ "$status" -lt 128 ] || exit "$status"
      exec env XDG_RUNTIME_DIR="$runtime" WAYLAND_DISPLAY="${WAYLAND_DISPLAY:-wayland-0}" swaybg -c "$wallpaper_bg"
    ) </dev/null >/tmp/crabbox-swaybg.log 2>&1 &
  fi
}
target_uid="$(id -u "$user" 2>/dev/null || printf 0)"
if [ "$(id -u)" -eq 0 ] && [ "$target_uid" -ne 0 ]; then
  su "$user" -s /bin/sh -c "CRABBOX_DESKTOP_USER='$user' CRABBOX_DESKTOP_THEME='$mode' DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 /usr/local/bin/crabbox-configure-desktop-theme '$mode'" || true
  exit 0
fi
if command -v gsettings >/dev/null 2>&1; then
  if [ "$(id -u)" -eq 0 ]; then
    su "$user" -s /bin/sh -c "DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme '$gsettings_scheme' >/dev/null 2>&1 || true"
    su "$user" -s /bin/sh -c "DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface gtk-theme '$gtk_theme' >/dev/null 2>&1 || true"
  else
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme" >/dev/null 2>&1 || true
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface gtk-theme "$gtk_theme" >/dev/null 2>&1 || true
    DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 set_gnome_terminal_theme
  fi
fi
set_gtk_chrome_theme
set_labwc_theme
set_desktop_background
if [ "$(id -u)" -eq 0 ] && pgrep -u "$user" -x gnome-panel >/dev/null 2>&1; then
  pkill -TERM -u "$user" -x gnome-panel >/dev/null 2>&1 || true
  su "$user" -s /bin/sh -c "DISPLAY='$display' XDG_RUNTIME_DIR='$runtime' DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 GTK_THEME='$gtk_theme' nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &" >/dev/null 2>&1 || true
elif [ "$(id -u)" -ne 0 ] && pgrep -x gnome-panel >/dev/null 2>&1; then
  pkill -TERM -x gnome-panel >/dev/null 2>&1 || true
  DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 GTK_THEME="$gtk_theme" nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &
fi
previous_terminal_theme="$(cat "$config_dir/crabbox/gnome-terminal-theme" 2>/dev/null || true)"
printf '%s\n' "$mode" > "$config_dir/crabbox/gnome-terminal-theme"
if [ "$(id -u)" -ne 0 ] && [ "$mode" = dark ] && command -v gnome-terminal >/dev/null 2>&1 && { [ "$previous_terminal_theme" != "$mode" ] || ! pgrep -u "$(id -u)" -f '/gnome-terminal-server' >/dev/null 2>&1; }; then
  (sleep 0.4; DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 GTK_THEME="$gtk_theme" NO_AT_BRIDGE=1 gnome-terminal -- bash -l >/tmp/crabbox-gnome-terminal.log 2>&1 &) >/dev/null 2>&1 &
fi
