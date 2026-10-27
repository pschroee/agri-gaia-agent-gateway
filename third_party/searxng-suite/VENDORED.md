# searxng-suite: unchanged copy

This directory is an **unchanged copy** of the npm package `@blazer2k/searxng-suite`. It lives in the repo and is copied
into the pi image at `/opt/agw/pihome/npm/node_modules/@blazer2k/searxng-suite`; the image build no longer fetches it
from npm (its dependencies come from a lockfile, see `images/agw-basis/pi-packages`).

## Origin

| | |
|---|---|
| Package | `@blazer2k/searxng-suite` on npm (<https://www.npmjs.com/package/@blazer2k/searxng-suite>) |
| Version | **0.2.3** |
| Integrity (`npm pack --json`) | `sha512-E0U3TaTEJWIUjWG/ID16NKCMiLydwxIfmFNPQOArTDBIIqEkTIJ99tki7JG0A9GVanOwLEjC7zFT/ru6+y7KJg==` |
| shasum | `7fbae1aed63044702e6eac424f43d2d541cef110` |
| Taken over on | 2026-10-27 |
| Content | the folder `package/` from `npm pack @blazer2k/searxng-suite@0.2.3` (25 files) |
| Repository | `package.json` names `https://github.com/blazer2k/pi-blz` (directory `packages/searxng-suite`); gateway issue #231 refers to the same code as `pi-extensions` |

## Why vendored

The package has one maintainer, about 23 downloads a week and no dependents, so its author can unpublish it at any
time. That already happened: the predecessor `pi-searxng-suite` was unpublished on 2026-10-25 and broke the image
build. A lockfile protects against changes, not against removal; a copy in the repo does both.

## License

MIT, copyright Fedor Vasilev; the license text is unchanged in [`LICENSE`](LICENSE).

## Changes

None. Verify at any time: fetch `npm pack @blazer2k/searxng-suite@0.2.3`, unpack it and compare with
`diff -r package/ <this directory>` (only this `VENDORED.md` differs).
