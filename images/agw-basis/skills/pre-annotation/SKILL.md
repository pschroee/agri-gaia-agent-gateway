---
name: pre-annotation
description: Pre-annotate the user's own image datasets via agw-platform annotation-*. Check capabilities first (entries annotation and vision) and take the path it allows; if pre-annotation is unavailable, point to manual annotation in CVAT. Show a label mapping by meaning as a table before any run. Own ONNX models become functions with annotation-function (irreversible, visible to all CVAT users). Calibrate before filling, fill only images without annotations, follow long jobs in a background loop 30 s apart. After every run or fill run annotation-check verify and report first the verdict usable or questionable, whether images were reviewed or statistics only, per-class figures and sample sizes; never call it done without a final verdict usable. If questionable you may correct it with the gateway's suggestions (rollback, threshold, mapping, fill), at most 3 rounds and only while the validation figures improve; report before and after each round.
---

# Pre-annotation with CVAT

A pre-annotation writes machine proposals into a CVAT task of one of the user's image datasets. The user
checks them in CVAT and corrects them; the models of the platform then train on what came back
(`annotation-sync`). It is never a finished annotation.

## 0. What this setup can do

- `agw-platform capabilities` names the entry `annotation`. Without it, say so and point at the
  **Annotate** button of the dataset, where the user works in CVAT by hand.
- The entry `vision` only matters for looking at images yourself; the pre-annotation itself runs in CVAT
  and needs no model of the chat. Whether images were looked at is part of every verdict (`image_review`
  in the report of §5b), and without that second view the gateway judges by stricter thresholds.
- No usable function (`annotation-functions` is empty) means there is nothing to run. Say that plainly
  instead of proposing a run.

## 1. Which functions exist

`agw-platform annotation-functions` lists the functions you may run: your own (from `annotation-function`,
irreversible) and those the operator installed. Each entry has `id`, `name`, `kind`, `labels`, `source`
and `state`. Only `kind: detector` runs over all images of a task on its own. A function of another user
is never usable — it receives every image of the run.

## 1a. Your own model as a function

A model the user trained on the platform (or uploaded) becomes a function as well:

```bash
agw-platform annotation-function <model_id> classification --labels class_0 --labels class_1
agw-platform annotation-function <model_id> detection --labels pig --labels cow --input-size 640
```

Only an **ONNX** model of the user's own works; a PyTorch file is refused (it is code, not data). The
gateway packs the fixed template of the gateway — you write no code — with the model, its classes and
its input size, and the platform builds the function from it. `--labels` are the class names **in the
order the model outputs them** (repeat the option); without them the gateway takes the `output_labels`
of the model, and for a classification model the names `class_0 … class_{n-1}`. A detector without
class names is refused — its output carries numbers only, and the mapping would have nothing to hold
on.

**This call is `irreversible`**, so it always needs its own approval (a plan does not cover it): the
function is visible to and usable by **every CVAT user of the platform**, it receives every image of a
run over which it is started, and you cannot remove it — only the operator can. At most three
functions per user; at that bound, ask the user to have one removed instead of trying again.

The build takes **several minutes**. `agw-platform annotation-functions` shows the state:
`deploying` until CVAT lists it, `ready` (then it can be run), `failed` when the build failed, and
`missing` when the function has disappeared from CVAT. Follow the build in a background loop like a
run, or simply ask again later. Run a function only in state `ready`.

**The order of the classes is the crux**: a wrong order puts every image into the wrong class. If you
do not know the order the model was trained with, say so and have the user confirm the mapping before
the run; never guess.

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

Measured on the CPU (4 vCPU, 2026-09): roughly **0.1 to 1 s per image**, so an estimate of a minute
for a few hundred images and an hour for a few thousand. Say that it is an estimate. Large images and
detection models take longer.

A **job** of the check and the fill (below) is followed the same way, with `annotation-job <job_id>`. A
job runs in the gateway and keeps running while the chat is idle; it cannot be stopped. Its answer
carries `done` and `total` images.

## 5a. Check before you trust

A function that has never seen this dataset is a guess. Measure it first, fill only what is empty, and
show the user where the answers are weak — that is the loop, not the run over all images.

**1. Measure (calibrate).** Only with manually annotated images of the task (`source != "auto"`); without
any, say so and ask for some by hand (a handful per class is a start, `annotation-task` creates the task
they are annotated in).

```bash
agw-platform annotation-check --dataset-id <id> --function <id> --mode calibrate --mapping '{"class_0":"0"}'
```

The answer carries `job_id`; follow it in a background loop as in §5. The finished result has `images`
(how many were measured), `mapping` (what each class of the function answered), `confusion`, `accuracy`,
`coverage`, `proposed_min_confidence` and a `note`. Show `mapping`, the hit rate and the threshold as a
table, **name the sample size**, and have the user confirm the mapping before any run: the confusion
table is the evidence, not your guess.

**2. Decide.** Below 80 percent accuracy, or with a coverage well under the threshold you would use, do
**not** run the function over all images: recommend more manual annotations instead, and say how many
more. That is the honest answer, and it is what the calibration is for.

**3. Fill what is empty.**

```bash
agw-platform annotation-fill --dataset-id <id> --function <id> --min-confidence <proposed> --mapping '{"class_0":"0"}'
```

`annotation-fill` asks the function about the images of the task that carry **no** annotation at all and
writes what it answers into exactly those images (`source: "auto"`). Images with an annotation stay
untouched, also when the user annotates one while the job runs; nothing is deleted. The result names
`filled`, `skipped_annotated` and `empty_results` — report all three.

**4. Name the weak images.**

```bash
agw-platform annotation-check --dataset-id <id> --function <id> --mode uncertain --uncertain-below 60
```

The result lists the images whose answer stays under the threshold, the weakest first, each with its file
and a `cvat_url` that opens exactly that image. Give the user that list (the links, not the raw JSON) and
what to look at.

**5. Close the loop.** After the user corrected the images in CVAT: `annotation-summary`, then
`annotation-sync`, then a new training on the corrected images (skill `training-advice`), a new function
from the new model (`annotation-function`) and calibrate again — the hit rate should have gone up. If the
task takes several of these steps, propose them **together** as one plan (`propose-plan`) and say how many
compute calls it needs.

## 5b. Report the result

A run or a fill is **not** a result. The gateway judges it, and you report that judgement:

```bash
agw-platform annotation-check --mode verify --source-id <run_id or job_id> --final true
```

- `--source-id` is the `run_id` of `pre-annotate` or the `job_id` of `annotation-fill`. The gateway
  measures **that** function, with **that** mapping and **that** threshold, on the images the user
  annotated himself — you do not name function, mapping or threshold here, and you never measure
  against a better setting than the one that produced the result.
- `--final true` measures the **held-out images** of the task and gives the verdict. It works **once
  per task and chat**: a second call is refused, and every correction round afterwards has to use
  the validation images (`--final false`) until new manual annotations or a new chat.
- `--final false` (the default) measures the validation images and gives an interim figure. Use it
  while you are still correcting.

Follow the job as in §5 (background loop, `annotation-job <job_id>`). The finished result is the
report:

| Field | What it says |
|---|---|
| `header` | the one line your reply starts with, e.g. `Result: usable (held-out images, statistics only, no image review)` |
| `verdict` | `usable`, `questionable` or `not_measured` |
| `final`, `set` | held-out (final) or validation (interim) |
| `measured_images` | how many images the figures rest on; `calibration_images` the size of the calibration set |
| `image_review` | whether a model looked at the pictures (`done`), with which `model`, how many it `checked` and the `flagged_share` it complained about; `reason` says why none looked |
| `per_class` | precision, recall, F1 and support per class; `below` names the figures under their limit |
| `overall` | accuracy (classification) or macro F1 (detector) |
| `plausibility` | the automatic annotations against the manual ones: class shares, `tvd`, empty share, boxes per image, box area |
| `reasons` | why the verdict is not `usable` |
| `hints` | what stands close to a bound (they change nothing) |
| `suggestions` | what to do next: more manual images, another threshold, another mapping, a rollback, a retraining |
| `suspicious` | up to 50 images worth a look, each with `reason` and `cvat_url` |

Then:

1. **Begin your reply with the header line, word for word.** Is the verdict `questionable` or
   `not_measured`, say so first and do **not** call the pre-annotation done, correct or finished.
2. Name the figures per class as a table, and name the size of the set they rest on. A hit rate of
   0.95 over eight images is not the same as one over eighty.
3. Name the reasons if there are any, the hints if any, and the suggestions as the steps that follow.
4. Give the suspicious images as links (`cvat_url`), not as a JSON dump: the user opens them and
   corrects them.

**With and without an image review.** `capabilities` answers under `vision` whether the current model
reads images. With it the gateway also looks at a sample of the filled images and at the images its
own rules found suspicious: it renders them with the automatic annotation drawn on them and lets your
model judge them in a call of its own — never in your context, and never for a dataset the operator
did not allow to a cloud model (`image_review.reason` says so). That second view can only make the
verdict worse: more than a fifth of the checked images flagged means `questionable`, and from ten
checked images on the milder thresholds apply (held-out images from 10 instead of 20, a figure from
0.80 instead of 0.85). Without vision the verdict rests on the statistics alone and needs more
held-out images. **Always say which of the two was used** — the header line names it and
`image_review.done` carries it, with `checked` and `flagged_share` as the numbers behind it.

**Never call a pre-annotation done without a final verdict `usable` from `--final true`.** Without
it the honest sentence is: the function ran, the figures are so and so, the gateway judges the
result questionable — or the images have not been checked at all.

**Taking the automatic annotations back.** If the verdict is questionable because the mapping or the
threshold was wrong, the automatic annotations can be taken back — exactly what the run or the fill
wrote, and only what still stands unchanged; every object the user changed or drew himself stays:

```bash
agw-platform annotation-rollback --source-id <run_id or job_id>                 # everything it wrote
agw-platform annotation-rollback --source-id <run_id or job_id> --scope suspicious  # only the images the verification named
```

`--scope suspicious` goes by the last verification of that source, so that one has to run first. The
answer names `deleted`, `changed_kept` (corrected by the user, left alone), `missing` (no longer in
CVAT) and `frames_emptied`. After a rollback, fill again with the corrected setting — what was taken
back comes again from the same function.

## 5c. Correct and stop

A questionable interim verdict is not the end: you correct the result yourself, and the gateway counts
how often. Every step is one of the tools above with its own effect class — there is no tool of its own
for the loop, and no step skips the check the others go through.

**What a round is, and when it is refused.** Every `pre-annotate` or `annotation-fill` on a task after
the first one in this chat is a **correction round**. At most **3** of them per task and chat, at most
**1** with a retraining. A round also has to be earned: it goes on only while the round before it made
the figure on the validation images (`overall.accuracy` of a classification, `overall.macro_f1` of a
detector) at least **0.02** better, and it is refused while the task has fewer than 5 validation images
and the user did not annotate any more. The gateway decides that and answers with the reason
(`no improvement on the validation images in round …`, `too few validation images …`, `at most 3
correction rounds …`, `no correction round after the final verdict …`). **Report that sentence and
stop** — do not try another setting after it, do not call the result done, and never ask for a second
`--final true`.

**Order of the corrections** — the cheap ones first, and only the suggestions the report of §5b named:

1. **Threshold or mapping.** `other_threshold` and `remap` come from the calibration set. Take the
   automatic annotations back (`annotation-rollback --source-id <id> --scope suspicious`, or `--scope
   all` when the mapping was wrong as a whole) and fill again with the confirmed mapping and the
   proposed `--min-confidence`. Both scopes are `write` and the fill is `compute`: one `propose-plan`
   covers the round, and the images the user annotated himself stay untouched either way.
2. **The uncertain images to the user.** `annotation-check --mode uncertain` lists the images the
   function is least sure about, the weakest first, each with a `cvat_url`. Give the user those links
   and end your turn there: this is a waiting step, and it is the only way the validation images grow.
   Come back when the user says they annotated.
3. **Retraining.** Only when the setting is right and the model is the weak part. Have the user confirm
   the class order, train on the corrected images (skill `training-advice`), take the model over, build
   a function from it (`annotation-function` — `irreversible`, so a plan never covers it and the user
   approves exactly this call), calibrate again and fill.

**Report each round with both columns.** The interim report of a correction carries `round`, `previous`
(the figures of the round before) and `delta` (the change of the figure and per class). Name them:

| Round | Measured on | Figure | Before | Change |
|---|---|---|---|---|
| 2 | 5 validation images | accuracy 0.80 | 0.60 | +0.20 |

Say plainly whether the correction helped — a round that changed nothing is a result as well. Then
`--final true` on the held-out images, once, and report that verdict as the end of the loop.

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
them in CVAT (`cvat_url`). Never call the annotations correct, never report a metric you did not measure
— a calibration over ten images is a tendency, not a property of the model — and never train on them
without the user's explicit consent.
