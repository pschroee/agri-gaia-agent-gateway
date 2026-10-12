---
name: training-advice
description: Advice before creating a platform training - use it before every agw-platform create-training or platform_create_training. Rules - check the task category against the annotation kind of the dataset and say so plainly when they do not match (get_dataset has only the classes, not the kind; take the kind from the annotation file you uploaded or read, CVAT tag means classification, box means object detection, polygon or mask means segmentation, otherwise ask the user); start from the default values of train_options and keep every value inside the schema; justify every change against the defaults in one sentence; always include export_config for ONNX with input_shapes matching img-size; give the runtime only as a range marked as an estimate, based on the measured runs of this instance in this skill (4 vCPU, no GPU); show the proposal first and create the training only after the user agrees.
---

# Training advice

Before you create a training on the platform, prepare a proposal, show it and wait for the user's consent.
The approval of `create-training` comes on top; it does not replace the consent.

## 1. Read the dataset and the templates

- `agw-platform dataset <id>`: name, `annotation_labels` (classes), `filecount`. The platform's metadata does
  **not** say how the data is annotated.
- The annotation kind, in this order: the annotation file you uploaded or can read in this chat (CVAT XML:
  `<tag>` per image = classification, `<box>` = object detection, `<polygon>`, `<polyline>` or `<mask>` =
  segmentation); what the user said; otherwise ask the user and say that the platform does not record it.
  Never infer it from the dataset's name alone.
- `agw-platform train-options` (providers), `agw-platform train-options <provider>` (architectures with
  `category`), `agw-platform train-options <provider> <architecture>` (JSON Schema and default values of the
  `train_config`).

## 2. Check the category

The category of the architecture must fit the annotation kind: Classification needs one tag per image, Object
Detection needs boxes, Instance Segmentation needs polygons or masks. On this instance there are Torchvision
EfficientNet (Classification), Torchvision Mask R-CNN (Instance Segmentation) and Ultralytics YOLOv5 and YOLOv11
(Object Detection); check with `train-options`, the list can change. If the requested category does not fit,
say so at the start of your reply, explain in one sentence why, and propose an architecture whose category
fits. Never create a training whose category does not fit the annotations.

## 3. Propose values

- Start from the default values of `train-options <provider> <architecture>`; change only what the task or
  this instance requires.
- Every value stays inside the schema (`minimum`, `maximum`, `enum`, type). Name the allowed range next to
  every value you change.
- Show the changes as a table with the columns parameter, default, proposed, reason (one sentence each).
  Values you keep need no row.
- This instance has no GPU (4 vCPU, 31 GB RAM): few epochs, a small `img-size`, a small `batch-size`. The
  defaults (for example 50 epochs and 300 px for EfficientNet) are meant for a GPU.

## 4. Always send export_config

Without `export_config` the platform saves `model.pt` without tensor metadata, and no inference container can
be built from the model. Always include it, with `input_shapes` matching `img-size`
(`[[1, 3, <img-size>, <img-size>]]`):

```json
{"model_type": "pytorch", "input_names": [], "output_names": [], "input_shapes": [[1, 3, 64, 64]],
 "input_types": ["float32"], "verbose": false, "dynamo": false, "opset_version": 18}
```

Keep `dynamo` false: the torch.export-based exporter cannot trace torchvision detection models.

## 5. Estimate the runtime

Give the runtime only as a range and call it an estimate. Base it on these measured runs of this instance
(2026-09-09, MNIST-mini with 30 images of 28 x 28 px in two classes, Torchvision EfficientNet
`efficientnet_v2_s`, img-size 64, batch-size 8, 4 vCPU, no GPU):

| Step | Measured |
|---|---|
| Building the training image (`create-training` until its task is completed) | about 4.5 min |
| Training, 3 epochs, without ONNX export | 12 s |
| Training with ONNX export | 3 min 45 s (the export takes most of it) |

Scale the training part with the number of images and epochs, and say that larger images and detection models
take longer; do not invent factors that were not measured. If the user's data is far from these runs
(thousands of images, Mask R-CNN), say that the estimate is rough.

## 6. Wait for consent

End the proposal with a question. Create the training only after the user agrees, with exactly the values
shown (or the ones the user changed). The approval of `create-training` follows anyway.
