---
name: model-card
description: Write a model card for a platform model as MODEL_CARD.md and send it to the user with agw-artifact upload. Rules - use only facts read in this chat with agw-platform model, training, training-logs and dataset (get_model, get_training, training_logs, get_dataset); give quality with the metric name, the value and what it refers to (test split, number of test images), overall and per class when the log has it; state the limits explicitly (data volume, class balance, camera position and setting, few epochs on CPU); write not known for anything these sources do not say instead of estimating; every number in the card must appear in those sources. Use when the user asks for a model card, model documentation or a datasheet for a model. In replies name files by their file name, never by sandbox path.
---

# Model cards

A model card describes a platform model in four sections: purpose, data, quality, limits. It contains only
what the platform and the training log say; everything else is "not known".

## 1. Collect the facts

- `agw-platform model <id>`: name, format, owner, last_modified, tensor metadata (input and output name,
  datatype, shape, semantics, labels).
- The model does not name its training. Find the training container with `agw-platform trainings`: same
  provider and architecture as in the model's name and the `dataset_id` of the dataset named there. If none or
  more than one fits, ask the user or write "not known".
- `agw-platform training <id>` (provider, architecture, category, dataset_id, score) and
  `agw-platform training-logs <id> --tail 400` (the train config at the start, the test metrics at the end).
- `agw-platform dataset <dataset_id>` (name, owner, classes, filecount).
- Never take numbers from memory, from other chats or from typical values of the architecture.

## 2. Quality

Take the metrics from the test table at the end of the log (PyTorch Lightning prints for example
`MulticlassAccuracy | 0.67` in a box table). Give the metric name as printed, the value as printed and what it
refers to: the test split (`test-split` of the train config) and the number of test images if the log names it.
Per class only if the log prints values per class. The platform's score field is usually empty (`None Acc.` in
the model name); do not use it.

## 3. Write and send

Write `/workspace/MODEL_CARD.md` with this template, then send it with `agw-artifact upload MODEL_CARD.md`.
Every statement names its source in brackets: (model), (training), (log), (dataset), (user).

```markdown
# Model card: <model name>

| | |
|---|---|
| Model id | … |
| Format | … |
| Owner | … |
| Last modified | … |
| Training container | … |
| Provider / architecture / category | … |
| Input | name, datatype, shape, semantics |
| Output | name, datatype, shape, labels |

## Purpose
What the model does (category, classes) and what it is for, as far as the user said it; otherwise not known.

## Data
Dataset (id, name, owner), number of files, classes, split from the train config. Class balance only with
counts from the sources; otherwise not known.

## Quality
| Metric | Value | Refers to | Source |
|---|---|---|---|

## Limits
- Data volume: …
- Class balance: …
- Camera position, lighting, setting: not known unless the user or the dataset says it.
- Training: epochs, img-size, batch-size, CPU without GPU.
- Further limits the sources show (for example no ONNX export, missing output labels).
```

## 4. Reply

One short paragraph: the file name and which points are "not known". Do not paste the card again.
