# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT
#
# Pre-annotation of a CVAT task from a classification model in ONNX. This is the fixed template of the
# agent gateway (template version 1, see funczip.go): the agent chooses the model, the class names and
# the input size, never code. CVAT calls the handler once per image with {"image": <base64>,
# "threshold": <0..1>} and expects a list of annotations.

import base64
import io
import json

import numpy as np
import onnxruntime
from PIL import Image

session = None
labels = []
input_size = 224


def init_context(context):
    """Load the classes, the model and the session once, when the container starts."""
    global session, labels, input_size
    with open("config.json", encoding="utf-8") as fh:
        config = json.load(fh)
    labels = list(config["labels"])
    input_size = int(config["input_size"])
    # The model file comes from bytes: an ONNX file with external data would be followed on the file
    # system otherwise (the gateway loads it the same way before it packs it).
    with open("model.onnx", "rb") as fh:
        model = fh.read()
    options = onnxruntime.SessionOptions()
    options.intra_op_num_threads = 2  # the CPU of the instance is shared with the other services
    session = onnxruntime.InferenceSession(model, sess_options=options)
    context.logger.info("pre-annotation function ready: %d classes, input %dx%d", len(labels), input_size, input_size)


def _tensor(image):
    """The image as the model expects it: RGB, square, 0..1, NCHW."""
    img = image.convert("RGB").resize((input_size, input_size), Image.BILINEAR)
    arr = np.asarray(img, dtype=np.float32) / 255.0
    return arr.transpose(2, 0, 1)[None, ...]


def _softmax(x):
    e = np.exp(x - x.max())
    return e / e.sum()


def handler(context, event):
    """One image of the run. Returns a tag when the best class reaches the threshold, else nothing."""
    data = event.body
    threshold = float(data.get("threshold", 0.5))
    image = Image.open(io.BytesIO(base64.b64decode(data["image"])))
    out = session.run(None, {session.get_inputs()[0].name: _tensor(image)})[0]
    probs = _softmax(np.asarray(out, dtype=np.float32).reshape(-1))
    index = int(probs.argmax())
    confidence = float(probs[index])
    result = []
    # The model may have more outputs than the function has class names; without a name there is no
    # annotation CVAT could write.
    if index < len(labels) and confidence >= threshold:
        result.append({
            "label": labels[index],
            "type": "tag",
            "confidence": "%.4f" % confidence,
            "raw_label": labels[index],
            # raw_label and scores are add-ons of the gateway: CVAT ignores unknown keys in a batch
            # run, the single call of a function returns them (the check of the gateway reads them).
            "scores": [round(float(p), 4) for p in probs],
        })
    return context.Response(body=json.dumps(result), headers={}, status_code=200, content_type="application/json")
