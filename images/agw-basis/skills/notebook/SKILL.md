---
name: notebook
description: Own Python code - use it when the training templates do not fit or the user asks for own code. First check capabilities - with notebook available run it in the chat's notebook server (2 CPUs, 8 GB, a shared GPU where the instance has one, no internet, no platform access) with agw-platform notebook-run; without it say so and offer the sandbox or a template. Rules - write the run as a Jupyter notebook (.ipynb via nbformat), a markdown cell per step and one step per code cell, print and plot results, the executed notebook keeps all outputs in JupyterHub; .py only for helper modules; test the code in the sandbox first with agw-run-notebook; a notebook takes no args, it reads params.json; datasets via dataset_ids; the user approves every run; torch.cuda.is_available() with CPU fallback; follow with notebook-run-status and notebook-log (a failed run names its cell); notebook-fetch, then upload-model as ONNX for an inference container; give the user the link of the run, it opens the executed notebook.
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

- small models with `numpy`/`pandas` in the sandbox (no `torch` there, and it cannot be installed: package files over 256 MB are refused), or
- a platform training template (`train-options`, `create-training`) if the task fits one.

Never promise a notebook run you cannot start, and never retry a call that answered `unavailable`.

## 2. Write the run as a notebook

Write the run as a **Jupyter notebook** (`.ipynb`): the user can then follow every step with its outputs, tables and
charts in JupyterHub. Use `.py` only for helper modules that the notebook imports.

```python
import nbformat
from nbformat.v4 import new_notebook, new_markdown_cell, new_code_cell

nb = new_notebook(cells=[
    new_markdown_cell("## Settings\nRead from params.json when it exists."),
    new_code_cell("import json, os\nparams = {'epochs': 5, 'lr': 0.001}\n"
                  "if os.path.exists('params.json'):\n    params.update(json.load(open('params.json')))\nprint(params)"),
    new_markdown_cell("## Train"),
    new_code_cell("..."),
])
nbformat.write(nb, "train.ipynb")
```

- One step per code cell, and a markdown cell above each step that says what it does.
- Settings in a `params` dict in the first code cell, read from `params.json` when that file exists. A notebook
  takes **no** `--args`.
- Print results and plot with matplotlib and `plt.show()` - the outputs are saved in the notebook.
- Only the source of the code cells runs. Tags and other cell metadata, stored outputs and the kernel name in the
  file are replaced by the starter. The user approves the code cells; changing markdown does not change the code.

Then test it in the sandbox on a **small sample** (a handful of images, a few rows), so that syntax errors and
shape mistakes show up before the user is asked:

```bash
agw-run-notebook train.ipynb      # writes executed-train.ipynb, prints the outputs of every cell
```

It is the starter the notebook server uses, so a notebook that runs here runs there the same way (the sandbox has
no torch and no GPU, so test with numpy/pandas or a tiny stand-in). Only files with the endings
`.py .ipynb .txt .json .yaml .yml .csv .md .cfg .toml` can go into a run, at most 50 files, 1 MB each and 4 MB
together: code and configuration, not data.

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
agw-platform notebook-run /workspace/train.ipynb --entry train.ipynb \
  --dataset_ids 17 --timeout_minutes 240
```

- `--entry` is the file name of the script or notebook that starts the run; it must be one of the files.
- A `.py` entry runs with `python -u <entry> <args>`. A `.ipynb` entry runs cell by cell with `agw-run-notebook`
  (the same as in the sandbox) and is saved with all outputs as `executed-<entry>` in the run directory after every
  cell, and when it ends as `notebooks/<name>_<date>.ipynb`. A notebook takes no `--args`.
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

`notebook-log` shows the outputs of every cell; a failed run names the cell (`failed_cell`, `cell_error`). The link
opens the executed notebook.

One run at a time per chat. `notebook_status` can report the state `stopping` (the server is shutting down); a run then waits up to 60 s for the stop to finish and starts on a fresh server. Write the results into the run directory (the working directory of the run):
`model.onnx`, `metrics.json`, `predictions.csv`. The link in the answer opens the executed notebook (for a `.py`
entry the run directory) for the user in JupyterLab; give it to them.

Inside a training search (skill `training-search`) the notebook reads its parameters from `params.json` in the run
directory and writes one JSON line per epoch to `outputs/metrics.jsonl` (`epoch`, `epochs`, `train_loss`, `val_loss`,
`train_acc`, `val_acc`); the search skill has the snippet.

A script that writes a model should also write `outputs/model.json` with exactly what it does to an image:

```python
json.dump({"class_names": classes, "preprocess": {"color": "gray", "size": [IMG, IMG], "scale": 1/255}},
          open("outputs/model.json", "w"))
```

Write exactly what the script does to an image; no mean or std unless the script subtracts them. The gateway
reads the file when you fetch the `.onnx` file of that run and the check uses it.

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
