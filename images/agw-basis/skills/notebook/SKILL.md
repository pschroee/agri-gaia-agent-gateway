---
name: notebook
description: Own Python code on the platform - use it when the platform's training templates do not fit (other architectures, tabular data) or the user asks for own code. First check capabilities - with notebook available run the code in the chat's notebook server in JupyterHub (2 CPUs, 8 GB, a shared GPU where the instance has one, no internet, no platform access, torch with CUDA, onnx) with agw-platform notebook-run; without it say so and either train small models in the sandbox or use a training template. Rules - test the code in the sandbox first on a small sample; pass platform datasets with dataset_ids, they appear under /home/jovyan/work/datasets/<id>/; the user approves every run and sees all code, so keep it short; pick the device with torch.cuda.is_available() and keep a CPU fallback, export ONNX from the CPU; follow the run with notebook-run-status and notebook-log; fetch results with notebook-fetch and upload a model with upload-model, as ONNX for an inference container; give the user the link of the run.
---

# Own code in the notebook server

The platform's training templates cover classification, detection and segmentation with the installed
architectures. For anything else - another architecture, tabular data, combining two models - write Python
yourself and let it run in this chat's notebook server in JupyterHub.

The server belongs to the user (their JupyterHub account, their files). It has 2 CPUs, 8 GB RAM, **no
internet** and **no access to the platform**: packages are fixed in the image (`torch` (CUDA 12.6, runs on the CPU without a GPU), `torchvision`,
`scikit-learn`, `numpy`, `pandas`, `matplotlib`, `onnx`, `onnxruntime`, `skl2onnx`), and an `pip install`
fails. Data comes in as a platform dataset, results go out as files.

## 1. First ask what this setup offers

```bash
agw-platform capabilities
```

The entry `notebook` says whether a notebook server is available. Without it (not configured, JupyterHub not
reachable, or the user never opened JupyterHub), say so plainly and take one of the two other ways:

- small models with `numpy`/`pandas` in the sandbox (no `torch` there), or
- a platform training template (`train-options`, `create-training`) if the task fits one.

Never promise a notebook run you cannot start, and never retry a call that answered `unavailable`.

## 2. Write and test the code in the sandbox first

Create the script (or notebook) under `/workspace`, then run it there on a **small sample** - a handful of
images, a few rows - so that syntax errors and shape mistakes show up before the user is asked:

```bash
python -c "import ast,sys; ast.parse(open('train.py').read())"     # quick syntax check
python train.py --epochs 1 --limit 8                                # small sample
```

Only files with the endings `.py .ipynb .txt .json .yaml .yml .csv .md .cfg .toml` can go into a run, at
most 50 files, 1 MB each and 4 MB together: code and configuration, not data.

## GPU or CPU

`notebook_status` says whether this instance gives the server a GPU (`"gpu": true`). Write the code so that it runs
either way - the sandbox has no GPU and no torch, so the GPU path is first tried in the run itself:

    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    print("device:", device, torch.cuda.get_device_name(0) if device.type == "cuda" else "")
    model.to(device)            # and every batch: x, y = x.to(device), y.to(device)

The GPU is shared with platform trainings and other chats: keep batches moderate, catch
`torch.cuda.OutOfMemoryError` and fall back to a smaller batch or the CPU. `notebook-run-status` names the GPU the run
saw (`"gpu"`); tell the user which device trained the model. Before the ONNX export move the model to the CPU:
`model.cpu().eval()` and a CPU sample.

## 3. Start the run

```bash
agw-platform notebook-run /workspace/train.py --entry train.py \
  --dataset_ids 17 --args --epochs --args 30 --timeout_minutes 240
```

- `--entry` is the file name of the script or notebook that starts the run; it must be one of the files.
- A `.py` entry runs with `python -u <entry> <args>`, a `.ipynb` entry is executed with `jupyter nbconvert`
  and saved next to itself as `executed-<entry>`.
- `--dataset_ids` (at most 3) unpacks each dataset to `/home/jovyan/work/datasets/<id>/` before the run;
  the unpacking is reused by later runs. Reading a dataset costs the delegation `update dataset` on it.
- `--timeout_minutes` defaults to 120 and is capped by this instance.

The user approves **every** run and sees the whole code; a plan never covers it. Keep the code short and
readable - a run that is too long for one approval is refused.

## 4. Follow the run

```bash
agw-platform notebook-run-status <run_id>    # state, exit code, duration, files
agw-platform notebook-log <run_id> --tail 100
```

One run at a time per chat. Write the results into the run directory (the working directory of the run):
`model.onnx`, `metrics.json`, `predictions.csv`. The link in the answer opens the run directory for the user
in JupyterLab; give it to them.

`exit_code` 124 means the run hit the time limit: nothing after that point is in the output. Look at the log,
make the code faster or ask for a longer limit with a new run.

## 5. Bring results back and upload a model

```bash
agw-platform notebook-fetch <run_id> model.onnx     # lands in /workspace/notebook/<run_id>/
agw-platform assess-model --path /workspace/notebook/<run_id>/model.onnx   # check it, then follow assess-status
agw-platform upload-model --name mnist_cnn --description "..." --format onnx \
  --model_file /workspace/notebook/<run_id>/model.onnx
```

Before `upload-model`, check the file with `assess-model` (skill `result-check`); never pass the dataset you
will check on to a notebook run. The upload is the usual writing call: the user approves it and sees the
SHA-256 of the file and what the check of this file said. For an inference
container the model must be ONNX **with tensor metadata** - export it that way in the script:

```python
torch.onnx.export(model, sample, "model.onnx", input_names=["input"], output_names=["output"],
                  dynamo=False, opset_version=18)
```

`input_shapes` must match what the container gets: for an image model of 64 x 64 px, `[1, 3, 64, 64]`.
`platform_validate_model` checks the model before an inference image is built from it.
