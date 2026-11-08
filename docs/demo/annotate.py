#!/usr/bin/env python3
"""Turn a VHS recording into a GIF that zooms in on parts of the screen and
explains them in a caption bar.

    annotate.py recording.mp4 spec.json out.gif

The spec lists shots in recording time:

    {"boxes": {"node": [x0, y0, x1, y1]},          # in recording pixels
     "shots": [{"t": 0, "caption": "Press `r` to run"},
               {"t": 4.5, "box": "node", "caption": "..."}]}

A shot holds until the next one; with no box it shows the whole screen.
Boxes are widened to the output's shape, and the camera eases between them.
Text in `backticks` is drawn as a key.

Needs ffmpeg and Pillow.
"""
import json
import math
import os
import subprocess
import sys
import tempfile

from PIL import Image, ImageChops, ImageDraw, ImageFont

OUT_W = 1200
FPS = 15
EASE = 0.4  # seconds the camera takes to move between shots
MOVE_STEPS = 3  # frames in a move; every frame of one is a new full image in the GIF
BAR_H = 72
BAR_BG = (17, 17, 27)
TEXT = (205, 214, 244)
KEY_BG = (69, 71, 90)
KEY_FG = (249, 226, 175)
FONTS = [
    "/System/Library/Fonts/SFNS.ttf",
    "/System/Library/Fonts/Helvetica.ttc",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
]
MONO = [
    "/System/Library/Fonts/SFNSMono.ttf",
    "/System/Library/Fonts/Menlo.ttc",
    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
]


def font(paths, size):
    for p in paths:
        if os.path.exists(p):
            return ImageFont.truetype(p, size)
    return ImageFont.load_default(size)


def fit(box, W, H):
    """Widen box to the frame's aspect ratio, keeping it inside the frame."""
    x0, y0, x1, y1 = box
    w, h = x1 - x0, y1 - y0
    if w / h < W / H:
        w = h * W / H
    else:
        h = w * H / W
    w, h = min(w, W), min(h, H)
    cx, cy = (x0 + x1) / 2, (y0 + y1) / 2
    x0 = min(max(cx - w / 2, 0), W - w)
    y0 = min(max(cy - h / 2, 0), H - h)
    return (x0, y0, w, h)


def ease(p):
    return p * p * (3 - 2 * p)


def camera(shots, t):
    """The crop box at time t, easing from the previous shot's box."""
    i = max(k for k, s in enumerate(shots) if s["t"] <= t) if t >= shots[0]["t"] else 0
    cur = shots[i]["rect"]
    if i == 0:
        return cur
    p = (t - shots[i]["t"]) / EASE
    if p >= 1:
        return cur
    prev = shots[i - 1]["rect"]
    p = ease(math.ceil(max(p, 0) * MOVE_STEPS) / MOVE_STEPS)
    return tuple(a + (b - a) * p for a, b in zip(prev, cur))


def caption_at(shots, t):
    i = max((k for k, s in enumerate(shots) if s["t"] <= t), default=0)
    return shots[i].get("caption", "")


def draw_caption(img, text, sans, mono):
    d = ImageDraw.Draw(img)
    top = img.height - BAR_H
    d.rectangle([0, top, img.width, img.height], fill=BAR_BG)
    parts = text.split("`")  # odd parts are keys
    widths = []
    for k, part in enumerate(parts):
        f = mono if k % 2 else sans
        widths.append(d.textlength(part, font=f) + (16 if k % 2 else 0))
    x = (img.width - sum(widths)) / 2
    cy = top + BAR_H / 2
    for k, part in enumerate(parts):
        if k % 2:
            d.rounded_rectangle([x, cy - 17, x + widths[k], cy + 17], radius=6, fill=KEY_BG)
            d.text((x + 8, cy), part, font=mono, fill=KEY_FG, anchor="lm")
        else:
            d.text((x, cy), part, font=sans, fill=TEXT, anchor="lm")
        x += widths[k]


def changed(a, b):
    """Whether two raw frames differ by more than video-compression noise."""
    diff = ImageChops.difference(a, b).convert("L").point(lambda v: 255 if v > 48 else 0)
    return diff.getbbox() is not None


def main(video, spec_path, out):
    spec = json.load(open(spec_path))
    with tempfile.TemporaryDirectory() as tmp:
        raw, done = os.path.join(tmp, "raw"), os.path.join(tmp, "done")
        os.makedirs(raw)
        os.makedirs(done)
        subprocess.run(["ffmpeg", "-loglevel", "error", "-i", video, "-vf", f"fps={FPS}",
                        os.path.join(raw, "%05d.png")], check=True)
        frames = sorted(os.listdir(raw))
        W, H = Image.open(os.path.join(raw, frames[0])).size
        out_h = round(OUT_W * H / W)
        shots = spec["shots"]
        for s in shots:
            box = spec.get("boxes", {}).get(s.get("box"), [0, 0, W, H])
            s["rect"] = fit(box, W, H)
        sans, mono = font(FONTS, 26), font(MONO, 22)
        last = None  # (raw frame, camera, caption, output) of the previous frame
        for n, name in enumerate(frames):
            t = n / FPS
            x, y, w, h = camera(shots, t)
            text = caption_at(shots, t)
            src = Image.open(os.path.join(raw, name)).convert("RGB")
            if last and last[1] == (x, y, w, h) and last[2] == text and not changed(last[0], src):
                # keep the previous frame byte for byte, so the GIF doesn't
                # store compression noise as changes
                last[3].save(os.path.join(done, name))
                continue
            img = src.crop((round(x), round(y), round(x + w), round(y + h)))
            img = img.resize((OUT_W, out_h), Image.LANCZOS)
            canvas = Image.new("RGB", (OUT_W, out_h + BAR_H), BAR_BG)
            canvas.paste(img, (0, 0))
            draw_caption(canvas, text, sans, mono)
            canvas.save(os.path.join(done, name))
            last = (src, (x, y, w, h), text, canvas)
        pattern = os.path.join(done, "%05d.png")
        subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-framerate", str(FPS), "-i", pattern,
                        "-vf", "split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle",
                        "-loop", "0", out], check=True)


if __name__ == "__main__":
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    main(*sys.argv[1:])
