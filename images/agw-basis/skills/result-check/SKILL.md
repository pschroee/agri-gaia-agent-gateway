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

Filled by R1.

## Own models

Filled by R2.

## Edge deployments

Filled by R3.
