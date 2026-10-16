#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT
"""Runs one check (R0): a model, images or tensors from a tar on stdin, one JSON line on stdout.

The script is part of the image and is never changed by the agent: it reads the job (job.json), the
model (model.onnx) and the inputs from the tar, runs every image through the model and answers the raw
predictions. It computes no figure and no verdict - the gateway does that from this answer (P5).

The job (job.json):
  {"mode": "images" | "tensors",
   "images":  [{"id": "<sha>", "file": "images/<sha>.<ext>"}],
   "tensors": [{"name": "input", "shape": [1, 3, 32, 32], "file": "tensors/0.f32"}],
   "pre": {"color": "rgb"|"gray", "size": [h, w]|null, "scale": 0.00392…, "mean": null|[…],
           "std": null|[…], "layout": "nchw"|"nhwc"},
   "max_classes": 1000}

The answer:
  {"model": {"inputs": [{"name","shape","type"}], "outputs": […], "load_error": ""},
   "predictions": [{"id","ok","error","top","probs"}],
   "tensors": [[{"name","shape","data"}]]}
"""

import json
import os
import sys
import tarfile

WORK = "/tmp/check"
MAX_ERROR = 400


def fail(message):
    """Ends the run with a reason the gateway shows: no traceback, one JSON line."""
    print(json.dumps({"model": {"inputs": [], "outputs": [], "load_error": ""},
                      "predictions": [], "tensors": [], "error": str(message)[:MAX_ERROR]}))
    sys.exit(0)


def unpack_stdin():
    """Unpacks the tar from stdin below WORK; refuses names that would leave it."""
    os.makedirs(WORK, exist_ok=True)
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as tf:
        for member in tf:
            if not member.isreg():
                continue
            name = os.path.normpath(member.name)
            if name.startswith("..") or name.startswith("/") or os.path.isabs(name):
                fail("the job archive has a path outside the working directory")
            path = os.path.join(WORK, name)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            src = tf.extractfile(member)
            with open(path, "wb") as f:
                while True:
                    chunk = src.read(1 << 20)
                    if not chunk:
                        break
                    f.write(chunk)


def dynamic(shape):
    """A shape entry as an int; -1 for a dynamic one."""
    return [d if isinstance(d, int) else -1 for d in shape]


def softmax(values):
    import numpy as np
    x = np.asarray(values, dtype=np.float64)
    x = x - x.max()
    e = np.exp(x)
    return e / e.sum()


def is_probability(values, tol=1e-3):
    """True when the values already are a distribution (the model ends with softmax)."""
    if any(v < -tol for v in values):
        return False
    return abs(sum(values) - 1.0) <= tol


def image_input(path, pre, shape, np, Image):
    """One image as the model's input: open, convert, resize, scale, mean/std, batch axis."""
    img = Image.open(path)
    img = img.convert("L" if pre.get("color") == "gray" else "RGB")
    size = pre.get("size") or []
    if len(size) == 2:
        h, w = int(size[0]), int(size[1])
    else:
        h, w = shape_fallback(shape)
    img = img.resize((w, h), Image.BILINEAR)
    x = np.asarray(img, dtype=np.float32) * float(pre.get("scale") or 1.0)
    mean, std = pre.get("mean"), pre.get("std")
    if mean:
        x = x - np.asarray(mean, dtype=np.float32)
    if std:
        x = x / np.asarray(std, dtype=np.float32)
    if x.ndim == 2:  # grayscale without a channel axis
        x = x[:, :, None]
    if pre.get("layout", "nchw") == "nhwc":
        x = x[None, ...]
    else:
        x = np.transpose(x, (2, 0, 1))[None, ...]
    return np.ascontiguousarray(x)


def shape_fallback(shape):
    """The image size out of the model's input shape, if it is fixed."""
    dims = [d for d in dynamic(shape) if d > 0]
    if len(dims) >= 3 and dims[0] <= 4:  # NCHW: the last two are the image
        return dims[-2], dims[-1]
    if len(dims) >= 3:  # NHWC
        return dims[1], dims[2]
    fail("the model has no fixed image size and the job names none")


def main():
    import numpy as np
    import onnxruntime as ort
    from PIL import Image

    # A picture may not be a decompression bomb (R0.14).
    Image.MAX_IMAGE_PIXELS = 50_000_000

    unpack_stdin()
    with open(os.path.join(WORK, "job.json"), "rb") as f:
        job = json.load(f)
    with open(os.path.join(WORK, "model.onnx"), "rb") as f:
        model_bytes = f.read()

    answer = {"model": {"inputs": [], "outputs": [], "load_error": ""}, "predictions": [], "tensors": []}
    opts = ort.SessionOptions()
    opts.intra_op_num_threads = 1
    opts.inter_op_num_threads = 1
    try:
        sess = ort.InferenceSession(model_bytes, sess_options=opts, providers=["CPUExecutionProvider"])
    except Exception as exc:  # a model that does not load: the gateway reports it as the reason
        answer["model"]["load_error"] = str(exc)[:MAX_ERROR]
        print(json.dumps(answer))
        return
    inputs = sess.get_inputs()
    outputs = sess.get_outputs()
    answer["model"]["inputs"] = [{"name": i.name, "shape": dynamic(i.shape), "type": i.type} for i in inputs]
    answer["model"]["outputs"] = [{"name": o.name, "shape": dynamic(o.shape), "type": o.type} for o in outputs]

    if not inputs:
        fail("the model has no input")
    max_classes = int(job.get("max_classes") or 1000)
    pre = job.get("pre") or {}

    if job.get("mode") == "tensors":
        answer["tensors"] = run_tensors(sess, job, inputs, np)
        print(json.dumps(answer))
        return

    for entry in job.get("images", []):
        pred = {"id": entry.get("id", ""), "ok": False, "error": "", "top": -1, "probs": []}
        try:
            path = os.path.join(WORK, os.path.normpath(entry.get("file", "")))
            x = image_input(path, pre, inputs[0].shape, np, Image)
            raw = sess.run(None, {inputs[0].name: x})[0]
            values = np.asarray(raw, dtype=np.float64).reshape(-1)
            if values.size == 0:
                pred["error"] = "the model answered nothing"
            elif values.size > max_classes:
                pred["error"] = "too many outputs for a classification check"
            elif not np.all(np.isfinite(values)):
                pred["error"] = "non-finite output"
            else:
                probs = values.tolist() if is_probability(values.tolist()) else softmax(values).tolist()
                probs = [float(v) for v in probs]
                pred.update(ok=True, top=int(np.argmax(np.asarray(probs))), probs=probs)
        except Exception as exc:
            pred["error"] = str(exc)[:MAX_ERROR]
        answer["predictions"].append(pred)
    print(json.dumps(answer))


def run_tensors(sess, job, inputs, np):
    """Runs the named input tensors and answers the output tensors (R3: test requests)."""
    feed = {}
    for entry in job.get("tensors", []):
        path = os.path.join(WORK, os.path.normpath(entry.get("file", "")))
        with open(path, "rb") as f:
            blob = f.read()
        shape = [int(d) for d in entry.get("shape") or []]
        values = np.frombuffer(blob, dtype="<f4")
        if shape:
            values = values.reshape(shape)
        feed[entry.get("name", "")] = values
    if not feed:
        return []
    out = sess.run(None, feed)
    return [[{"name": o.name, "shape": dynamic(o.shape), "data": [float(v) for v in np.asarray(r).reshape(-1)]}
             for o, r in zip(sess.get_outputs(), out)]]


if __name__ == "__main__":
    main()
