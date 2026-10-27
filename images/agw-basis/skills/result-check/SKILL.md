---
name: result-check
description: Check long results before you call them done - after a training, before you upload a model from own code and after a deployment has started on an edge device. Call capabilities first (entry checks); without it say that the result is unchecked and report the platform's own figure as such. The gateway measures and judges; never compute, estimate or invent figures or a verdict yourself. Start every report with the header the gateway returns (Result usable, Result questionable or Result not measured), then the number of images or test requests, what was measured and the baseline. Use final=true once per dataset and chat and only when the model will not change any more; compare improvements with final=false. If questionable, follow the gateway's suggestions within the compute calls left (at most one new training or notebook run), otherwise propose them to the user. Never call a result done, correct or good without a final verdict usable.
---

# Checking a result before it counts

A training that ends with "finished" is not a result: it is a file. The gateway measures what a model
reaches on images the model never saw and answers with figures, a baseline and a verdict. You report
that verdict; you never compute, estimate or invent a figure yourself.

## Report format

Start your reply with the header the gateway returned, word for word (`Result: usable`,
`Result: questionable`, `Result: not measured – …`). Then, in this order:

1. the sample size (how many images or test requests the figures rest on),
2. what was measured (the set the gateway names, e.g. "statistics only, no image review"),
3. the baseline and what the model reaches next to it,
4. the figures per class (accuracy, per-class hit rate; the card paints the values the gateway named),
5. the reasons, hints and suggestions of the gateway.

Never describe the checked set itself: do not name images, file names, classes of single images or how
many images of a class are in it. The gateway holds that back on purpose, so that the measurement stays
a measurement. The sentence "statistics only, no image review" is part of the header; take it over
word for word when the gateway wrote it.

The check card shows the confusion matrix and the figures per class to the user. You do not get the matrix
(`assess-status` omits it) and you draw no chart about the checked set; learning curves of the training itself are
another matter (skill `charts`, section Evaluation charts).

## Budget

Checks do not count against the compute calls of a chat: training, notebook runs, image builds and
deployments do. `agw-platform capabilities` says how many compute calls are left and how many checks
this chat still has (entry `checks`); if one of them is not available, say so and do not retry a call
that answered `unavailable`.

An autonomous plan covers checks within its budget. Outside a plan, a suggestion of the gateway is a
suggestion to the user, not a permission to spend: propose it and wait.

## After a training

1. Propose the split before the training (the skill training-advice says so): `split-dataset` creates a
   training dataset from about 70 % of the annotated images per class and keeps the rest back. Train on
   that training dataset, never on the source. The answer carries `check_images`: the validation and
   held-out images the later check will find (it needs at least 5 and 20). When it also carries
   `check_warning`, tell the user in your next message, before the training starts, with the numbers it
   names; with `suggested_train_share`, ask whether to split again with that share. Do not split again or
   skip the training on your own: another split is another call that needs its own approval.
2. The gateway wakes you when the training is done; take the model over with `model-from-training` as
   usual.
3. Check it with `assess-model <model_id>` (without `--final`, so the validation images are measured).
   The answer is a `check_id`; follow it with `assess-status <check_id>` or wait for the wake-up. Start
   your report with the header the gateway returns.
4. Interim result usable: measure once more with `assess-model <model_id> --final true` if the model will
   not change any more. `final=true` works once per dataset and chat; use it for the final report.
5. Result questionable: follow the suggestions the gateway names (for example `more_epochs`). At most one
   retraining: train once more with exactly that change, take the new model over, check it with
   `--final false` and report before and after against the previous check.
6. Result not measured: report it as it stands. The reason names what is missing - "no held-out images"
   (train on a split training dataset next time), "too few validation images" or "too few held-out images" (more annotated images; the split said so in `check_warning`),
   "origin unknown" (the gateway did not see this model being created in this chat).
7. The platform's own figure ("platform figure") is the number the training template printed on its own
   internal test split; quote it as the platform's figure and never as the result of your check.

## Own models

A model from your own code is checked like a training result: **before** it goes to the platform, not after.
The gateway measures the file you fetched out of the notebook server, so what it judges is exactly what
`upload-model` sends.

1. Write and test the code in the sandbox on a small sample (the skill `notebook` says how).
2. Train it in the notebook server with the **training** dataset of `split-dataset` as `dataset_ids` — never
   with the dataset you will check on. Every dataset a run of this chat received counts as seen afterwards;
   if that is the whole source, no held-out image is left and the check answers `not measured – no held-out
   images`.
3. Fetch the result: `notebook-fetch <run_id> model.onnx`. It lands under `/workspace/notebook/<run_id>/`.
   Only a file that came out of a run of this chat can be checked — the gateway knows its SHA-256; a file it
   did not hand over is `not measured – origin unknown`.
4. Check it with `assess-model --path /workspace/notebook/<run_id>/model.onnx` (without `--final`) and follow
   the check with `assess-status`. Add `--preprocess` when your model does not expect what the platform
   templates feed (rgb, the image size of the model, 1/255, NCHW, no mean/std), and `--class_names` when you
   know which output means which class. The gateway loads the file, reads its input and output shapes and
   measures it on images no run of this chat received; it derives the class order itself and only compares it
   with `class_names` — a declared order that differs from the one the model shows is a reason of its own.
   A run that wrote `outputs/model.json` needs no `--preprocess`. On a final check a `--preprocess` that differs from it is refused before anything runs; the final test stays available. `Not measured – class order unclear` on a
   final check: the hold-out set was not used; fix the preprocessing and run the final check again.
5. Interim result usable: measure once with `--final true` when the model will not change any more, and
   report as under *After a training*.
6. Result questionable: the reasons name what is off (for example `the model returns the same output for
   different images`). Change the code, start **one** new run, fetch the new file, check it with
   `--final false` and report before and after.
7. Only then upload the file with `upload-model`. Its approval names the check of the file
   (`Model check of this file: usable (…)`); the check stays the check of the model that was uploaded.

## Edge deployments

After `agw-platform deployment-status <deployment_id>` shows `deployed`, have the gateway test the
container on the device: `agw-platform deployment-check <deployment_id> <model_id>`. It sends six fixed
test requests to the device and compares the answers with what the model file computes for the same
inputs. It only asks the device when the entry `edge` of `agw-platform capabilities` names it under
"test requests"; a real device is out of reach for the gateway.

Report the answer like every other check:

1. `Result: usable (synthetic test requests)` means the device serves exactly the model you named.
2. `Result: questionable (…)` names the difference: the deployment serves another model
   (`the deployment serves <name>, not <other>`), the answers differ from the model file, the device
   answers the same to every request, or the interface of the device, of the platform's model record and
   of the model file do not agree. The gateway names a suggestion (`undeploy`, `rebuild_image`); follow it
   within the compute calls that are left or propose it to the user.
3. `Result: not measured – …` means nothing was sent: the device is no simulated device the gateway may
   address, the container does not answer yet (`check again in about a minute`, suggestion
   `wait_and_recheck`), or the input type of the model cannot be tested this way. Say that instead of
   claiming the deployment works.
4. Never write that a deployment "works" without a `usable` verdict, and never claim that the model is
   good because the device serves it: that is what the model check is for (`assess-model`); the report
   names it as "model check" when this chat has one.
