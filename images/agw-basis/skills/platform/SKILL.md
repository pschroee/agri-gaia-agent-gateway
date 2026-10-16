---
name: platform
description: Work with the Agri-Gaia platform (datasets, models, training, background tasks, edge devices, container images) via agw-platform. Use when the user asks about data or models on the platform or wants to create, start or follow a training there, and to deploy inference images to edge devices when capabilities shows an edge device online, otherwise build the image and tell the user that no device is available.
---

# Agri-Gaia platform

Check with `agw-platform capabilities` what this setup offers before you plan work that needs
notebooks, edge devices, pre-annotation or images.

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

## Taking over a trained model

Once a training is `exited`, `agw-platform model-from-training <train_container_id>` creates a platform model
from it (ONNX when the training was created with an export). It needs approval. **Every call creates another
model**, so do it once per training and only when the user wants the model. Without `export_config` in
`create-training` the model has no tensor metadata and no inference container can be built from it; the example
is in `agw-platform create-training --help`.

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

## Renaming and model metadata

- `agw-platform rename-dataset <dataset_id> <name>` renames a dataset. The name may only contain letters,
  digits, `_`, `.` and `-` (1 to 100 characters). Only the name is sent. The catalogue entry in the triplestore
  keeps the old name. Keywords, description and licence of a dataset cannot be changed on the platform.
- `agw-platform update-model <model_id> --name …` changes the name and the tensor metadata of a model, for example
  `--input-shape '[1,3,64,64]'`, `--input-datatype float32`, `--input-semantics NCHW`, `--output-shape '[1,2]'`,
  `--output-labels 0 --output-labels 1`. Give only the fields to change. The tool reads the model first, merges
  the change and sends all fields, because the platform clears every field it does not get. The format stays as
  it is and cannot be changed here. The approval shows the whole body. The catalogue entry keeps the old name.
- Score in the name: if the model name shows `None Acc.`, the platform did not read the accuracy from the log.
  Read it with `agw-platform training-logs <train_container_id> --tail 50`, then put it into the name with
  `update-model`, for example `0.67 Acc.`.

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

## Inference containers

An inference image is built from a model and an installed template, in this order:

1. `agw-platform validate-model <model_id>` lists what keeps the model from becoming an inference container
   (`problems`) and what to look at (`warnings`). Fix the problems first: a name with spaces or brackets
   cannot be addressed by Triton, so rename it with `agw-platform update-model` (only letters, digits, `_ . -`).
2. `agw-platform templates` lists the installed templates. If the official one (branch `triton`) is missing,
   `agw-platform import-template triton-inference` imports it (approval needed).
3. `agw-platform build-image <template_id> <model_id> <name> <tag> amd64` builds the image into the registry
   (approval needed; several minutes, about 9 GB). The build refuses a tag that is in the registry already,
   because the platform would overwrite it without asking, and a build that is still running.
4. Follow the build with `agw-platform task <id>` (not in a tight loop, as with trainings), then check
   `agw-platform image-status <repository> <tag>` until `present` is `true`.

An image cannot be started without an edge device, and `/triton` checks the model on the Triton server,
not the image: do not use it to test an image. Templates cannot be changed or deleted; the agent imports
only the official one.

## Deploying an inference image to an edge device

`agw-platform capabilities` names the entry `edge`; it is available only while an edge device is
online. Without it the way ends at the built image — say that and invent no device. With it:

1. `agw-platform edge-devices` lists the devices with `arch` and `online`; pick an online device whose
   arch matches the image.
2. `agw-platform image-status <repository> <tag>` gives the image's `entries[].id`, the `image_id`.
3. Propose both writing steps as **one plan** — `create-deployment <device_id> <repository> <tag>
   <name> <host_port> --image_id <id>`, then `start-deployment` with `{{step1.id}}` — or run them one by
   one. Only images built in this chat or from your own namespace can be deployed; host ports only
   18100-18199; the name (without `agw-`) is free to choose.
4. The start asks **on its own** (compute, also inside a plan): the device pulls the image (several GB
   the first time) and runs the container with network access, its ports open on every interface of the
   device. Nothing runs before that.
5. Follow it with `agw-platform deployment-status <deployment_id> --task_id <id>`; `deployed`, or a
   `hint`. After a failed start do not start again — create a new deployment with another name and port
   and tell the user that the leftover container needs manual removal.

Edge devices, edge groups and applications are read-only for you.

## Taking a deployment back and cleaning up

`agw-platform undeploy <deployment_id>` removes the container and its anonymous volumes from the device; the
deployment stays and can be started again. It needs **no device online** (the platform sends the removal to
the device, and the task fails when the device is gone); `agw-platform capabilities` names the device state.
`agw-platform delete-deployment <deployment_id>` removes the deployment entry afterwards — it asks on its own
and cannot be undone. An image can only be deleted when no deployment points at it any more; **you delete no
images** (the user does). A deployment that has no container id means its start failed and a container of that
name stays on the device, which the platform cannot remove — tell the user.

## Everything else

`agw-platform request <METHOD> <path> [--query k=v …] [--body JSON|@file|-]` calls any path of the
REST API, only with a JSON body (no file uploads). Which paths exist is shown by
`agw-platform api-paths` (one line per operation; `agw-platform api-paths /train` only for training).
Paths under `/users`, `/urls`, `/service` and `/network` are blocked. Values of passwords, keys
and tokens appear as `[redacted]`, downloads only as size and type.

So far the platform only distinguishes logged in or not: lists show the objects of **all**
users. Do not delete or change anything the user has not explicitly asked for.
