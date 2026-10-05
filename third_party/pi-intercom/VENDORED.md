# pi-intercom: our own copy in the PoC

This directory is a **modified copy** of the npm package `pi-intercom`. It lives in the repo and,
when building the pi image, replaces the files that `pi install npm:pi-intercom@0.15.0` creates
(the dependency `tsx` still comes from npm). Changes happen only here.

## Origin

| | |
|---|---|
| Package | `pi-intercom` on npm (<https://www.npmjs.com/package/pi-intercom>) |
| Version | **0.15.0** |
| Integrity (`npm pack --json`) | `sha512-Dy2BZdkqbOqk10Ac4J6u0E3bYOxXOoNqE0wJl/7aIMVz+wOtyS3BbG5iDsjHOuP0wlfqDILu+03/MSbg0Ik5WQ==` |
| shasum | `7a38cabfec28ecd622dca7615ca2a4191cb9c40e` |
| Taken over on | 2026-09-30 |
| Content | the folder `package/` from `npm pack pi-intercom@0.15.0` (37 files) |

**What is foreign and what is our own is shown by the Git history** (of the master's thesis
repository, see the note at the end): commit `4ce9ca6` contains the package unchanged
(byte-for-byte identical to the tarball), the following commit only our own changes:

```bash
git diff 4ce9ca6 HEAD -- poc/third_party/pi-intercom
```

## License

MIT, copyright with the authors of pi-intercom; the license text is unchanged in
[`LICENSE`](LICENSE). The local changes are under the same license.

## Changes

1. **`index.ts`, `deliverIncomingBrokerMessage`: messages to busy sessions without a UI are
   injected instead of refused.** In the original, a session without a UI (`hasUI` false) that is
   currently working automatically answers the sender "This agent is running in
   non-interactive mode and cannot respond …" and discards the message. Subagents of
   pi-subagents are such sessions and almost always busy; direct messages between
   running subagents would therefore never arrive (observed on the system on 2026-09-30:
   `delivered: true` at the sender, no message in the recipient's history). Now the same path
   applies to them as to sessions with a UI: injection via `steer`, i.e. after the running tools and
   before the next model call. With `PI_INTERCOM_REFUSE_WHEN_BUSY=1` the original's behaviour
   applies again. Covered by `TestSlotSubagentIntercom` (`internal/worker/talk_docker_test.go`).

## Note on the separate repository (2026-10-05)

Since 2026-10-05 the gateway lives in its own repository. The commits named above are in the
master's thesis repository (`poc/third_party/…`); here the copy came in with the first commit
**already modified**. Our own changes can still be verified at any time: fetch `npm pack` with the
version named above, unpack it and compare with `diff -r package/ <this directory>`.
