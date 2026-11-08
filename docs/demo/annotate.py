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
"mark" names a box (or a list of boxes) to point at: once the camera has
settled, everything else is dimmed and the box gets an outline. Text in
`backticks` is drawn as a key, and a "\n" in a caption starts a second line
("bar_lines": 2 in the spec makes room for it).

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
LINE_H = 40
BAR_PAD = 16
MARK = (249, 226, 175)
DIM = 0.35  # how bright the unmarked part stays
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


def shot_at(shots, t):
    return max((k for k, s in enumerate(shots) if s["t"] <= t), default=0)


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


def settled(shots, t):
    i = shot_at(shots, t)
    return i == 0 or t - shots[i]["t"] >= EASE


def draw_marks(img, marks, cam, scale):
    """Dim everything but the marked boxes, and outline them."""
    x, y = cam[0], cam[1]
    rects = [((m[0] - x) * scale, (m[1] - y) * scale, (m[2] - x) * scale, (m[3] - y) * scale) for m in marks]
    mask = Image.new("L", img.size, 0)
    md = ImageDraw.Draw(mask)
    for r in rects:
        md.rounded_rectangle(r, radius=10, fill=255)
    dim = img.point(lambda v: int(v * DIM))
    out = Image.composite(img, dim, mask)
    d = ImageDraw.Draw(out)
    for r in rects:
        d.rounded_rectangle(r, radius=10, outline=MARK, width=3)
    return out


def draw_caption(img, text, sans, mono, bar_h):
    d = ImageDraw.Draw(img)
    top = img.height - bar_h
    d.rectangle([0, top, img.width, img.height], fill=BAR_BG)
    lines = text.split("\n")
    first = top + bar_h / 2 - (len(lines) - 1) * LINE_H / 2
    for k, line in enumerate(lines):
        draw_line(d, img.width, first + k * LINE_H, line, sans, mono)


def draw_line(d, width, cy, text, sans, mono):
    parts = text.split("`")  # odd parts are keys
    widths = []
    for k, part in enumerate(parts):
        f = mono if k % 2 else sans
        widths.append(d.textlength(part, font=f) + (16 if k % 2 else 0))
    x = (width - sum(widths)) / 2
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
        shots, boxes = spec["shots"], spec.get("boxes", {})
        bar_h = spec.get("bar_lines", 1) * LINE_H + 2 * BAR_PAD
        for s in shots:
            s["rect"] = fit(boxes.get(s.get("box"), [0, 0, W, H]), W, H)
            mark = s.get("mark", [])
            s["marks"] = [boxes[m] for m in ([mark] if isinstance(mark, str) else mark)]
        sans, mono = font(FONTS, 26), font(MONO, 22)
        last = None  # (raw frame, camera, caption, output) of the previous frame
        for n, name in enumerate(frames):
            t = n / FPS
            x, y, w, h = camera(shots, t)
            shot = shots[shot_at(shots, t)]
            text = shot.get("caption", "")
            marks = shot["marks"] if settled(shots, t) else []
            src = Image.open(os.path.join(raw, name)).convert("RGB")
            if last and last[1] == (x, y, w, h) and last[2] == (text, marks) and not changed(last[0], src):
                # keep the previous frame byte for byte, so the GIF doesn't
                # store compression noise as changes
                last[3].save(os.path.join(done, name))
                continue
            img = src.crop((round(x), round(y), round(x + w), round(y + h)))
            img = img.resize((OUT_W, out_h), Image.LANCZOS)
            if marks:
                img = draw_marks(img, marks, (x, y), OUT_W / w)
            canvas = Image.new("RGB", (OUT_W, out_h + bar_h), BAR_BG)
            canvas.paste(img, (0, 0))
            draw_caption(canvas, text, sans, mono, bar_h)
            canvas.save(os.path.join(done, name))
            last = (src, (x, y, w, h), (text, marks), canvas)
        pattern = os.path.join(done, "%05d.png")
        subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-framerate", str(FPS), "-i", pattern,
                        "-vf", "split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle",
                        "-loop", "0", out], check=True)


if __name__ == "__main__":
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    main(*sys.argv[1:])
