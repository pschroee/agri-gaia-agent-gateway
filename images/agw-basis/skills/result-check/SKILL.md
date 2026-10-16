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
   that training dataset, never on the source.
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
   (train on a split training dataset next time), "too few held-out images" (more annotated images),
   "origin unknown" (the gateway did not see this model being created in this chat).
7. The platform's own figure ("platform figure") is the number the training template printed on its own
   internal test split; quote it as the platform's figure and never as the result of your check.

## Own models

Filled by R2.

## Edge deployments

Filled by R3.
