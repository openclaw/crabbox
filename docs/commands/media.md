# media

`crabbox media` turns a recorded desktop video into lightweight review
artifacts. It runs entirely on your machine, needs no lease or broker, and
depends only on `ffmpeg` and `ffprobe` being on your `PATH`.

The command pairs naturally with [`desktop record`](desktop.md) and
[`artifacts video`](artifacts.md), which produce the MP4 source you feed in
here.

## `media preview`

`crabbox media preview` converts an MP4 (or any ffmpeg-readable video) into a
small animated GIF that GitHub renders inline in issues, pull request bodies,
and comments. It can optionally also emit a trimmed MP4 clip covering the same
window.

```sh
crabbox media preview \
  --input desktop.mp4 \
  --output desktop-preview.gif \
  --trimmed-video-output desktop-change.mp4
```

Both `--input` and `--output` are required.

### Motion trimming

By default the preview is motion-focused so the GIF shows the change, not the
idle desktop around it:

- ffmpeg `freezedetect` locates leading and trailing static regions.
- Crabbox keeps `--trim-padding` (default `750ms`) of context before the first
  and after the last moving frame.
- The window is widened to at least `--min-duration` (default `1500ms`) when the
  detected motion is shorter.
- If no motion is detected at all, Crabbox keeps the full source rather than
  emitting an empty preview.

Disable trimming and render the whole video with `--no-trim-static`.

### Encoding

The GIF is palette-optimized for quality: ffmpeg generates a diff-mode palette,
then renders at `--fps` (default `24`) and `--width` (default `1000` px) with
Lanczos scaling and Floyd–Steinberg dithering.

When `gifsicle` is on `PATH`, Crabbox runs a second optimization pass
(`gifsicle -O3 --lossy=<n> --gamma=<n>`) to shrink the file while preserving the
higher-quality palette. Control this with `--gifsicle`:

- `auto` (default) — optimize if `gifsicle` is available, otherwise skip.
- `off` — never run gifsicle.
- `required` — fail the command if `gifsicle` is missing.

### Flags

```text
--input <path>                  source video (required)
--output <path>                 GIF preview output (required)
--trimmed-video-output <path>   also write an MP4 clip of the same window
--width <px>                    preview width (default 1000)
--fps <n>                       preview frames per second (default 24)
--trim-static                   trim static edges (default true)
--no-trim-static                disable static-edge trimming
--trim-padding <duration>       context kept around motion (default 750ms)
--freeze-duration <duration>    min still duration for freezedetect (default 500ms)
--freeze-noise <level>          freezedetect noise threshold (default -50dB)
--min-duration <duration>       min preview duration after trimming (default 1500ms)
--gifsicle auto|off|required    gifsicle optimization mode (default auto)
--gifsicle-lossy <n>            gifsicle lossy compression value (default 65)
--gifsicle-gamma <n>            gifsicle gamma value (default 1.2)
--json                          print machine-readable result metadata
```

### Output

By default `media preview` prints the paths it wrote (and the trimmed window
when static edges were removed):

```text
wrote desktop-preview.gif from 1.250s..4.500s
wrote desktop-change.mp4
```

With `--json` it prints structured metadata instead, including the source and
preview durations, the detected motion window, the number of freeze intervals
found, and whether the gifsicle pass ran:

```sh
crabbox media preview --input desktop.mp4 --output desktop-preview.gif --json
```

## Requirements

`ffmpeg` and `ffprobe` must be on `PATH`. `gifsicle` is optional unless you pass
`--gifsicle required`.
