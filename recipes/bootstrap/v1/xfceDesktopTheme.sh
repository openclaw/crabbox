#!/bin/bash
set -eu
requested_mode="${1:-}"
[ -n "$requested_mode" ] || requested_mode="${CRABBOX_DESKTOP_THEME:-}"
css_style={{cssStyle}}
user="${CRABBOX_DESKTOP_USER:-crabbox}"
home_dir="$(getent passwd "$user" | cut -d: -f6)"
[ -n "$home_dir" ] || { echo "Desktop user does not exist: $user" >&2; exit 1; }
user_id="$(id -u "$user")"
if [ -n "${DISPLAY:-}" ] && [ "$(id -u)" != "$user_id" ]; then
  exec runuser -u "$user" -- env DISPLAY="$DISPLAY" CRABBOX_DESKTOP_USER="$user" /usr/local/bin/crabbox-configure-desktop-theme "$requested_mode"
fi

if [ -n "${DISPLAY:-}" ]; then
  . /usr/local/lib/crabbox/xfce-session.sh
fi

config_dir="$home_dir/.config"
mode="$requested_mode"
if [ -z "$mode" ] && [ -f "$config_dir/crabbox/desktop-theme" ]; then
  mode="$(cat "$config_dir/crabbox/desktop-theme")"
fi
case "$mode" in light|dark) ;; *) mode=dark ;; esac
if [ "$mode" = light ]; then
  gtk_theme=Adwaita
  gtk_prefer_dark=false
  gtk_prefer_dark_ini=0
  gsettings_scheme=prefer-light
  root_color="#f4f6f8"
  terminal_fg="#1f2937"
  terminal_bg="#f8fafc"
  terminal_cursor="#111827"
  panel_rgba="0.94 0.95 0.97 1"
  panel_css_bg="#eef2f7"
  panel_css_fg="#111827"
  gtk_candidates="Arc Greybird Adwaita"
  xfwm_candidates="Arc Greybird Daloa Default"
else
  gtk_theme=Adwaita-dark
  gtk_prefer_dark=true
  gtk_prefer_dark_ini=1
  gsettings_scheme=prefer-dark
  root_color="#20242b"
  terminal_fg="#e5e7eb"
  terminal_bg="#111827"
  terminal_cursor="#f3f4f6"
  panel_rgba="0.12 0.13 0.15 1"
  panel_css_bg="#20242b"
  panel_css_fg="#e5e7eb"
  gtk_candidates="Arc-Dark Greybird-dark Adwaita-dark Greybird"
  xfwm_candidates="Arc-Dark Greybird-dark Daloa Default"
fi
for candidate in $gtk_candidates; do
  if [ -d "/usr/share/themes/$candidate/gtk-3.0" ]; then gtk_theme="$candidate"; break; fi
done
xfwm_theme=Default
for candidate in $xfwm_candidates; do
  if [ -d "/usr/share/themes/$candidate/xfwm4" ]; then xfwm_theme="$candidate"; break; fi
done
mkdir -p "$config_dir/xfce4/xfconf/xfce-perchannel-xml" "$config_dir/xfce4/terminal" "$config_dir/gtk-3.0" "$config_dir/crabbox"
chmod 0700 "$config_dir" "$config_dir/xfce4" "$config_dir/xfce4/xfconf" "$config_dir/xfce4/xfconf/xfce-perchannel-xml" "$config_dir/xfce4/terminal" "$config_dir/gtk-3.0" "$config_dir/crabbox"
printf '%s\n' "$mode" >"$config_dir/crabbox/desktop-theme"
# Before XFCE starts, seed defaults on disk. A live session owns its xfconf files.
if [ -z "${DISPLAY:-}" ]; then
  cat >"$config_dir/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<channel name="xsettings" version="1.0">
  <property name="Net" type="empty"><property name="ThemeName" type="string" value="$gtk_theme"/><property name="IconThemeName" type="string" value="Adwaita"/></property>
  <property name="Gtk" type="empty"><property name="ApplicationPreferDarkTheme" type="bool" value="$gtk_prefer_dark"/></property>
</channel>
EOF
  if [ ! -s "$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml" ]; then
    cat > "$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml" <<XML
<?xml version="1.0" encoding="UTF-8"?>
<channel name="xfwm4" version="1.0">
  <property name="general" type="empty">
    <property name="theme" type="string" value="$xfwm_theme"/>
    <property name="box_move" type="bool" value="false"/>
    <property name="box_resize" type="bool" value="false"/>
    <property name="move_opacity" type="int" value="100"/>
    <property name="resize_opacity" type="int" value="100"/>
    <property name="snap_resist" type="bool" value="false"/>
    <property name="snap_to_border" type="bool" value="false"/>
    <property name="snap_to_windows" type="bool" value="false"/>
    <property name="snap_width" type="int" value="0"/>
    <property name="tile_on_move" type="bool" value="false"/>
    <property name="use_compositing" type="bool" value="false"/>
    <property name="wrap_windows" type="bool" value="false"/>
  </property>
</channel>
XML
  fi
fi
cat >"$config_dir/xfce4/terminal/terminalrc" <<EOF
[Configuration]
ColorForeground=$terminal_fg
ColorBackground=$terminal_bg
ColorCursor=$terminal_cursor
MiscBell=FALSE
EOF
cat >"$config_dir/gtk-3.0/settings.ini" <<EOF
[Settings]
gtk-theme-name=$gtk_theme
gtk-icon-theme-name=Adwaita
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
cat >"$home_dir/.gtkrc-2.0" <<EOF
gtk-theme-name="$gtk_theme"
gtk-icon-theme-name="Adwaita"
gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini
EOF
# Preserve each bootstrap producer's existing panel styling.
panel_css_border="$panel_css_fg"
if [ "$css_style" = coordinator ]; then panel_css_border=transparent; fi
css_file="$config_dir/gtk-3.0/gtk.css"
css_tmp="$(mktemp "$config_dir/gtk-3.0/gtk.css.XXXXXX")"
if [ -f "$css_file" ]; then
  sed '/^[/][*] crabbox desktop theme start [*][/]$/,/^[/][*] crabbox desktop theme end [*][/]$/d' "$css_file" > "$css_tmp" || true
fi
cat >> "$css_tmp" <<EOF
/* crabbox desktop theme start */
.xfce4-panel { background: $panel_css_bg; background-color: $panel_css_bg; color: $panel_css_fg; }
.xfce4-panel * { color: $panel_css_fg; text-shadow: none; -gtk-icon-shadow: none; }
.xfce4-panel button,
.xfce4-panel button.flat,
.xfce4-panel button:hover,
.xfce4-panel button:active,
.xfce4-panel button:checked,
.xfce4-panel button:focus,
.xfce4-panel button:backdrop,
.xfce4-panel .tasklist button,
.xfce4-panel .tasklist button:hover,
.xfce4-panel .tasklist button:active,
.xfce4-panel .tasklist button:checked,
.xfce4-panel .tasklist button:checked:hover,
.xfce4-panel .tasklist button:focus,
.xfce4-panel .tasklist button:backdrop,
.xfce4-panel .tasklist .toggle,
.xfce4-panel .tasklist .toggle:hover,
.xfce4-panel .tasklist .toggle:checked,
.xfce4-panel .tasklist .toggle:checked:hover,
.xfce4-panel .tasklist button:checked,
.xfce4-panel .tasklist button:active {
  background: $panel_css_bg;
  background-image: none;
  background-color: $panel_css_bg;
  border-image: none;
  border-color: $panel_css_border;
EOF
if [ "$css_style" = coordinator ]; then printf '  border-radius: 2px;\n' >> "$css_tmp"; fi
cat >> "$css_tmp" <<EOF
  box-shadow: none;
  color: $panel_css_fg;
  outline-color: transparent;
  text-shadow: none;
  -gtk-icon-shadow: none;
}
.xfce4-panel .tasklist button label,
.xfce4-panel .tasklist .toggle label {
  color: $panel_css_fg;
  text-shadow: none;
}
EOF
if [ "$css_style" = coordinator ]; then
  cat >> "$css_tmp" <<EOF
menubar,
menubar > menuitem,
menubar > menuitem label {
  background: $panel_css_bg;
  background-image: none;
  background-color: $panel_css_bg;
  border-color: transparent;
  box-shadow: none;
  color: $panel_css_fg;
  text-shadow: none;
  -gtk-icon-shadow: none;
}
menubar > menuitem:hover,
menubar > menuitem:hover label,
menubar > menuitem:selected,
menubar > menuitem:selected label {
  background: $panel_css_bg;
  background-image: none;
  background-color: $panel_css_bg;
  color: $panel_css_fg;
}
EOF
fi
printf '/* crabbox desktop theme end */\n' >> "$css_tmp"
mv "$css_tmp" "$css_file"
background_file="$config_dir/crabbox/desktop-background-$mode.svg"
printf '<svg xmlns="http://www.w3.org/2000/svg" width="1920" height="1080"><rect width="100%%" height="100%%" fill="%s"/></svg>\n' "$root_color" >"$background_file"
if [ "$(id -u)" -eq 0 ]; then chown -R "$user" "$config_dir" "$home_dir/.gtkrc-2.0"; fi
[ -n "${DISPLAY:-}" ] || exit 0

xfconf-query -c xsettings -p /Net/ThemeName -n -t string -s "$gtk_theme"
xfconf-query -c xsettings -p /Net/IconThemeName -n -t string -s Adwaita
xfconf-query -c xsettings -p /Gtk/ApplicationPreferDarkTheme -n -t bool -s "$gtk_prefer_dark"
xfconf-query -c xfwm4 -p /general/theme -n -t string -s "$xfwm_theme"
for property in box_move box_resize snap_resist snap_to_border snap_to_windows tile_on_move use_compositing wrap_windows; do
  xfconf-query -c xfwm4 -p "/general/$property" -n -t bool -s false
done
for property in move_opacity resize_opacity; do
  xfconf-query -c xfwm4 -p "/general/$property" -n -t int -s 100
done
xfconf-query -c xfwm4 -p /general/snap_width -n -t int -s 0
xfconf-query -c xfce4-panel -p /panels/dark-mode -n -t bool -s "$gtk_prefer_dark"
set -- $panel_rgba
for panel_id in panel-1 panel-2; do
  xfconf-query -c xfce4-panel -p "/panels/$panel_id/background-style" -n -t int -s 1
  xfconf-query -c xfce4-panel -p "/panels/$panel_id/background-rgba" -n -a -t double -s "$1" -t double -s "$2" -t double -s "$3" -t double -s "$4"
done
for workspace in workspace0 workspace1 workspace2 workspace3; do
  backdrop="/backdrop/screen0/monitorscreen/$workspace"
  xfconf-query -c xfce4-desktop -p "$backdrop/color-style" -n -t int -s 0
  xfconf-query -c xfce4-desktop -p "$backdrop/image-style" -n -t int -s 5
  xfconf-query -c xfce4-desktop -p "$backdrop/last-image" -n -t string -s "$background_file"
done
if command -v gsettings >/dev/null 2>&1; then
  gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme" >/dev/null 2>&1 || true
  gsettings set org.gnome.desktop.interface gtk-theme "$gtk_theme" >/dev/null 2>&1 || true
fi
