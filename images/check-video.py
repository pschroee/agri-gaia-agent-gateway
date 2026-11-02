#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT
"""Prove that the execution image can write, open and read MP4 clips with OpenCV.

usage (inside the built exec image): python3 /path/to/check-video.py [--version 4.14.0]

Writes a clip of 10 frames of 64x48 (mp4v) into a temporary directory, opens it again, checks the frame count and the
size, and reads frame 5 by position. Garage footage is never needed. With --version it also compares cv2.__version__
with the library version (wheel 4.14.0.94 reports 4.14.0). Exit 0 on success, 1 on a failed check. Run it after `docker build` of the exec stage:
  docker run --rm -v "$PWD/images:/chk:ro" <exec image> python3 /chk/check-video.py
"""
import argparse
import os
import sys
import tempfile

FRAMES, WIDTH, HEIGHT = 10, 64, 48


def fail(msg):
    print("FAIL\tvideo\t%s" % msg)
    sys.exit(1)


def main():
    ap = argparse.ArgumentParser(description="write and read a small MP4 clip with cv2")
    ap.add_argument("--version", help="expected cv2.__version__")
    args = ap.parse_args()
    try:
        import cv2
        import numpy as np
    except ImportError as e:
        fail("import: %s" % e)
    if args.version and cv2.__version__ != args.version:
        fail("cv2 %s, expected %s" % (cv2.__version__, args.version))
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "check.mp4")
        w = cv2.VideoWriter(path, cv2.VideoWriter_fourcc(*"mp4v"), 30.0, (WIDTH, HEIGHT))
        if not w.isOpened():
            fail("VideoWriter did not open")
        for i in range(FRAMES):
            frame = np.full((HEIGHT, WIDTH, 3), i * 20, dtype=np.uint8)
            w.write(frame)
        w.release()
        cap = cv2.VideoCapture(path)
        if not cap.isOpened():
            fail("VideoCapture did not open the clip")
        n = int(cap.get(cv2.CAP_PROP_FRAME_COUNT))
        if n != FRAMES:
            fail("frame count %d, want %d" % (n, FRAMES))
        if (int(cap.get(cv2.CAP_PROP_FRAME_WIDTH)), int(cap.get(cv2.CAP_PROP_FRAME_HEIGHT))) != (WIDTH, HEIGHT):
            fail("size differs from %dx%d" % (WIDTH, HEIGHT))
        cap.set(cv2.CAP_PROP_POS_FRAMES, 5)
        ok, frame = cap.read()
        cap.release()
        if not ok or frame.shape != (HEIGHT, WIDTH, 3):
            fail("frame 5 could not be read")
    print("OK\tvideo\topencv-python-headless@%s\t%d frames of %dx%d" % (cv2.__version__, FRAMES, WIDTH, HEIGHT))


if __name__ == "__main__":
    main()
