---
name: platform
description: Work with the Agri-Gaia platform (datasets, models, training, background tasks, edge devices, container images) via agw-platform. Use when the user asks about data or models on the platform or wants to create, start or follow a training there, and to deploy inference images to edge devices when capabilities shows an edge device online, otherwise build the image and tell the user that no device is available. For a task with several writing calls (e.g. split, train, take over the model) propose one plan with all known steps.
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
   loop; wait at least 20 seconds between queries). When the task is completed, its message names the
   training container ("Train Container #<id> created."); `agw-platform trainings` shows it too.
5. Start: `agw-platform start-training <train_container_id>`, follow with
   `agw-platform training-status <id>` and `agw-platform training-logs <id> --tail 50`.

## Taking over a trained model

Once a training is `exited`, `agw-platform model-from-training <train_container_id>` creates a platform model
from it (ONNX when the training was created with an export). It needs approval. **Every call creates another
model**, so do it once per training and only when the user wants the model. Without `export_config` in
`create-training` the model has no tensor metadata and no inference container can be built from it; the example
is in `agw-platform create-training --help`.

## One plan for a whole task

When the user wants a whole task done (train and take over the model, build and deploy an image), propose **one**
plan with every step you already know, not one plan per step. The user approves it once. Example: split dataset 59,
train on the training part, take over the model.

```bash
agw-platform propose-plan '{"title": "tf-flowers: split, train EfficientNet, take over the model", "steps": [
  {"tool": "split_dataset", "args": {"dataset_id": 59, "train_share": 70},
   "description": "Split tf-flowers 70/30"},
  {"tool": "create_training", "args": {"provider": "Torchvision", "architecture": "EfficientNet",
   "category": "Classification", "dataset_id": "{{step1.id}}",
   "train_config": {"architecture": "efficientnet_v2_s", "epochs": 2, "batch-size": 16, "img-size": 64,
     "optimizer": "AdamW", "learning-rate": 0.001, "lr-scheduler": "ReduceLROnPlateau",
     "train-split": 0.8, "test-split": 0.2},
   "export_config": {"model_type": "pytorch", "input_names": [], "output_names": [],
     "input_shapes": [[1, 3, 64, 64]], "input_types": ["float32"], "verbose": false, "dynamo": false,
     "opset_version": 18}},
   "description": "Build the training container"},
  {"tool": "start_training", "args": {"train_container_id": "{{step2.id}}"},
   "description": "Train"},
  {"tool": "model_from_training", "args": {"train_container_id": "{{step2.id}}"},
   "description": "Take over the model"}]}'
```

- `{{step1.id}}` is the training dataset the split created; `{{step2.id}}` is the **training container** the build
  creates — not the task id of the build. A placeholder never points at a start (`start_training`,
  `start_deployment`): use the step that created the object.
- Steps 2 and 3 start platform jobs. The gateway observes them; the next step runs only after the job ended.
  Calling it earlier is refused with the reason. End your turn while a job runs; the gateway's note at its end
  names the object, e.g. "Created training container 63; {{step2.id}} is 63". Then make the next call with that id.
- If a job fails, the plan ends; the note says why. Propose a new plan for what is left.
- Take the `train_config` keys and values from `agw-platform train-options <provider> <architecture>`; the values
  above are only an example.
- A plan step may upload many files: the approval shows the number, size, folders and one SHA-256 over the list,
  pinned when the plan is proposed. Do not change, add or remove those files before the step runs; otherwise the
  step is refused (files_changed) and you have to propose a new plan.

## Uploading files

You upload files from the sandbox with two commands; the orchestrator reads them itself from the
sandbox, and before approving the user sees the number, size, folders and file types of the files and
one SHA-256 over the list of all files (up to 10 files: name, size and SHA-256 of each):

```bash
# Dataset: name, description, then the files (globs work); classes as a repeated option,
# a CVAT annotation (annotations.xml) as --annotation-file
agw-platform upload-dataset piglet-images "Piglets, barn 3, October" images/*.png \
  --annotation-labels 0 --annotation-labels 1 --annotation-file annotations.xml
# Model: name, description, format (onnx, pytorch, tensorflow, tensorrt), file
agw-platform upload-model mnist-small "MNIST, two classes" onnx model.onnx
```

At most 2 000 files and 512 MB per call, each file at most as large as an artifact. Every call creates a new
dataset, so upload a dataset in one call. File names must be unique across folders (`daisy/001.jpg` and
`rose/001.jpg` collide): the platform keeps only the file name.
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

<!-- experiment:rights -->
## Delegated rights

The user can delegate only certain rights to you for a chat (which actions on which
objects). `agw-platform rights` shows them, together with the objects you created in this chat.
Check before writing calls. Anything outside them is refused by the authorization service
(exit code 4, "denied by the authorization service"); do not try another way then,
but tell the user which right is missing.
<!-- /experiment:rights -->

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
6. Once it shows `deployed`, have the gateway test the container on the device:
   `agw-platform deployment-check <deployment_id> <model_id>` — the skill `result-check` says how to
   report it. This reaches simulated devices only (the entry `edge` names them under "test requests").

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
