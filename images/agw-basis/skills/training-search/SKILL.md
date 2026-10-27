---
name: training-search
description: Training search - use it when a task needs several training rounds (tuning hyperparameters, trying architectures or augmentation) - propose ONE search with propose_search instead of single trainings. Rules - split the dataset first (split_dataset) so that a held-back test set exists; keep the search space small and say why; before every trial read search_status and never repeat a trial; declare each trial with search_next and a one-sentence reason, then make exactly the calls it names; in the notebook way the code is one notebook (.ipynb) that reads params.json and writes outputs/metrics.jsonl (one JSON line per epoch with epoch, epochs, train_loss, val_loss, train_acc, val_acc), and only params.json changes between trials; choose by the validation value only; when the search ends run the final test once for the winner.
---

# Training search with a budget

A task like "find a good learning rate" needs several trainings. Do not propose each one on its own: propose
**one search**. The user approves the search space and the budget once; inside them you run trial after trial
without asking again. Anything outside the approved space is refused.

## 1. Prepare

```bash
agw-platform capabilities
```

- **Split first.** `agw-platform split-dataset` gives a training part and a held-back test part. The search must
  never see the test part: a dataset that holds held-back images is refused, and the winner is chosen on the
  validation value only (`val_acc`, `val_loss` or `val_f1`), never on the training value.
- **Count the compute calls.** Every trial uses one (a copy plus its start counts as one trial, a notebook run as
  one). `capabilities` and the refusal of `propose_search` name what this chat has left.
- **Keep the space small:** two to four parameters, bounds from the template's schema (`train-options`), a
  `log` scale for learning rates. Say in the `note` why these parameters and these bounds.

## 2. Propose the search (one approval)

Two ways, same tool.

**Template way** - the base is a built training container of this chat (`create-training`, wait for the build,
then `trainings` shows its id). Each trial is a copy of it with other `train_config` values (`clone-training`).

```bash
agw-platform propose-search --title "tf-flowers: learning rate and batch size" --path template \
  --base_container_id 41 \
  --space '{"learning-rate":{"type":"float","min":0.0001,"max":0.01,"scale":"log"},
            "batch-size":{"type":"choice","values":[16,32]}}' \
  --budget '{"max_runs":6,"max_compute_minutes":90,"target":0.9,"patience":3}' \
  --objective '{"metric":"val_acc"}' \
  --note "Learning rate has the largest effect on a short run; the batch size is the cheap second axis."
```

**Notebook way** - your own code, see skill `notebook`. The files are fixed for the whole search; a trial may only
change `params.json`, any change of the code asks the user again.

```bash
agw-platform propose-search --title "own CNN: width and dropout" --path notebook \
  --files /workspace/train.ipynb --entry train.ipynb --dataset_ids 17 --timeout_minutes 30 \
  --space '{"width":{"type":"choice","values":[16,32,64]},"dropout":{"type":"float","min":0,"max":0.5,"scale":"linear"}}' \
  --budget '{"max_runs":5}'
```

`budget` fields are all optional; the first one reached ends the search (`max_runs`, `max_compute_minutes`,
`target` = validation value that is good enough, `patience` = trials in a row without improvement). Without
`budget` the compute limit of the chat applies.

## 3. Run the trials

Repeat until the search ends:

1. `agw-platform search-status <search_id>` - budget left, every trial with parameters and validation value.
   Never repeat a trial and never walk back to a worse region without a reason.
2. `agw-platform search-next <search_id> --params '{...}' --note "one sentence why"` - `params` names **every**
   parameter of the space, each inside its bounds. The answer names the calls to make.
3. Make exactly those calls, nothing else (template way: `clone-training` and `start-training`; notebook way:
   `notebook-run` with the fixed files). They need no approval.
4. Follow the run (`training-status`, `notebook-run-status`) and go back to 1. A trial counts as finished when
   its job ended; the validation value comes from the platform, not from your reading of the log.

`agw-platform end-search <search_id> --note "why"` ends a search early. The user can also stop it, change the
budget or send a hint; read `search-status` again after a pause.

## 4. When the search ended

The answer of `search-status` names the reason (budget, target, patience, you, the user) and the winner. Then:

1. Run the final test **once**, for the winner only, on the held-back part (`assess-model` with `final=true`, skill
   `result-check`). Before the end, the gateway refuses it.
2. Report the table of trials (parameters, validation value, state), the winner, the reason the search ended and
   the figure of the final test. If the final figure is far below the validation value, say so.
3. The model takeover of the winner (`model-from-training`) and its final test are covered by the search: they need no
   approval of their own, once, for the winner's container only.
4. If the user's task goes on after the final test (rename the model, inference image, deployment, model card),
   propose the remaining steps as **one** plan with `propose-plan` instead of asking call by call.

## 5. A search inside a plan

When the whole route from the dataset to the deployment is known, put the search into the plan: **one** approval
covers the split, the training container, the search, the takeover and test of the winner, the image and the deployment.

- The step is `propose_search` with the arguments of section 2, with these restrictions: `path` is `template`;
  `base_container_id` is `"{{stepN.id}}"` of a `create_training` step (or the id of a container of this chat); both
  `budget.max_runs` and `budget.max_compute_minutes` are required, because the approval shows compute calls and
  duration. One search per plan, none in an experiment chat.
- Call `propose_search` with exactly the approved arguments when the step is next. It then needs no card: the plan's
  approval covers it. A changed space, another base or other limits are not the step and ask the user on their own.
- The step is done when the search ends. `{{stepN.id}}` of the search step is then the **training container of the
  winner**: use it in `create_model_from_training`. The trials run as in section 3 and cost compute calls of the chat
  (counted when the plan is proposed).
- The plan stays valid for the 30 minutes plus the compute minutes of the search.

Example (MNIST, three trials):

1. `split_dataset` with `dataset_id` of the uploaded dataset
2. `create_training` with `dataset_id "{{step1.id}}"`
3. `propose_search` with `path "template"`, `base_container_id "{{step2.id}}"`, a small `space`, `budget`
   `{"max_runs": 3, "max_compute_minutes": 30}`
4. `create_model_from_training` with `train_container_id "{{step3.id}}"`
5. `assess_model` with `model_id "{{step4.id}}"`, `final=true`, `eval_dataset_id` of the held-back set
6. `update_model` with `model_id "{{step4.id}}"` and only `name`
7. `build_inference_image` with `model_id "{{step4.id}}"`
8. `create_deployment` with `image_id "{{step7.id}}"`
9. `start_deployment` with `deployment_id "{{step8.id}}"`

## Convention for the notebook way

The entry notebook reads its parameters from `params.json` in the run directory and writes **one JSON line per
epoch** to `outputs/metrics.jsonl`. The gateway reads the file while the run goes on (progress, stopping a
hopeless trial) and after it. No line, no figure: a trial without `val_acc` or `val_loss` cannot win.

```python
import json, os

params = json.load(open("params.json"))          # the values search-next declared for this trial
os.makedirs("outputs", exist_ok=True)

with open("outputs/metrics.jsonl", "a") as log:
    for epoch in range(1, EPOCHS + 1):
        train_loss, train_acc = train_one_epoch(model, params["dropout"])
        val_loss, val_acc = evaluate(model)       # on the validation part only
        log.write(json.dumps({"epoch": epoch, "epochs": EPOCHS, "train_loss": train_loss,
                              "val_loss": val_loss, "train_acc": train_acc, "val_acc": val_acc}) + "\n")
        log.flush()                              # the gateway reads it during the run
```

Keys are exactly these six; numbers must be finite. Read `params.json` for **every** searched value - a value
hard-coded in the script makes the trial meaningless.

Write next to the model how your script feeds an image to it, so the check feeds it the same way (the gateway reads
the file when you fetch the `.onnx` file of this run):

```python
json.dump({"class_names": classes, "preprocess": {"color": "gray", "size": [IMG, IMG], "scale": 1/255}},
          open("outputs/model.json", "w"))
```

Write exactly what the script does to an image; no mean or std unless the script subtracts them.

Changing markdown cells does not change the code; changing a code cell asks the user again.
