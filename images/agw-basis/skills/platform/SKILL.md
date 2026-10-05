---
name: platform
description: Work with the Agri-Gaia platform (datasets, models, training, background tasks, edge devices, container images) via agw-platform. Use when the user asks about data or models on the platform or wants to create, start or follow a training there.
---

# Agri-Gaia platform

Agri-Gaia is an AI platform for the agricultural and food industry: datasets (mostly images with
annotations), training from installed templates, model management, inference containers and
edge devices. You reach it **only** via `agw-platform`; the orchestrator logs in there, there is no
token in the sandbox, and `curl` against the platform gets you nowhere.

```bash
agw-platform --help                 # all commands
agw-platform datasets               # datasets (all users)
agw-platform dataset 3              # one dataset
agw-platform models                 # models
agw-platform trainings              # training containers with status and score
agw-platform tasks --limit 20       # background tasks of the platform
```

The output is `HTTP <code>` followed by JSON. Exit code 0 ok, 3 rejected by the user, 1 error
(including an error response from the platform). Long responses are truncated; then ask again with
`--skip`/`--limit` or filter with `jq` (`agw-platform datasets | tail -n +2 | jq '.[].name'`).

## Creating and following a training

1. Explore the templates: `agw-platform train-options` (providers), `agw-platform train-options Torchvision`
   (architectures with `category`), `agw-platform train-options Torchvision EfficientNet` (schema and
   default values of the `train_config`).
2. Write the `train_config` as a file, starting from the default values, changing only what the task
   requires (this instance has **no GPU**: few epochs, small images).
3. Create: `agw-platform create-training Torchvision EfficientNet Classification <dataset_id> @train_config.json`
   — the platform builds the training image in the background. The response names `Location: /tasks/<id>`.
4. Follow the build: `agw-platform task <id>` until `status` is `completed` or `failed` (not in a tight
   loop; wait at least 20 seconds between queries). Then `agw-platform trainings`:
   the new entry has the `id` of the training container.
5. Start: `agw-platform start-training <train_container_id>`, follow with
   `agw-platform training-status <id>` and `agw-platform training-logs <id> --tail 50`.

## Uploading files

You upload files from the sandbox with two commands; the orchestrator reads them itself from the
sandbox, and before approving the user sees name, size and SHA-256 of every file:

```bash
# Dataset: name, description, then the files (globs work); classes as a repeated option,
# a CVAT annotation (annotations.xml) as --annotation-file
agw-platform upload-dataset piglet-images "Piglets, barn 3, October" images/*.png \
  --annotation-labels 0 --annotation-labels 1 --annotation-file annotations.xml
# Model: name, description, format (onnx, pytorch, tensorflow, tensorrt), file
agw-platform upload-model mnist-small "MNIST, two classes" onnx model.onnx
```

At most 2 000 files and 512 MB per call, each file at most as large as an artifact.
Keywords (`--keywords`) are **AGROVOC URIs** for both, not free words; search with
`agw-platform request GET /agrovoc/keywords --query keyword=pig`. The orchestrator never sets "Classification Dataset" (otherwise a bug in the platform discards the
classes); the classes go via `--annotation-labels`.

## Delegated rights

The user can delegate only certain rights to you for a chat (which actions on which
objects). `agw-platform rights` shows them, together with the objects you created in this chat.
Check before writing calls. Anything outside them is refused by the authorization service
(exit code 4, "denied by the authorization service"); do not try another way then,
but tell the user which right is missing.

## What needs approval

Reading works directly. **Every writing call** (`create-training`, `start-training`, `upload-dataset`,
`upload-model`, `request` with
POST, PUT, PATCH or DELETE, plus some GETs that create something on the platform, e.g.
`/train/containers/<id>/model`) waits until the user agrees in the UI. Call such commands
with a generous timeout (at least 900 seconds) and not in the background. With exit code 3
do not ask again unless the user wants it.

## Everything else

`agw-platform request <METHOD> <path> [--query k=v …] [--body JSON|@file|-]` calls any path of the
REST API, only with a JSON body (no file uploads). Which paths exist is shown by
`agw-platform api-paths` (one line per operation; `agw-platform api-paths /train` only for training).
Paths under `/users`, `/urls`, `/service` and `/network` are blocked. Values of passwords, keys
and tokens appear as `[redacted by the orchestrator]`, downloads only as size and type.

So far the platform only distinguishes logged in or not: lists show the objects of **all**
users. Do not delete or change anything the user has not explicitly asked for.
