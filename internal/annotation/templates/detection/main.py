# SPDX-FileCopyrightText: 2026 Philipp Schröer
#
# SPDX-License-Identifier: MIT
#
# Pre-annotation of a CVAT task from an object detector in ONNX (YOLOv5/v8/v11 layout). This is the
# fixed template of the agent gateway (template version 1, see funczip.go): the agent chooses the
# model, the class names and the input size, never code. CVAT calls the handler once per image with
# {"image": <base64>, "threshold": <0..1>} and expects a list of boxes.

import base64
import io
import json

import numpy as np
import onnxruntime
from PIL import Image

session = None
labels = []
input_size = 640

# The padding colour and the NMS threshold of the usual YOLO inference.
FILL = 114
IOU_THRESHOLD = 0.45
MAX_BOXES = 300


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


def _letterbox(image):
    """The image in a square canvas: scaled by its longer side, padded with FILL."""
    width, height = image.size
    scale = min(input_size / width, input_size / height)
    new_w, new_h = max(1, int(round(width * scale))), max(1, int(round(height * scale)))
    pad_x, pad_y = (input_size - new_w) // 2, (input_size - new_h) // 2
    canvas = Image.new("RGB", (input_size, input_size), (FILL, FILL, FILL))
    canvas.paste(image.convert("RGB").resize((new_w, new_h), Image.BILINEAR), (pad_x, pad_y))
    arr = np.asarray(canvas, dtype=np.float32) / 255.0
    return arr.transpose(2, 0, 1)[None, ...], scale, pad_x, pad_y


def _candidates(out, threshold):
    """Boxes (centre, width, height) of one image as the model's layout has them."""
    a = np.asarray(out, dtype=np.float32)
    if a.ndim == 3:
        a = a[0]  # drop the batch
    n = len(labels)
    if a.ndim != 2:
        raise RuntimeError("unsupported output shape %r" % (a.shape,))
    if a.shape[0] == 4 + n:  # YOLOv8/v11: 4 box values and one score per class
        a = a.T
        boxes, scores = a[:, :4], a[:, 4:4 + n]
    elif a.shape[1] == 5 + n:  # YOLOv5: xywh, objectness and one score per class
        boxes, scores = a[:, :4], a[:, 4:5] * a[:, 5:5 + n]
    else:
        raise RuntimeError("unsupported output shape %r for %d classes" % (a.shape, n))
    classes = scores.argmax(axis=1)
    confidence = scores[np.arange(scores.shape[0]), classes]
    keep = confidence >= threshold
    return boxes[keep], classes[keep], confidence[keep]


def _iou(box, boxes):
    """The intersection over union of one box and a list of boxes, all in centre form."""
    left = np.maximum(box[0] - box[2] / 2, boxes[:, 0] - boxes[:, 2] / 2)
    top = np.maximum(box[1] - box[3] / 2, boxes[:, 1] - boxes[:, 3] / 2)
    right = np.minimum(box[0] + box[2] / 2, boxes[:, 0] + boxes[:, 2] / 2)
    bottom = np.minimum(box[1] + box[3] / 2, boxes[:, 1] + boxes[:, 3] / 2)
    inter = np.clip(right - left, 0, None) * np.clip(bottom - top, 0, None)
    union = box[2] * box[3] + boxes[:, 2] * boxes[:, 3] - inter
    return np.where(union > 0, inter / np.where(union > 0, union, 1), 0)


def _nms(boxes, classes, confidence):
    """One detection per object: the usual greedy suppression, per class."""
    keep = []
    for cls in np.unique(classes):
        order = np.where(classes == cls)[0]
        order = order[np.argsort(-confidence[order])]
        while order.size:
            best = order[0]
            keep.append(int(best))
            if order.size == 1 or len(keep) >= MAX_BOXES:
                break
            rest = order[1:]
            order = rest[_iou(boxes[best], boxes[rest]) <= IOU_THRESHOLD]
    return keep[:MAX_BOXES]


def handler(context, event):
    """One image of the run: the boxes the model finds above the threshold, in image coordinates."""
    data = event.body
    threshold = float(data.get("threshold", 0.5))
    image = Image.open(io.BytesIO(base64.b64decode(data["image"])))
    tensor, scale, pad_x, pad_y = _letterbox(image)
    out = session.run(None, {session.get_inputs()[0].name: tensor})[0]
    boxes, classes, confidence = _candidates(out, threshold)
    result = []
    if boxes.size:
        width, height = image.size
        for i in _nms(boxes, classes, confidence):
            box = boxes[i]
            # Centre form back into the coordinates of the image, the padding removed.
            x1 = (box[0] - box[2] / 2 - pad_x) / scale
            y1 = (box[1] - box[3] / 2 - pad_y) / scale
            x2 = (box[0] + box[2] / 2 - pad_x) / scale
            y2 = (box[1] + box[3] / 2 - pad_y) / scale
            # Keep the box inside the image; one without area is no annotation.
            x1, y1 = max(0.0, float(x1)), max(0.0, float(y1))
            x2, y2 = min(float(width), float(x2)), min(float(height), float(y2))
            if x2 <= x1 or y2 <= y1 or int(classes[i]) >= len(labels):
                continue
            result.append({
                "label": labels[int(classes[i])],
                "type": "rectangle",
                "points": [round(x1, 1), round(y1, 1), round(x2, 1), round(y2, 1)],
                "confidence": "%.4f" % float(confidence[i]),
                "raw_label": labels[int(classes[i])],
            })
    return context.Response(body=json.dumps(result), headers={}, status_code=200, content_type="application/json")
