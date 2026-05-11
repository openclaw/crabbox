# media

`crabbox media` creates lightweight review artifacts from recorded desktop
videos. It runs locally and does not need a lease.

## Preview

`crabbox media preview` converts an MP4 or other ffmpeg-readable video into a
small animated GIF that GitHub can render inline in comments and pull request
bodies.

```sh
crabbox media preview \
  --input desktop.mp4 \
  --output desktop-preview.gif \
  --trimmed-video-output desktop-change.mp4
```

By default the preview is motion-focused:

- ffmpeg `freezedetect` finds leading and trailing static regions.
- Crabbox keeps a little padding around the first and last moving frame.
- The GIF is palette-optimized at 24 fps and 1000 px wide with Floyd-Steinberg dithering.
- When `gifsicle` is on `PATH`, Crabbox runs `gifsicle -O3 --lossy=65 --gamma=1.2` after ffmpeg to reduce file size without dropping the higher-quality palette.
- `--trimmed-video-output` writes an MP4 clip using the same motion window.

If no motion is detected, Crabbox keeps the full source video instead of
returning an empty preview.

Useful flags:

```text
--input <path>
--output <path>
--trimmed-video-output <path>
--width <px>              default 1000
--fps <n>                 default 24
--trim-static             default true
--no-trim-static
--trim-padding <duration> default 750ms
--freeze-duration <dur>   default 500ms
--freeze-noise <level>    default -50dB
--min-duration <duration> default 1500ms
--gifsicle auto|off|required
--gifsicle-lossy <n>      default 65
--gifsicle-gamma <n>      default 1.2
--json
```

`ffmpeg` and `ffprobe` must be on `PATH`. `gifsicle` is optional by default;
use `--gifsicle required` when you want the command to fail instead of skipping
that optimization pass.
