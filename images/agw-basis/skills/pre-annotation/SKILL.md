---
name: pre-annotation
description: Pre-annotate platform datasets with CVAT functions via agw-platform annotation-*. Rules - check capabilities first (entry annotation, and vision for image review); if pre-annotation is unavailable say so and point to manual annotation in CVAT; only the user's own image datasets with classes; first annotation-functions, then annotation-task; propose a mapping from function labels to dataset classes by meaning and show it as a table before pre-annotate, never map a label you are unsure about; existing=keep only on a task without annotations, existing=replace deletes all annotations and needs a clear yes; follow a run with annotation-run in a background loop at least 30 s apart and do not wait in the foreground; afterwards annotation-summary, then offer annotation-sync; never claim the pre-annotation is correct, it is a proposal the user checks in CVAT (give the cvat_url).
---

# Pre-annotation with CVAT

A pre-annotation writes machine proposals into a CVAT task of one of the user's image datasets. The user
checks them in CVAT and corrects them; the models of the platform then train on what came back
(`annotation-sync`). It is never a finished annotation.

## 0. What this setup can do

- `agw-platform capabilities` names the entry `annotation`. Without it, say so and point at the
  **Annotate** button of the dataset, where the user works in CVAT by hand.
- The entry `vision` only matters for looking at images yourself; the pre-annotation itself runs in CVAT
  and needs no model of the chat.
- No usable function (`annotation-functions` is empty) means there is nothing to run. Say that plainly
  instead of proposing a run.

## 1. Which functions exist

`agw-platform annotation-functions` lists the functions you may run: your own (from `annotation-function`,
irreversible) and those the operator installed. Each entry has `id`, `name`, `kind`, `labels`, `source`
and `state`. Only `kind: detector` runs over all images of a task on its own. A function of another user
is never usable — it receives every image of the run.

## 2. Prepare the dataset

`agw-platform annotation-task --dataset-id <id>` creates the CVAT task of the dataset (or reuses the one
recorded on it) and writes its id onto the dataset. It works only for the user's own datasets of type
`AgriImageDataResource` with classes (`annotation_labels`). The answer has `cvat_url`: give it to the
user, so they can look at the task.

## 3. Propose the mapping

`annotation-functions` names the labels of the function, `agw-platform dataset <id>` the classes of the
dataset. They rarely match. Read both lists and propose a mapping by meaning, for example
`class_0 -> 0`, `class_1 -> 1` for an MNIST model, or `cow -> Rind`. Show it as a table with the function
label, the dataset class and one sentence of reasoning, and ask for the user's consent — a wrong mapping
puts every image into the wrong class, and the user notices that faster than you.

**Never map a label you are unsure about.** Leave it unmapped and say which ones you left out; CVAT drops
unmapped labels, so the run then writes fewer ideas instead of wrong ones.

## 4. Start the run

`agw-platform pre-annotate --dataset-id <id> --function <id> --mapping '{"class_0":"0","class_1":"1"}'`
starts the run over all images of the task. `--min-confidence` is the threshold in percent (default 50).
`--existing keep` (the default) adds to the task and is refused when the task already has annotations;
`--existing replace` deletes **all** annotations of the task first and is irreversible.

Effect class `compute`: it needs the user's approval, and a chat has a limited number of compute calls.
The answer carries `run_id`.

## 5. Follow the run

Do not wait in the foreground: a long run ends after the chat has gone idle, and the agent would sit in a
tool call for minutes. Start a background task with `run_in_background: true` and at least 30 s between
the queries:

```bash
while :; do
  out=$(agw-platform annotation-run <run_id>)
  echo "$out"
  grep -q '"state":"\(finished\|failed\|expired\)"' <<< "$out" && break
  sleep 30
done
```

The states are `queued`, `started`, `finished`, `failed` and `expired`. `expired` means CVAT no longer
knows the run; what it already wrote stays in the task, so `annotation-summary` still counts it.

Runtime on this instance (4 vCPU, no GPU): roughly **0.1 to 1 s per image**, so an estimate of a minute
for a few hundred images and an hour for a few thousand. Say that it is an estimate. Large images and
detection models take longer.

## 6. Count, then write back

`agw-platform annotation-summary --dataset-id <id>` counts the annotations of the task: per class manual
and automatic, and the images with and without annotations. Report those numbers; they are the honest
result of the run.

`agw-platform annotation-sync --dataset-id <id>` writes the annotations from CVAT back into the dataset
(`annotations.xml` in the object store). Starting a training does the same. Effect class `write`, so the
user approves; offer it rather than running it silently.

## 7. What you must not claim

The pre-annotation is a **proposal**, not a result. Say which function ran, with which threshold and
mapping, how many annotations and how many images were touched, and that the user checks and corrects
them in CVAT (`cvat_url`). Never call the annotations correct, never report a metric you did not measure,
and never train on them without the user's explicit consent.
